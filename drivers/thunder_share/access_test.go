package thunder_share

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/thunder_browser"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestThunderShareAccessRequiresToken(t *testing.T) {
	// db.Init cannot restore a nil handle. Keep the account fixture in a child.
	const childEnv = "MV_THUNDER_SHARE_ACCESS_TEST"
	if os.Getenv(childEnv) != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestThunderShareAccessRequiresToken$", "-test.v", "-test.timeout=15s")
		cmd.Env = []string{childEnv + "=1"}
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated share test failed: %v\n%s", err, output)
		}
		t.Logf("%s", output)
		return
	}

	conf.Conf = conf.DefaultConfig(t.TempDir())
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	db.Init(database)

	for _, tc := range []struct {
		name    string
		body    string
		wantErr string
		files   int
	}{
		{name: "empty token", body: `{"pass_code_token":"","files":[]}`, wantErr: "分享无法访问:提取码错误或分享可能因违规被屏蔽"},
		{name: "missing token", body: `{"files":[]}`, wantErr: "分享无法访问:提取码错误或分享可能因违规被屏蔽"},
		{name: "files without token", body: `{"files":[{"id":"file-1"}]}`, wantErr: "分享无法访问:提取码错误或分享可能因违规被屏蔽"},
		{name: "accessible empty share", body: `{"pass_code_token":"valid-token","files":[]}`},
		{name: "accessible files", body: `{"pass_code_token":"valid-token","files":[{"id":"file-1"}]}`, files: 1},
		{name: "upstream error", body: `{"error_code":404,"error":"share_missing"}`, wantErr: (&thunder_browser.ErrResp{ErrorCode: 404, ErrorMsg: "share_missing"}).Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var initializing atomic.Bool
			initializing.Store(true)
			var captchaCalls, shareCalls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Connection", "close")
				switch r.URL.Path {
				case "/v1/shield/captcha/init":
					if initializing.Load() {
						_, _ = io.WriteString(w, `{"error_code":400,"error":"fixture_init_stop"}`)
						return
					}
					captchaCalls.Add(1)
					_, _ = io.WriteString(w, `{"captcha_token":"test-captcha"}`)
				case "/drive/v1/share":
					shareCalls.Add(1)
					if r.Method != http.MethodGet || r.URL.Query().Get("share_id") != "test-share" ||
						r.URL.Query().Get("pass_code") != "test-pass" || r.URL.Query().Get("pass_code_token") != "" {
						t.Error("share request changed its parameters")
					}
					if r.Header.Get("Authorization") != "Bearer account-token" || r.Header.Get("X-Captcha-Token") != "test-captcha" {
						t.Error("share request changed its account or captcha headers")
					}
					_, _ = io.WriteString(w, tc.body)
				default:
					t.Errorf("unexpected account request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			t.Cleanup(server.Close)
			proxy := thunderShareLocalProxy(t, server.Listener.Addr().String())
			conf.Conf.ProxyAddress = proxy.URL
			conf.Conf.TlsInsecureSkipVerify = true
			account := &thunder_browser.ThunderBrowser{
				Storage: model.Storage{MountPath: "/share-test/" + tc.name, Driver: "ThunderBrowser"},
				Addition: thunder_browser.Addition{
					Username: "fixture-user", Password: "fixture-password", DeviceID: strings.Repeat("a", 32),
				},
			}
			// Initialize the real private HTTP client, but stop before background jobs.
			if err := account.Init(context.Background()); err == nil || !strings.Contains(err.Error(), "fixture_init_stop") {
				t.Fatalf("unexpected fixture initialization: %v", err)
			}
			account.SetCaptchaToken("bootstrap-captcha")
			account.SetTokenResp(&thunder_browser.TokenResp{TokenType: "Bearer", AccessToken: "account-token", UserID: "fixture-user"})
			initializing.Store(false)
			d := &ThunderShare{Addition: Addition{ShareId: "test-share", SharePwd: "test-pass", ShareToken: "previous-token"}}
			share, err := d.getShareInfo(context.Background(), account)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Errorf("share error = %v, want %q", err, tc.wantErr)
				}
				if d.ShareToken != "previous-token" {
					t.Error("failed share access overwrote the previous token")
				}
			} else if err != nil || d.ShareToken != "valid-token" || len(share.Files) != tc.files {
				t.Errorf("accessible share result: error=%v files=%d", err, len(share.Files))
			}
			if captchaCalls.Load() != 1 || shareCalls.Load() != 1 {
				t.Errorf("captcha/share calls = %d/%d, want 1/1", captchaCalls.Load(), shareCalls.Load())
			}
		})
	}
}

// Tunnel only to the local fixture, never to the requested external host.
func thunderShareLocalProxy(t *testing.T, target string) *httptest.Server {
	t.Helper()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || (r.Host != "xluser-ssl.xunlei.com:443" && r.Host != "x-api-pan.xunlei.com:443") {
			t.Errorf("unexpected proxy request: %s %s", r.Method, r.Host)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		local, err := net.DialTimeout("tcp", target, 5*time.Second)
		if err != nil {
			t.Errorf("connect local fixture: %v", err)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer local.Close()
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack local proxy: %v", err)
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprint(buf, "HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buf.Flush()
		go func() { _, _ = io.Copy(local, buf) }()
		_, _ = io.Copy(conn, local)
	}))
	t.Cleanup(proxy.Close)
	return proxy
}
