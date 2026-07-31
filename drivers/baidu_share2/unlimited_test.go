package baidu_share

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/drivers/baidu_netdisk"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// baiduRoundTripFunc 让单测按请求 URL 精确编排 dlna 交互，
// 无需真实网络。返回的响应 Body 会被调用方 Close。
type baiduRoundTripFunc func(req *http.Request) (*http.Response, error)

func (f baiduRoundTripFunc) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}

func baiduJSONResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func baiduRedirectResponse(location string) *http.Response {
	header := http.Header{}
	if location != "" {
		header.Set("Location", location)
	}
	return &http.Response{
		StatusCode: http.StatusFound,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader("")),
	}
}

func TestBaiduDlnaRandIsDeterministic(t *testing.T) {
	// 签名 = sha1( sha1(BDUSS) + uid + saltA + time + saltB )，
	// 相同输入必须稳定，且 BDUSS/uid/time 任一变化都改变结果。
	got := baiduDlnaRand("BDUSS_VALUE", "12345", "1700000000000")
	inner := baiduSha1("BDUSS_VALUE")
	want := baiduSha1(inner + "12345" + baiduDlnaSaltA + "1700000000000" + baiduDlnaSaltB())
	if got != want {
		t.Fatalf("dlna rand mismatch: got %s want %s", got, want)
	}
	if baiduDlnaRand("BDUSS_VALUE", "12345", "1700000000001") == got {
		t.Fatal("dlna rand must change when time changes")
	}
	if baiduDlnaRand("OTHER", "12345", "1700000000000") == got {
		t.Fatal("dlna rand must change when BDUSS changes")
	}
}

func TestBaiduPickHTTPURLPrefersNamedFields(t *testing.T) {
	// 严格模式只认 dlink/downloadurl/download_url/url，即便有其它 http 字符串。
	data := map[string]any{
		"list": []any{
			map[string]any{
				"server_filename": "movie.mp4",
				"thumbs":          "https://thumb.example.com/x.jpg",
				"dlink":           "https://d.pcs.baidu.com/file/real",
			},
		},
	}
	if got := baiduPickHTTPURL(data); got != "https://d.pcs.baidu.com/file/real" {
		t.Fatalf("expected dlink, got %s", got)
	}
}

func TestBaiduPickHTTPURLFallsBackToAnyHTTP(t *testing.T) {
	// 宽松模式：没有命名字段时退化为任意 http 字符串。
	data := map[string]any{"nested": map[string]any{"weird_key": "https://fallback.example.com/x"}}
	if got := baiduPickHTTPURL(data); got != "https://fallback.example.com/x" {
		t.Fatalf("expected fallback URL, got %s", got)
	}
}

func TestBaiduErrnoParsesVariants(t *testing.T) {
	if baiduErrno(map[string]any{"errno": float64(0)}) != 0 {
		t.Fatal("errno 0 float should be 0")
	}
	if baiduErrno(map[string]any{"errno": float64(-9)}) != -9 {
		t.Fatal("errno -9 float should be -9")
	}
	if baiduErrno(map[string]any{"error_code": "31066"}) != 31066 {
		t.Fatal("error_code string should parse")
	}
}

func TestResolveBaiduUnlimitedLinkHappyPath(t *testing.T) {
	const finalURL = "https://d.pcs.baidu.com/appall01/real-stream"
	client := baiduRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(req.URL.Host, "mbd.baidu.com"):
			return baiduJSONResponse(http.StatusOK, `{"uid":"987654"}`), nil
		case strings.Contains(req.URL.Path, "/share/list"):
			if req.URL.Query().Get("origin") != "dlna" {
				t.Fatalf("share/list must use origin=dlna, got %q", req.URL.Query().Get("origin"))
			}
			if req.URL.Query().Get("rand") == "" || req.URL.Query().Get("time") == "" {
				t.Fatal("share/list must carry signed rand and time")
			}
			return baiduJSONResponse(http.StatusOK, `{"errno":0,"list":[{"dlink":"https://pan.baidu.com/redirect/dlink"}]}`), nil
		case strings.Contains(req.URL.Path, "/redirect/dlink"):
			return baiduRedirectResponse(finalURL), nil
		default:
			t.Fatalf("unexpected request: %s", req.URL.String())
			return nil, nil
		}
	})

	link, err := resolveBaiduUnlimitedLink(context.Background(), client, baiduUnlimitedParams{
		shareID: "sid",
		shareUK: "uk",
		sekey:   "sekey-raw",
		fileID:  "fid-1",
		cookie:  "BDUSS=abc; STOKEN=xyz",
	})
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if link.URL != finalURL {
		t.Fatalf("expected final CDN URL %s, got %s", finalURL, link.URL)
	}
	if got := link.Header.Get("User-Agent"); got != baiduDlnaUA {
		t.Fatalf("expected dlna UA, got %s", got)
	}
	if link.Header.Get("Cookie") != "" {
		t.Fatal("final dlna link must not carry Cookie")
	}
	if link.Expiration == nil {
		t.Fatal("dlna link should set an expiration")
	}
}

