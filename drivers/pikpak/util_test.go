package pikpak

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
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

// db.Init cannot restore a nil database. Keep its state (and uploader workers)
// in a disposable test process, without changing the parent test's globals.
func isolatedPikPakTest(t *testing.T) bool {
	t.Helper()
	const childEnv = "MV_PIKPAK_ISOLATED_TEST"
	if os.Getenv(childEnv) == t.Name() {
		return true
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	names := strings.Split(t.Name(), "/")
	for i, name := range names {
		names[i] = "^" + regexp.QuoteMeta(name) + "$"
	}
	cmd := exec.CommandContext(ctx, executable, "-test.run="+strings.Join(names, "/"), "-test.v", "-test.timeout=10s")
	cmd.Env = []string{childEnv + "=" + t.Name()}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated test failed: %v\n%s", err, output)
	}
	t.Logf("%s", output)
	return false
}

func newPikPakAuthDriver(t *testing.T) *PikPak {
	t.Helper()
	if os.Getenv("MV_PIKPAK_ISOLATED_TEST") != t.Name() {
		t.Fatal("storage fixture requires an isolated test process")
	}
	originalConfig := conf.Conf
	t.Cleanup(func() { conf.Conf = originalConfig })
	conf.Conf = &conf.Config{Database: conf.Database{Type: "sqlite3"}}
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := g.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})
	db.Init(g)
	d := &PikPak{
		Storage: model.Storage{Driver: "PikPak", MountPath: "/pikpak-test", Status: "old-status"},
		Addition: Addition{
			Username:     "tester@example.invalid",
			Password:     "test-password",
			RefreshToken: "old-refresh",
		},
		Common: &Common{
			ClientID:      "test-client",
			ClientSecret:  "test-secret",
			ClientVersion: "test-version",
			PackageName:   "test-package",
			DeviceID:      "test-device",
			UserAgent:     "test-agent",
			Algorithms:    []string{"test-algorithm"},
			CaptchaToken:  "old-captcha",
			UserID:        "old-user",
		},
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
	}
	addition, err := json.Marshal(d.Addition)
	if err != nil {
		t.Fatal(err)
	}
	d.Storage.Addition = string(addition)
	if err := db.CreateStorage(&d.Storage); err != nil {
		t.Fatal(err)
	}
	return d
}

func savedPikPakStorage(t *testing.T, d *PikPak) (model.Storage, Addition) {
	t.Helper()
	stored, err := db.GetStorageById(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	var addition Addition
	if err := json.Unmarshal([]byte(stored.Addition), &addition); err != nil {
		t.Fatal(err)
	}
	return *stored, addition
}

type pikPakHTTPReply struct {
	method string
	path   string
	status int
	body   string
	check  func(*http.Request)
}

func stubPikPakHTTP(t *testing.T, replies ...pikPakHTTPReply) {
	t.Helper()
	originalClient := base.RestyClient
	next := 0
	t.Cleanup(func() {
		base.RestyClient = originalClient
		if next != len(replies) {
			t.Errorf("HTTP requests = %d, want %d", next, len(replies))
		}
	})
	base.RestyClient = resty.New().SetTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		reply := pikPakHTTPReply{status: http.StatusBadRequest, body: `{"error_code":4126,"error":"unexpected_request"}`}
		if next < len(replies) {
			expected := replies[next]
			if req.Method != expected.method || req.URL.Path != expected.path {
				t.Errorf("request = %s %s, want %s %s", req.Method, req.URL.Path, expected.method, expected.path)
			} else {
				reply = expected
			}
		} else {
			t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
		}
		next++
		if reply.check != nil {
			reply.check(req)
		}
		if reply.status == 0 {
			reply.status = http.StatusOK
		}
		return &http.Response{
			StatusCode: reply.status,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(reply.body)),
			Request:    req,
		}, nil
	}))
}

func pikPakRequestBody(t *testing.T, req *http.Request) map[string]interface{} {
	t.Helper()
	var body map[string]interface{}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return body
}

