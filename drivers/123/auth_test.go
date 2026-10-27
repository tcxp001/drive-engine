package _123

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/glebarez/sqlite"
	"github.com/go-resty/resty/v2"
	"gorm.io/gorm"
)

type pan123AuthTransport func(*http.Request) (*http.Response, error)

func (f pan123AuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestPan123LoginPreservesErrorMessage(t *testing.T) {
	previousClient := base.RestyClient
	t.Cleanup(func() { base.RestyClient = previousClient })
	for _, message := range []string{"login refused", "quota 100% exhausted", "literal %s %d %%"} {
		t.Run(message, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"code": 403, "message": message})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			base.RestyClient = resty.New().SetTransport(pan123AuthTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != "/api/user/sign_in" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       io.NopCloser(strings.NewReader(string(body))),
					Request:    r,
				}, nil
			}))
			d := &Pan123{Addition: Addition{
				Username: "synthetic-user", Password: "synthetic-password", AccessToken: "old-token",
			}}
			if err := d.login(); err == nil || err.Error() != message {
				t.Errorf("login error = %v, want literal %q", err, message)
			}
			if calls != 1 || d.AccessToken != "old-token" {
				t.Error("failed login changed its request count or existing token")
			}
		})
	}
}

func TestPan123RequestReloginPersistence(t *testing.T) {
	// db.Init cannot restore a nil handle; isolate this storage fixture.
	const childEnv = "MV_PAN123_AUTH_TEST"
	if os.Getenv(childEnv) != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPan123RequestReloginPersistence$", "-test.v", "-test.timeout=10s")
		cmd.Env = []string{childEnv + "=1"}
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated authentication test failed: %v\n%s", err, output)
		}
		t.Logf("%s", output)
		return
	}
	previousConfig, previousClient := conf.Conf, base.RestyClient
	previousDB := db.GetDb()
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	conf.Conf = conf.DefaultConfig(t.TempDir())
	db.Init(database)
	t.Cleanup(func() {
		base.RestyClient, conf.Conf = previousClient, previousConfig
		if previousDB != nil {
			db.Init(previousDB)
		}
		_ = sqlDB.Close()
	})

	for _, tc := range []struct {
		name       string
		qr         bool
		loginFails bool
		retryFails bool
		valid      bool
	}{
		{name: "password success"},
		{name: "QR success", qr: true},
		{name: "password failure", loginFails: true},
		{name: "retry still unauthorized", retryFails: true},
		{name: "valid token", valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &Pan123{
				Storage: model.Storage{MountPath: "/test-123/" + tc.name, Driver: "123Pan"},
				Addition: Addition{
					Username: "synthetic-user", Password: "synthetic-password",
					AccessToken: "old-token", UseQrCodeLogin: tc.qr, UniID: "test-qr-id",
				},
				params: Params{UserAgent: "preserved-agent", Platform: "preserved-platform"},
			}
			initial, err := json.Marshal(d.Addition)
			if err != nil {
				t.Fatal(err)
			}
			d.Storage.Addition = string(initial)
			if err := db.CreateStorage(&d.Storage); err != nil {
				t.Fatal(err)
			}
			storedToken := func() string {
				t.Helper()
				stored, err := db.GetStorageById(d.ID)
				if err != nil {
					t.Fatal(err)
				}
				var addition Addition
				if err := json.Unmarshal([]byte(stored.Addition), &addition); err != nil {
					t.Fatal(err)
				}
				return addition.AccessToken
			}
			apiCalls, loginCalls := 0, 0
			base.RestyClient = resty.New().SetTransport(pan123AuthTransport(func(r *http.Request) (*http.Response, error) {
				body := `{"code":200}`
				switch r.URL.Path {
				case "/api/user/info":
					apiCalls++
					wantToken := "old-token"
					if apiCalls > 1 {
						wantToken = "new-token"
						if storedToken() != wantToken {
							t.Error("new credential was not persisted before retry")
						}
					}
					if r.Header.Get("Authorization") != "Bearer "+wantToken {
						t.Error("request did not use the expected token")
					}
					if r.Header.Get("User-Agent") != "preserved-agent" || r.Header.Get("Platform") != "preserved-platform" {
						t.Error("request changed existing account headers")
					}
					if !tc.valid && (apiCalls == 1 || tc.retryFails) {
						body = `{"code":401,"message":"expired"}`
					}
				case "/api/user/sign_in":
					loginCalls++
					if tc.qr || r.Method != http.MethodPost {
						t.Error("unexpected password login")
					}
					body = `{"code":200,"data":{"token":"new-token"}}`
					if tc.loginFails {
						body = `{"code":403,"message":"login refused"}`
					}
				case "/api/user/qr-code/result":
					loginCalls++
					if !tc.qr {
						t.Error("unexpected QR login")
					}
					body = `{"code":200,"data":{"loginStatus":1,"token":"new-token"}}`
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					body = `{"code":403,"message":"unexpected request"}`
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       io.NopCloser(strings.NewReader(body)),
					Request:    r,
				}, nil
			}))
			_, err = d.Request(UserInfo, http.MethodGet, nil, nil)
			if (err != nil) != (tc.loginFails || tc.retryFails) {
				t.Fatalf("unexpected request result: %v", err)
			}
			wantCalls, wantLogins, wantToken := 2, 1, "new-token"
			if tc.loginFails || tc.valid {
				wantCalls, wantToken = 1, "old-token"
			}
			if tc.valid {
				wantLogins = 0
			}
			if apiCalls != wantCalls || loginCalls != wantLogins || storedToken() != wantToken {
				t.Fatalf("unexpected relogin/persistence: API calls=%d login calls=%d", apiCalls, loginCalls)
			}
		})
	}
}