func TestResolveBaiduUnlimitedLinkDecodesSekeyAndSendsShareCookie(t *testing.T) {
	const (
		encodedSekey = "%2Ftoken%2Bvalue%3D"
		decodedSekey = "/token+value="
		finalURL     = "https://d.pcs.baidu.com/appall01/encoded-sekey-stream"
	)
	client := baiduRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(req.URL.Host, "mbd.baidu.com"):
			return baiduJSONResponse(http.StatusOK, `{"uid":"987654"}`), nil
		case strings.Contains(req.URL.Path, "/share/list"):
			shareCookie, err := req.Cookie("BDCLND")
			if err != nil || shareCookie.Value != encodedSekey {
				return baiduJSONResponse(http.StatusOK, `{"errno":-9}`), nil
			}
			if got := req.URL.Query().Get("sekey"); got != decodedSekey {
				return baiduJSONResponse(http.StatusOK, `{"errno":-9}`), nil
			}
			return baiduJSONResponse(
				http.StatusOK,
				`{"errno":0,"list":[{"dlink":"https://pan.baidu.com/redirect/encoded"}]}`,
			), nil
		case strings.Contains(req.URL.Path, "/redirect/encoded"):
			return baiduRedirectResponse(finalURL), nil
		default:
			t.Fatalf("unexpected request: %s", req.URL.String())
			return nil, nil
		}
	})

	link, err := resolveBaiduUnlimitedLink(context.Background(), client, baiduUnlimitedParams{
		shareID: "sid",
		shareUK: "uk",
		sekey:   encodedSekey,
		fileID:  "fid-encoded",
		cookie:  "BDUSS=abc; STOKEN=xyz",
	})
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if link.URL != finalURL {
		t.Fatalf("expected final CDN URL %s, got %s", finalURL, link.URL)
	}
}

func TestDecodeBaiduSekeyPreservesLiteralPlus(t *testing.T) {
	got, err := decodeBaiduSekey("literal+plus")
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if got != "literal+plus" {
		t.Fatalf("literal plus must be preserved, got %q", got)
	}
}

func TestResolveBaiduUnlimitedLinkAllowsPublicShareWithoutSekey(t *testing.T) {
	const dlink = "https://pan.baidu.com/direct/public-dlink"
	client := baiduRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(req.URL.Host, "mbd.baidu.com"):
			return baiduJSONResponse(http.StatusOK, `{"uid":"1"}`), nil
		case strings.Contains(req.URL.Path, "/share/list"):
			if req.URL.Query().Has("sekey") {
				t.Fatal("public share request must not add an empty sekey")
			}
			if _, err := req.Cookie("BDCLND"); err == nil {
				t.Fatal("public share request must not add an empty BDCLND Cookie")
			}
			return baiduJSONResponse(http.StatusOK, `{"errno":0,"dlink":"`+dlink+`"}`), nil
		default:
			return baiduRedirectResponse(""), nil
		}
	})

	link, err := resolveBaiduUnlimitedLink(context.Background(), client, baiduUnlimitedParams{
		shareID: "sid", shareUK: "uk", fileID: "fid", cookie: "BDUSS=v",
	})
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if link.URL != dlink {
		t.Fatalf("expected public dlink %s, got %s", dlink, link.URL)
	}
}

func TestResolveBaiduUnlimitedLinkFallsBackToDlinkWithoutRedirect(t *testing.T) {
	// 302 无 Location 时用原始 dlink 兜底。
	const dlink = "https://pan.baidu.com/direct/dlink"
	client := baiduRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(req.URL.Host, "mbd.baidu.com"):
			return baiduJSONResponse(http.StatusOK, `{"uid":"1"}`), nil
		case strings.Contains(req.URL.Path, "/share/list"):
			return baiduJSONResponse(http.StatusOK, `{"errno":0,"dlink":"`+dlink+`"}`), nil
		default:
			return baiduRedirectResponse(""), nil
		}
	})
	link, err := resolveBaiduUnlimitedLink(context.Background(), client, baiduUnlimitedParams{
		shareID: "sid", shareUK: "uk", sekey: "sk", fileID: "fid", cookie: "BDUSS=v",
	})
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if link.URL != dlink {
		t.Fatalf("expected fallback to raw dlink %s, got %s", dlink, link.URL)
	}
}