func TestPikPakLoginRefreshesCaptchaAndPersists(t *testing.T) {
	if !isolatedPikPakTest(t) {
		return
	}
	d := newPikPakAuthDriver(t)
	for _, generation := range []string{"first", "second"} {
		before := time.Now().UnixMilli()
		stubPikPakHTTP(t,
			pikPakHTTPReply{
				method: http.MethodPost, path: "/v1/shield/captcha/init",
				body: `{"captcha_token":"` + generation + `-captcha"}`,
				check: func(req *http.Request) {
					if bearer := req.Header.Get("Authorization"); bearer != "" {
						t.Errorf("login captcha carries stale bearer %q", bearer)
					}
					body := pikPakRequestBody(t, req)
					if body["action"] != "POST:/v1/auth/signin" || body["device_id"] != "test-device" {
						t.Errorf("captcha request = %#v", body)
					}
					meta, ok := body["meta"].(map[string]interface{})
					if !ok {
						t.Fatalf("captcha meta = %#v", body["meta"])
					}
					for key, want := range map[string]string{
						"email": "tester@example.invalid", "client_version": "test-version", "package_name": "test-package",
					} {
						if meta[key] != want {
							t.Errorf("captcha meta[%s] = %#v, want %q", key, meta[key], want)
						}
					}
					timestamp, _ := meta["timestamp"].(string)
					millis, err := strconv.ParseInt(timestamp, 10, 64)
					if err != nil || millis < before || millis > time.Now().UnixMilli() {
						t.Errorf("captcha timestamp = %q", timestamp)
					}
					sign, _ := meta["captcha_sign"].(string)
					if ok, _ := regexp.MatchString(`^1\.[0-9a-f]{32}$`, sign); !ok {
						t.Errorf("captcha sign = %q", sign)
					}
				},
			},
			pikPakHTTPReply{
				method: http.MethodPost, path: "/v1/auth/signin",
				body: `{"access_token":"` + generation + `-access","refresh_token":"` + generation + `-refresh","sub":"new-user"}`,
				check: func(req *http.Request) {
					body := pikPakRequestBody(t, req)
					if body["captcha_token"] != generation+"-captcha" {
						t.Errorf("signin captcha_token = %#v", body["captcha_token"])
					}
					if body["username"] != d.Username || body["password"] != d.Password || req.URL.Query().Get("client_id") != d.ClientID {
						t.Error("signin credentials/client_id changed")
					}
				},
			},
		)
		if err := d.login(); err != nil {
			t.Fatalf("login: %v", err)
		}
		if d.AccessToken != generation+"-access" || d.RefreshToken != generation+"-refresh" || d.GetUserID() != "new-user" {
			t.Error("successful login did not accept the returned account tokens")
		}
		_, saved := savedPikPakStorage(t, d)
		if saved.RefreshToken != generation+"-refresh" {
			t.Errorf("saved refresh token = %q, want %q", saved.RefreshToken, generation+"-refresh")
		}
	}
}

func TestPikPakAuthRejectsEmptyTokens(t *testing.T) {
	for _, auth := range []string{"login", "refresh"} {
		for _, payload := range []struct {
			name string
			body string
		}{
			{"empty_access", `{"access_token":"","refresh_token":"new-refresh","sub":"new-user"}`},
			{"empty_refresh", `{"access_token":"new-access","refresh_token":"","sub":"new-user"}`},
			{"missing_tokens", `{}`},
		} {
			t.Run(auth+"/"+payload.name, func(t *testing.T) {
				if !isolatedPikPakTest(t) {
					return
				}
				d := newPikPakAuthDriver(t)
				before, _ := savedPikPakStorage(t, d)
				var replies []pikPakHTTPReply
				path := "/v1/auth/token"
				if auth == "login" {
					path = "/v1/auth/signin"
					replies = append(replies, pikPakHTTPReply{
						method: http.MethodPost, path: "/v1/shield/captcha/init", body: `{"captcha_token":"new-captcha"}`,
					})
				}
				replies = append(replies, pikPakHTTPReply{method: http.MethodPost, path: path, body: payload.body})
				stubPikPakHTTP(t, replies...)
				var err error
				if auth == "login" {
					err = d.login()
				} else {
					err = d.refreshToken(d.RefreshToken)
				}
				if err == nil || err.Error() != auth+" failed: server returned empty tokens" {
					t.Errorf("%s error = %v, want empty-token rejection", auth, err)
				}
				if d.GetUserID() != "old-user" || d.Addition.RefreshToken != "old-refresh" || d.Status != "old-status" {
					t.Error("invalid tokens changed the accepted user, stored refresh token, or status")
				}
				if auth == "refresh" && (d.AccessToken != "old-access" || d.RefreshToken != "old-refresh") {
					t.Error("invalid refresh payload replaced the existing runtime tokens")
				}
				after, _ := savedPikPakStorage(t, d)
				if after != before {
					t.Error("invalid tokens changed the persisted storage row")
				}
			})
		}
	}
}

