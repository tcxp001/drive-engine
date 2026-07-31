package thunder_browser

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/go-resty/resty/v2"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func jsonResponse(req *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": {"application/json"},
		},
		Body:    io.NopCloser(strings.NewReader(body)),
		Request: req,
	}
}

func newCaptchaRetryCommon(transport http.RoundTripper) *XunLeiBrowserCommon {
	client := resty.New()
	client.SetTransport(transport)
	common := &Common{
		client:        client,
		ClientID:      "client-id",
		ClientVersion: "client-version",
		PackageName:   "package-name",
		DeviceID:      "device-id",
		UserAgent:     "user-agent",
	}
	common.SetCaptchaToken("stale-captcha-token")
	return &XunLeiBrowserCommon{
		Common: common,
		TokenResp: &TokenResp{
			TokenType:   "Bearer",
			AccessToken: "access-token",
			UserID:      "user-id",
		},
	}
}

func TestRequestRetriesOnceAfterCaptchaRefresh(t *testing.T) {
	dataRequests := 0
	captchaRequests := 0
	common := newCaptchaRetryCommon(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/shield/captcha/init") {
			captchaRequests++
			return jsonResponse(req, `{"captcha_token":"fresh-captcha-token"}`), nil
		}
		dataRequests++
		if dataRequests == 1 {
			if got := req.Header.Get("X-Captcha-Token"); got != "stale-captcha-token" {
				t.Fatalf("first captcha token = %q", got)
			}
			return jsonResponse(req, `{"error_code":9,"error":"captcha_invalid"}`), nil
		}
		if got := req.Header.Get("X-Captcha-Token"); got != "fresh-captcha-token" {
			t.Fatalf("retried captcha token = %q", got)
		}
		return jsonResponse(req, `{"name":"video.mkv"}`), nil
	}))

	var result struct {
		Name string `json:"name"`
	}
	_, err := common.Request(
		"https://x-api-pan.xunlei.com/drive/v1/files",
		http.MethodGet,
		nil,
		&result,
	)
	if err != nil {
		t.Fatalf("Request() error = %v", err)
	}
	if result.Name != "video.mkv" {
		t.Fatalf("result name = %q", result.Name)
	}
	if dataRequests != 2 {
		t.Fatalf("data requests = %d, want 2", dataRequests)
	}
	if captchaRequests != 1 {
		t.Fatalf("captcha requests = %d, want 1", captchaRequests)
	}
}

func TestRequestReturnsCaptchaRefreshError(t *testing.T) {
	dataRequests := 0
	captchaRequests := 0
	common := newCaptchaRetryCommon(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/shield/captcha/init") {
			captchaRequests++
			return jsonResponse(
				req,
				`{"error_code":9,"error":"captcha_refresh_failed"}`,
			), nil
		}
		dataRequests++
		return jsonResponse(req, `{"error_code":9,"error":"captcha_invalid"}`), nil
	}))

	_, err := common.Request(
		"https://x-api-pan.xunlei.com/drive/v1/files",
		http.MethodGet,
		nil,
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "captcha_refresh_failed") {
		t.Fatalf("Request() error = %v", err)
	}
	if dataRequests != 1 {
		t.Fatalf("data requests = %d, want 1", dataRequests)
	}
	if captchaRequests != 1 {
		t.Fatalf("captcha requests = %d, want 1", captchaRequests)
	}
}

func TestRequestStopsAfterRetriedCaptchaInvalid(t *testing.T) {
	dataRequests := 0
	captchaRequests := 0
	common := newCaptchaRetryCommon(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/shield/captcha/init") {
			captchaRequests++
			return jsonResponse(req, `{"captcha_token":"fresh-captcha-token"}`), nil
		}
		dataRequests++
		return jsonResponse(req, `{"error_code":9,"error":"captcha_invalid"}`), nil
	}))

	_, err := common.Request(
		"https://x-api-pan.xunlei.com/drive/v1/files",
		http.MethodGet,
		nil,
		nil,
	)
	if err == nil || err.Error() != "captcha_invalid" {
		t.Fatalf("Request() error = %v", err)
	}
	if dataRequests != 2 {
		t.Fatalf("data requests = %d, want 2", dataRequests)
	}
	if captchaRequests != 1 {
		t.Fatalf("captcha requests = %d, want 1", captchaRequests)
	}
}
