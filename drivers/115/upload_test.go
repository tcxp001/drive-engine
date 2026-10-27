package _115

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	driver115 "github.com/power721/115driver/pkg/driver"
)

type pan115UploadTransport func(*http.Request) (*http.Response, error)

func (f pan115UploadTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type pan115CountingFile struct {
	*bytes.Reader
	reads atomic.Int32
}

func (f *pan115CountingFile) ReadAt(p []byte, offset int64) (int, error) {
	f.reads.Add(1)
	return f.Reader.ReadAt(p, offset)
}

func TestPan115MultipartCancellationStopsRetry(t *testing.T) {
	for _, before := range []bool{true, false} {
		name := "after_first_failure"
		if before {
			name = "before_chunks"
		}
		t.Run(name, func(t *testing.T) {
			runPan115MultipartTest(t, before, !before, false)
		})
	}
}

func TestPan115MultipartKeepsThreeAttempts(t *testing.T) {
	for _, succeeds := range []bool{true, false} {
		name := "exhausted"
		if succeeds {
			name = "success_on_third"
		}
		t.Run(name, func(t *testing.T) {
			runPan115MultipartTest(t, false, false, succeeds)
		})
	}
}

func runPan115MultipartTest(t *testing.T, cancelBefore, cancelOnFailure, succeeds bool) {
	t.Helper()
	// Keep the existing uploader's worker lifetime confined to a child process.
	const childEnv = "MV_PAN115_MULTIPART_TEST"
	if os.Getenv(childEnv) != t.Name() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		names := strings.Split(t.Name(), "/")
		for i, name := range names {
			names[i] = "^" + regexp.QuoteMeta(name) + "$"
		}
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run="+strings.Join(names, "/"), "-test.v", "-test.timeout=10s")
		cmd.Env = []string{childEnv + "=" + t.Name()}
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated multipart test failed: %v\n%s", err, output)
		}
		t.Logf("%s", output)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	payload := bytes.Repeat([]byte("x"), 1024)
	var tokenCalls, initiations, uploads, completions, progress atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if r.Host != "test-bucket."+driver115.OSSEndpoint || r.URL.Path != "/fixture.bin" {
			t.Errorf("unexpected local proxy request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Query().Has("uploads"):
			initiations.Add(1)
			_, _ = io.WriteString(w, `<InitiateMultipartUploadResult><Bucket>test-bucket</Bucket><Key>fixture.bin</Key><UploadId>test-upload</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == http.MethodPut && r.URL.Query().Get("uploadId") == "test-upload":
			attempt := uploads.Add(1)
			body, err := io.ReadAll(r.Body)
			if !cancelBefore && (err != nil || !bytes.Equal(body, payload) || r.URL.Query().Get("partNumber") != "1") {
				t.Error("multipart retry changed the part number or payload")
			}
			if cancelOnFailure {
				cancel()
			}
			if attempt < 3 || !succeeds {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `<Error><Code>InternalError</Code><Message>controlled failure</Message></Error>`)
				return
			}
			w.Header().Set("ETag", "part-etag")
		case r.Method == http.MethodPost && r.URL.Query().Get("uploadId") == "test-upload":
			completions.Add(1)
			var completion struct {
				Parts []struct {
					Number int    `xml:"PartNumber"`
					ETag   string `xml:"ETag"`
				} `xml:"Part"`
			}
			if err := xml.NewDecoder(r.Body).Decode(&completion); err != nil {
				t.Errorf("decode multipart completion: %v", err)
			} else if len(completion.Parts) != 1 || completion.Parts[0].Number != 1 || completion.Parts[0].ETag != "part-etag" {
				t.Errorf("completed parts = %+v", completion.Parts)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"state":true,"data":{"file_id":"fixture-file","file_name":"fixture.bin"}}`)
		default:
			t.Errorf("unexpected OSS request: %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(server.Close)
	// The SDK endpoint is fixed; the existing proxy option routes only to this server.
	conf.Conf = &conf.Config{ProxyAddress: server.URL}
	d := &Pan115{client: driver115.New()}
	d.client.Client.SetTransport(pan115UploadTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != driver115.ApiUploadOSSToken || r.Method != http.MethodGet {
			return nil, fmt.Errorf("unexpected token request: %s %s", r.Method, r.URL.Path)
		}
		tokenCalls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"AccessKeyID":"test-key","AccessKeySecret":"test-secret","SecurityToken":"test-token","StatusCode":"200"}`)),
			Request:    r,
		}, nil
	}))
	file := &pan115CountingFile{Reader: bytes.NewReader(payload)}
	s := &stream.FileStream{Ctx: ctx, Reader: file, Obj: &model.Object{Name: "fixture.bin", Size: int64(len(payload))}}
	params := &driver115.UploadOSSParams{Bucket: "test-bucket", Object: "fixture.bin"}
	params.Callback.Callback = "{}"
	if cancelBefore {
		cancel()
	}
	result, err := d.UploadByMultipart(ctx, params, int64(len(payload)), s, "0", func(value float64) {
		progress.Add(1)
		if value != 100 {
			t.Errorf("completed upload progress = %v, want 100", value)
		}
	}, driver115.UploadMultipartWithTimeout(5*time.Second))
	wantAttempts, wantCompleted := int32(3), int32(0)
	if cancelBefore || cancelOnFailure {
		wantAttempts = 1
		if cancelBefore {
			wantAttempts = 0
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("upload error = %v, want context.Canceled", err)
		}
	} else if succeeds {
		wantCompleted = 1
		if err != nil || result == nil || result.Data.FileID != "fixture-file" {
			t.Errorf("successful upload: result=%+v error=%v", result, err)
		}
	} else if err == nil || !strings.Contains(err.Error(), "controlled failure") {
		t.Errorf("exhausted upload error = %v", err)
	}
	if file.reads.Load() != wantAttempts || uploads.Load() != wantAttempts {
		t.Errorf("chunk reads/uploads = %d/%d, want %d/%d", file.reads.Load(), uploads.Load(), wantAttempts, wantAttempts)
	}
	if tokenCalls.Load() != 1 || initiations.Load() != 1 || completions.Load() != wantCompleted || progress.Load() != wantCompleted {
		t.Errorf("token/initiate/complete/progress = %d/%d/%d/%d, want 1/1/%d/%d", tokenCalls.Load(), initiations.Load(), completions.Load(), progress.Load(), wantCompleted, wantCompleted)
	}
}