func TestPikPakRequestDoesNotRecoverAuthEndpoints(t *testing.T) {
	for _, endpoint := range []struct {
		name  string
		path  string
		codes []int
	}{
		{"auth_token", "/v1/auth/token", []int{4122, 4121, 16}},
		{"auth_signin", "/v1/auth/signin", []int{4122, 4121, 16}},
		{"captcha", "/v1/shield/captcha/init", []int{4122, 4121, 16, 9}},
	} {
		for _, code := range endpoint.codes {
			t.Run(endpoint.name+"/"+strconv.Itoa(code), func(t *testing.T) {
				d := &PikPak{
					Common:       &Common{CaptchaToken: "old-captcha", UserID: "old-user"},
					AccessToken:  "old-access",
					RefreshToken: "old-refresh",
				}
				stubPikPakHTTP(t, pikPakHTTPReply{
					method: http.MethodPost, path: endpoint.path, status: http.StatusBadRequest,
					body: fmt.Sprintf(`{"error_code":%d,"error":"expired"}`, code),
				})
				body, err := d.request("https://user.mypikpak.net"+endpoint.path, http.MethodPost, nil, nil)
				if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("ErrorCode: %d ", code)) {
					t.Errorf("request error = %v, want the original provider error %d", err, code)
				}
				if body != nil || d.AccessToken != "old-access" || d.RefreshToken != "old-refresh" || d.GetCaptchaToken() != "old-captcha" {
					t.Error("auth endpoint error returned a body or changed account tokens")
				}
			})
		}
	}
}

