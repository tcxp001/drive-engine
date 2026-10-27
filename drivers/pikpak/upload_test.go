package pikpak

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
)

type pikPakCountingFile struct {
	*bytes.Reader
	reads atomic.Int32
}

func (f *pikPakCountingFile) ReadAt(p []byte, offset int64) (int, error) {
	f.reads.Add(1)
	return f.Reader.ReadAt(p, offset)
}

func TestPikPakMultipartCancellationStopsRetry(t *testing.T) {
	for _, beforeChunks := range []bool{true, false} {
		name := "after_first_failure"
		if beforeChunks {
			name = "before_chunks"
		}
		t.Run(name, func(t *testing.T) {
			if !isolatedPikPakTest(t) {
				return
			}
			originalConfig := conf.Conf
			t.Cleanup(func() { conf.Conf = originalConfig })
			conf.Conf = &conf.Config{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var initiations, uploads, completions, progress atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/xml")
				if req.URL.Path != "/test-bucket/fixture.bin" {
					t.Errorf("unexpected OSS path: %s", req.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				switch {
				case req.Method == http.MethodPost && req.URL.Query().Has("uploads"):
					initiations.Add(1)
					_, _ = io.WriteString(w, `<InitiateMultipartUploadResult><Bucket>test-bucket</Bucket><Key>fixture.bin</Key><UploadId>test-upload</UploadId></InitiateMultipartUploadResult>`)
				case req.Method == http.MethodPut && req.URL.Query().Get("uploadId") == "test-upload":
					uploads.Add(1)
					_, _ = io.Copy(io.Discard, req.Body)
					cancel()
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = io.WriteString(w, `<Error><Code>InternalError</Code><Message>controlled failure</Message></Error>`)
				case req.Method == http.MethodPost && req.URL.Query().Get("uploadId") == "test-upload":
					completions.Add(1)
					_, _ = io.WriteString(w, `<CompleteMultipartUploadResult/>`)
				default:
					t.Errorf("unexpected OSS request: %s %s", req.Method, req.URL.RequestURI())
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			t.Cleanup(server.Close)
			payload := bytes.Repeat([]byte("x"), 1024)
			file := &pikPakCountingFile{Reader: bytes.NewReader(payload)}
			s := &stream.FileStream{
				Ctx: ctx, Reader: file,
				Obj: &model.Object{Name: "fixture.bin", Size: int64(len(payload))},
			}
			if beforeChunks {
				cancel()
			}
			err := (&PikPak{}).UploadByMultipart(ctx, &S3Params{
				Endpoint: server.URL, Bucket: "test-bucket", Key: "fixture.bin",
				AccessKeyID: "test-access-key", AccessKeySecret: "test-secret", SecurityToken: "test-security-token",
			}, int64(len(payload)), s, func(float64) { progress.Add(1) })
			if !errors.Is(err, context.Canceled) {
				t.Errorf("upload error = %v, want context.Canceled", err)
			}
			wantAttempts := int32(1)
			if beforeChunks {
				wantAttempts = 0
			}
			if reads := file.reads.Load(); reads != wantAttempts {
				t.Errorf("chunk reads = %d, want %d; canceled upload must not start another attempt", reads, wantAttempts)
			}
			if got := uploads.Load(); got != wantAttempts {
				t.Errorf("OSS part uploads = %d, want %d", got, wantAttempts)
			}
			if initiations.Load() != 1 || completions.Load() != 0 || progress.Load() != 0 {
				t.Errorf("initiate/complete/progress = %d/%d/%d, want 1/0/0", initiations.Load(), completions.Load(), progress.Load())
			}
		})
	}
}

func TestPikPakMultipartKeepsThreeAttempts(t *testing.T) {
	for _, succeeds := range []bool{true, false} {
		name := "exhausted"
		if succeeds {
			name = "success_on_third"
		}
		t.Run(name, func(t *testing.T) {
			if !isolatedPikPakTest(t) {
				return
			}
			originalConfig := conf.Conf
			t.Cleanup(func() { conf.Conf = originalConfig })
			conf.Conf = &conf.Config{}
			payload := bytes.Repeat([]byte("x"), 1024)
			var initiations, uploads, completions, progress atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/xml")
				switch {
				case req.Method == http.MethodPost && req.URL.Query().Has("uploads"):
					initiations.Add(1)
					_, _ = io.WriteString(w, `<InitiateMultipartUploadResult><Bucket>test-bucket</Bucket><Key>fixture.bin</Key><UploadId>test-upload</UploadId></InitiateMultipartUploadResult>`)
				case req.Method == http.MethodPut && req.URL.Query().Get("uploadId") == "test-upload":
					attempt := uploads.Add(1)
					body, err := io.ReadAll(req.Body)
					if err != nil || !bytes.Equal(body, payload) || req.URL.Query().Get("partNumber") != "1" {
						t.Error("multipart retry changed the part number or payload")
					}
					if attempt < 3 || !succeeds {
						w.WriteHeader(http.StatusInternalServerError)
						_, _ = io.WriteString(w, `<Error><Code>InternalError</Code><Message>controlled failure</Message></Error>`)
						return
					}
					w.Header().Set("ETag", "part-etag")
				case req.Method == http.MethodPost && req.URL.Query().Get("uploadId") == "test-upload":
					completions.Add(1)
					var completion struct {
						Parts []struct {
							Number int    `xml:"PartNumber"`
							ETag   string `xml:"ETag"`
						} `xml:"Part"`
					}
					if err := xml.NewDecoder(req.Body).Decode(&completion); err != nil {
						t.Errorf("decode multipart completion: %v", err)
					} else if len(completion.Parts) != 1 || completion.Parts[0].Number != 1 || completion.Parts[0].ETag != "part-etag" {
						t.Errorf("completed parts = %+v", completion.Parts)
					}
					_, _ = io.WriteString(w, `<CompleteMultipartUploadResult/>`)
				default:
					t.Errorf("unexpected OSS request: %s %s", req.Method, req.URL.RequestURI())
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			t.Cleanup(server.Close)
			file := &pikPakCountingFile{Reader: bytes.NewReader(payload)}
			ctx := context.Background()
			s := &stream.FileStream{
				Ctx: ctx, Reader: file,
				Obj: &model.Object{Name: "fixture.bin", Size: int64(len(payload))},
			}
			err := (&PikPak{}).UploadByMultipart(ctx, &S3Params{
				Endpoint: server.URL, Bucket: "test-bucket", Key: "fixture.bin",
				AccessKeyID: "test-access-key", AccessKeySecret: "test-secret", SecurityToken: "test-security-token",
			}, int64(len(payload)), s, func(value float64) {
				progress.Add(1)
				if value != 100 {
					t.Errorf("completed upload progress = %v, want 100", value)
				}
			})
			if (err == nil) != succeeds {
				t.Errorf("upload error = %v, succeeds = %v", err, succeeds)
			}
			if file.reads.Load() != 3 || uploads.Load() != 3 || initiations.Load() != 1 {
				t.Errorf("reads/uploads/initiate = %d/%d/%d, want 3/3/1", file.reads.Load(), uploads.Load(), initiations.Load())
			}
			wantCompleted := int32(0)
			if succeeds {
				wantCompleted = 1
			}
			if completions.Load() != wantCompleted || progress.Load() != wantCompleted {
				t.Errorf("complete/progress = %d/%d, want %d/%d", completions.Load(), progress.Load(), wantCompleted, wantCompleted)
			}
		})
	}
}
