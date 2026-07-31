package _123Share

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/go-resty/resty/v2"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestUnlimitedOnlyRequestIsAnonymousAndSkipsCookieAccounts(t *testing.T) {
	originalClient := base.RestyClient
	originalNext := next123PanDriver
	accountLookups := 0
	next123PanDriver = func(_ int) driver.Driver {
		accountLookups++
		return nil
	}
	t.Cleanup(func() {
		base.RestyClient = originalClient
		next123PanDriver = originalNext
	})

	var authorization string
	var platform string
	var appVersion string
	base.RestyClient = resty.New().SetTransport(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		authorization = request.Header.Get("Authorization")
		platform = request.Header.Get("platform")
		appVersion = request.Header.Get("app-version")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
			},
			Body:    io.NopCloser(strings.NewReader(`{"code":0,"data":{}}`)),
			Request: request,
		}, nil
	}))

	d := &Pan123Share{
		Addition: Addition{
			AccessToken:   "must-not-be-used",
			UnlimitedOnly: true,
		},
	}
	if _, err := d.request(
		"https://anonymous.123.test/share/get",
		http.MethodGet,
		nil,
		nil,
	); err != nil {
		t.Fatalf("anonymous unlimited request: %v", err)
	}
	if authorization != "" {
		t.Fatalf("unexpected Authorization header %q", authorization)
	}
	if platform != "android" || appVersion != "43" {
		t.Fatalf("unexpected anonymous headers platform=%q app-version=%q", platform, appVersion)
	}
	if accountLookups != 0 {
		t.Fatalf("unlimited-only request looked up a cookie account %d time(s)", accountLookups)
	}
}

func TestAccessTokenAdditionRemainsDecodeCompatible(t *testing.T) {
	var addition Addition
	if err := json.Unmarshal(
		[]byte(`{"AccessToken":"legacy-open-token","unlimited_only":true}`),
		&addition,
	); err != nil {
		t.Fatalf("decode addition: %v", err)
	}
	if addition.AccessToken != "legacy-open-token" {
		t.Fatalf("unexpected AccessToken %q", addition.AccessToken)
	}
	if !addition.UnlimitedOnly {
		t.Fatal("expected unlimited_only addition to be enabled")
	}
}

func TestUnlimitedOnlyLinkReturnsAnonymousFailureWithoutFallback(t *testing.T) {
	originalAnon := resolveAnonLink
	originalCount := get123PanDriverCount
	sentinel := errors.New("anonymous exchange failed")
	accountCountCalls := 0
	anonymousCalls := 0
	resolveAnonLink = func(_ *Pan123Share, _ File, _ string) (*model.Link, error) {
		anonymousCalls++
		return nil, sentinel
	}
	get123PanDriverCount = func() int {
		accountCountCalls++
		return 1
	}
	t.Cleanup(func() {
		resolveAnonLink = originalAnon
		get123PanDriverCount = originalCount
	})

	d := &Pan123Share{Addition: Addition{UnlimitedOnly: true}}
	_, err := d.Link(
		context.Background(),
		File{FileName: "video.mkv", FileId: 1, Size: 1024},
		model.LinkArgs{},
	)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected anonymous failure, got %v", err)
	}
	if anonymousCalls != 1 {
		t.Fatalf("expected one anonymous attempt, got %d", anonymousCalls)
	}
	if accountCountCalls != 0 {
		t.Fatalf("unlimited-only link consulted cookie accounts %d time(s)", accountCountCalls)
	}
}

func TestTraditionalModeRequiresNormal123PanAccount(t *testing.T) {
	originalAnon := resolveAnonLink
	originalCount := get123PanDriverCount
	anonymousCalls := 0
	resolveAnonLink = func(_ *Pan123Share, _ File, _ string) (*model.Link, error) {
		anonymousCalls++
		return &model.Link{URL: "https://should-not-be-used.example"}, nil
	}
	get123PanDriverCount = func() int {
		return 0
	}
	t.Cleanup(func() {
		resolveAnonLink = originalAnon
		get123PanDriverCount = originalCount
	})

	d := &Pan123Share{
		Addition: Addition{
			AccessToken: "legacy-open-token-must-not-authorize",
		},
	}
	_, err := d.Link(
		context.Background(),
		File{FileName: "video.mkv", FileId: 1, Size: 1024},
		model.LinkArgs{},
	)
	if err == nil || !strings.Contains(err.Error(), "找不到123云盘帐号") {
		t.Fatalf("expected missing normal 123Pan account, got %v", err)
	}
	if anonymousCalls != 0 {
		t.Fatalf("traditional mode called anonymous provider %d time(s)", anonymousCalls)
	}
}