func TestPikPakRequestRecoveryPreservesAccountAndPersists(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		code    int
		relogin bool
	}{
		{"refresh_4122", 4122, false},
		{"refresh_4121", 4121, false},
		{"refresh_16", 16, false},
		{"relogin_4126", 4122, true},
		{"captcha_9", 9, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if !isolatedPikPakTest(t) {
				return
			}
			d := newPikPakAuthDriver(t)
			const filePath = "/drive/v1/files/file-id"
			tokenReply := `{"access_token":"new-access","refresh_token":"new-refresh","sub":"new-user"}`
			wantAccess, wantRefresh, wantCaptcha := "new-access", "new-refresh", "old-captcha"
			replies := []pikPakHTTPReply{{
				method: http.MethodGet, path: filePath, status: http.StatusBadRequest,
				body: fmt.Sprintf(`{"error_code":%d,"error":"expired"}`, scenario.code),
				check: func(req *http.Request) {
					if req.Header.Get("Authorization") != "Bearer old-access" || req.Header.Get("X-Captcha-Token") != "old-captcha" {
						t.Error("initial API request did not use the existing account tokens")
					}
				},
			}}
			if scenario.code != 9 {
				refreshReply := pikPakHTTPReply{
					method: http.MethodPost, path: "/v1/auth/token", body: tokenReply,
					check: func(req *http.Request) {
						body := pikPakRequestBody(t, req)
						if body["refresh_token"] != "old-refresh" || body["grant_type"] != "refresh_token" || body["client_id"] != "test-client" {
							t.Error("refresh request did not use the existing account credentials")
						}
					},
				}
				if scenario.relogin {
					refreshReply.status = http.StatusBadRequest
					refreshReply.body = `{"error_code":4126,"error":"invalid_grant"}`
				}
				replies = append(replies, refreshReply)
			} else {
				wantAccess, wantRefresh = "old-access", "old-refresh"
			}
			if scenario.relogin || scenario.code == 9 {
				wantCaptcha = "new-captcha"
				replies = append(replies, pikPakHTTPReply{
					method: http.MethodPost, path: "/v1/shield/captcha/init", body: `{"captcha_token":"new-captcha"}`,
					check: func(req *http.Request) {
						wantBearer, wantAction := "Bearer old-access", "GET:"+filePath
						if scenario.relogin {
							wantBearer, wantAction = "", "POST:/v1/auth/signin"
						}
						body := pikPakRequestBody(t, req)
						if req.Header.Get("Authorization") != wantBearer || body["action"] != wantAction {
							t.Error("captcha refresh did not follow the appropriate login/API path")
						}
					},
				})
			}
			if scenario.relogin {
				replies = append(replies, pikPakHTTPReply{
					method: http.MethodPost, path: "/v1/auth/signin", body: tokenReply,
					check: func(req *http.Request) {
						if body := pikPakRequestBody(t, req); body["captcha_token"] != "new-captcha" {
							t.Error("relogin did not use the refreshed captcha")
						}
					},
				})
			}
			replies = append(replies, pikPakHTTPReply{
				method: http.MethodGet, path: filePath,
				body: `{"id":"file-id","web_content_link":"https://media.example.invalid/original"}`,
				check: func(req *http.Request) {
					if req.Header.Get("Authorization") != "Bearer "+wantAccess || req.Header.Get("X-Captcha-Token") != wantCaptcha {
						t.Error("API retry did not use the recovered tokens")
					}
					if req.Header.Get("X-Device-ID") != "test-device" || req.Header.Get("User-Agent") != "test-agent" {
						t.Error("API recovery changed device identity or user agent")
					}
					if req.URL.Query().Get("usage") != "CACHE" || req.URL.Query().Get("_magic") != "2021" {
						t.Error("API retry lost the original request callback")
					}
					_, saved := savedPikPakStorage(t, d)
					if saved.RefreshToken != wantRefresh {
						t.Errorf("refresh token was not persisted before API retry: %q", saved.RefreshToken)
					}
				},
			})
			stubPikPakHTTP(t, replies...)
			file, err := d.GetFileInfo(context.Background(), "file-id")
			if err != nil {
				t.Fatalf("GetFileInfo: %v", err)
			}
			if file.Id != "file-id" || file.WebContentLink != "https://media.example.invalid/original" {
				t.Error("API retry did not return the original file's result")
			}
			if d.AccessToken != wantAccess || d.RefreshToken != wantRefresh || d.GetCaptchaToken() != wantCaptcha {
				t.Error("recovery did not retain the expected account tokens")
			}
		})
	}
}

func TestPikPakLoginRequiresVerificationURL(t *testing.T) {
	if !isolatedPikPakTest(t) {
		return
	}
	d := newPikPakAuthDriver(t)
	before, _ := savedPikPakStorage(t, d)
	stubPikPakHTTP(t, pikPakHTTPReply{
		method: http.MethodPost, path: "/v1/shield/captcha/init",
		body: `{"captcha_token":"unverified-captcha","url":"https://captcha.example.invalid/verify"}`,
	})
	err := d.login()
	if err == nil || !strings.Contains(err.Error(), "need verify:") || !strings.Contains(err.Error(), "https://captcha.example.invalid/verify") {
		t.Errorf("login error = %v, want the verification URL", err)
	}
	if d.AccessToken != "" || d.GetCaptchaToken() != "old-captcha" || d.RefreshToken != "old-refresh" {
		t.Error("verification-required response accepted unverified credentials")
	}
	after, _ := savedPikPakStorage(t, d)
	if after != before {
		t.Error("verification-required response changed persisted credentials")
	}
}