func TestResolveBaiduUnlimitedLinkRequiresBDUSS(t *testing.T) {
	client := baiduRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatal("must not hit network without BDUSS")
		return nil, nil
	})
	_, err := resolveBaiduUnlimitedLink(context.Background(), client, baiduUnlimitedParams{
		shareID: "sid", shareUK: "uk", sekey: "sk", fileID: "fid", cookie: "STOKEN=only",
	})
	if err == nil {
		t.Fatal("expected error when Cookie lacks BDUSS")
	}
}

func TestResolveBaiduUnlimitedLinkRejectsUpstreamErrno(t *testing.T) {
	client := baiduRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(req.URL.Host, "mbd.baidu.com"):
			return baiduJSONResponse(http.StatusOK, `{"uid":"1"}`), nil
		case strings.Contains(req.URL.Path, "/share/list"):
			return baiduJSONResponse(http.StatusOK, `{"errno":-9}`), nil
		default:
			t.Fatalf("unexpected request: %s", req.URL.String())
			return nil, nil
		}
	})
	_, err := resolveBaiduUnlimitedLink(context.Background(), client, baiduUnlimitedParams{
		shareID: "sid", shareUK: "uk", sekey: "sk", fileID: "fid", cookie: "BDUSS=v",
	})
	if err == nil {
		t.Fatal("expected error when share/list returns non-zero errno")
	}
	if !isBaiduDlnaErrno(err, -9) {
		t.Fatalf("expected typed errno -9, got %v", err)
	}
}

func TestResolveBaiduUnlimitedLinkRejectsIncompleteParams(t *testing.T) {
	client := baiduRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatal("must not hit network with incomplete params")
		return nil, nil
	})
	_, err := resolveBaiduUnlimitedLink(context.Background(), client, baiduUnlimitedParams{
		shareID: "sid", shareUK: "", sekey: "sk", fileID: "fid", cookie: "BDUSS=v",
	})
	if err == nil {
		t.Fatal("expected error when share metadata is incomplete")
	}
}

func TestBaiduUnlimitedLinkRefreshesShareValidationOnceOnErrnoMinus9(t *testing.T) {
	origClient := baiduUnlimitedHTTPClient
	origValidate := validateBaiduShare
	t.Cleanup(func() {
		baiduUnlimitedHTTPClient = origClient
		validateBaiduShare = origValidate
	})

	validateCalls := 0
	validateBaiduShare = func(d *BaiduShare2) error {
		validateCalls++
		d.Token = "%2Ffresh%2Btoken%3D"
		return nil
	}

	shareCalls := 0
	baiduUnlimitedHTTPClient = baiduRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(req.URL.Host, "mbd.baidu.com"):
			return baiduJSONResponse(http.StatusOK, `{"uid":"1"}`), nil
		case strings.Contains(req.URL.Path, "/share/list"):
			shareCalls++
			if req.URL.Query().Get("sekey") != "/fresh+token=" {
				return baiduJSONResponse(http.StatusOK, `{"errno":-9}`), nil
			}
			return baiduJSONResponse(
				http.StatusOK,
				`{"errno":0,"dlink":"https://pan.baidu.com/direct/refreshed"}`,
			), nil
		default:
			return baiduRedirectResponse(""), nil
		}
	})

	d := &BaiduShare2{
		ShareId: "sid",
		ShareUk: "uk",
		Token:   "%2Fstale%2Btoken%3D",
	}
	link, err := d.unlimitedLink(
		context.Background(),
		&model.Object{ID: "fid", Name: "video.mkv"},
		&baidu_netdisk.BaiduNetdisk{Addition: baidu_netdisk.Addition{Cookie: "BDUSS=v"}},
	)
	if err != nil {
		t.Fatalf("resolve after refresh failed: %v", err)
	}
	if link.URL != "https://pan.baidu.com/direct/refreshed" {
		t.Fatalf("unexpected refreshed link: %s", link.URL)
	}
	if validateCalls != 1 {
		t.Fatalf("expected one share validation refresh, got %d", validateCalls)
	}
	if shareCalls != 2 {
		t.Fatalf("expected one errno -9 request plus one retry, got %d", shareCalls)
	}
}
