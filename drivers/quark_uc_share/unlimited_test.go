package quark_uc_share

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
)

var ucUnlimitedTestContent = []byte("0123456789abcdefghijklmnopqrstuvwxyz")

type ucUnlimitedMock struct {
	server *httptest.Server

	mu                 sync.Mutex
	tokenCalls         int
	detailCalls        int
	downloadCalls      int
	saveCalls          int
	cdnCookies         []string
	cdnReferers        []string
	cdnOrigins         []string
	traditionalCookies []string
	traditionalRanges  []string
	apiCookies         []string
	apiPR              []string
	apiEntries         []string
	downloadCookie     map[string]string
	oldFails           bool
	strictFailure      bool
	apiFailure         bool
}

func newUCUnlimitedMock(t *testing.T) *ucUnlimitedMock {
	t.Helper()
	mock := &ucUnlimitedMock{downloadCookie: make(map[string]string)}
	mock.server = httptest.NewServer(http.HandlerFunc(mock.serveHTTP))
	t.Cleanup(mock.server.Close)
	return mock
}

func (m *ucUnlimitedMock) serveHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.apiPR = append(m.apiPR, r.URL.Query().Get("pr"))
	m.apiEntries = append(m.apiEntries, r.URL.Query().Get("entry"))
	m.mu.Unlock()
	switch r.URL.Path {
	case "/share/sharepage/token":
		m.mu.Lock()
		m.tokenCalls++
		m.apiCookies = append(m.apiCookies, r.Header.Get("Cookie"))
		apiFailure := m.apiFailure
		m.mu.Unlock()
		if apiFailure {
			http.Error(w, "rejected", http.StatusServiceUnavailable)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		http.SetCookie(w, &http.Cookie{Name: "__puus", Value: "lease-" + body["pwd_id"]})
		writeUCJSON(w, map[string]any{
			"status": 200,
			"code":   0,
			"data":   map[string]string{"stoken": "stoken-" + body["pwd_id"]},
		})
	case "/share/sharepage/detail":
		m.mu.Lock()
		m.detailCalls++
		m.apiCookies = append(m.apiCookies, r.Header.Get("Cookie"))
		m.mu.Unlock()
		writeUCJSON(w, map[string]any{
			"status": 200,
			"code":   0,
			"data": map[string]any{"list": []map[string]any{{
				"fid":             "fid",
				"share_fid_token": "fresh-token",
				"pdir_fid":        "parent",
				"file_type":       1,
				"file":            true,
				"size":            len(ucUnlimitedTestContent),
			}}},
			"metadata": map[string]int{"_total": 1},
		})
	case "/file/download":
		var body struct {
			ShareID string `json:"pwd_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		m.downloadCalls++
		call := m.downloadCalls
		m.apiCookies = append(m.apiCookies, r.Header.Get("Cookie"))
		m.downloadCookie[body.ShareID] = r.Header.Get("Cookie")
		apiFailure := m.apiFailure
		oldFails := m.oldFails
		m.mu.Unlock()
		if apiFailure {
			http.Error(w, "rejected", http.StatusServiceUnavailable)
			return
		}
		cdnPath := "/cdn/current"
		if oldFails && call == 1 {
			cdnPath = "/cdn/old"
		}
		writeUCJSON(w, map[string]any{
			"status": 200,
			"code":   0,
			"data": []map[string]any{{
				"download_url": m.server.URL + cdnPath,
				"size":         len(ucUnlimitedTestContent),
			}},
		})
	case "/share/sharepage/save":
		m.mu.Lock()
		m.saveCalls++
		m.mu.Unlock()
		http.Error(w, "must not be called", http.StatusInternalServerError)
	case "/cdn/old":
		m.mu.Lock()
		m.cdnCookies = append(m.cdnCookies, r.Header.Get("Cookie"))
		m.cdnReferers = append(m.cdnReferers, r.Header.Get("Referer"))
		m.cdnOrigins = append(m.cdnOrigins, r.Header.Get("Origin"))
		oldFails := m.oldFails
		m.mu.Unlock()
		if oldFails && r.Header.Get("Range") != "bytes=0-0" {
			http.Error(w, "expired", http.StatusForbidden)
			return
		}
		m.serveCDN(w, r)
	case "/cdn/current":
		m.mu.Lock()
		m.cdnCookies = append(m.cdnCookies, r.Header.Get("Cookie"))
		m.cdnReferers = append(m.cdnReferers, r.Header.Get("Referer"))
		m.cdnOrigins = append(m.cdnOrigins, r.Header.Get("Origin"))
		m.mu.Unlock()
		m.serveCDN(w, r)
	case "/traditional":
		m.mu.Lock()
		m.traditionalCookies = append(m.traditionalCookies, r.Header.Get("Cookie"))
		m.traditionalRanges = append(m.traditionalRanges, r.Header.Get("Range"))
		m.mu.Unlock()
		m.serveCDNResponse(w, r, false)
	default:
		http.NotFound(w, r)
	}
}

func (m *ucUnlimitedMock) serveCDN(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	strictFailure := m.strictFailure
	m.mu.Unlock()
	m.serveCDNResponse(w, r, strictFailure)
}

func (m *ucUnlimitedMock) serveCDNResponse(
	w http.ResponseWriter,
	r *http.Request,
	strictFailure bool,
) {
	rangeHeader := r.Header.Get("Range")
	if rangeHeader == "" || strictFailure {
		w.Header().Set("Content-Length", strconv.Itoa(len(ucUnlimitedTestContent)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(ucUnlimitedTestContent)
		return
	}
	var start, end int
	if _, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end); err != nil ||
		start < 0 || end < start || end >= len(ucUnlimitedTestContent) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	payload := ucUnlimitedTestContent[start : end+1]
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(ucUnlimitedTestContent)))
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(payload)
}

func writeUCJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func installUCUnlimitedTestKeeper(t *testing.T, mock *ucUnlimitedMock) *ucUnlimitedLeaseKeeper {
	t.Helper()
	original := quarkUCUnlimitedKeeper
	keeper := newUCUnlimitedLeaseKeeper(mock.server.Client(), mock.server.URL)
	quarkUCUnlimitedKeeper = keeper
	t.Cleanup(func() {
		quarkUCUnlimitedKeeper = original
	})
	return keeper
}

func ucUnlimitedTestDriver(storageID uint, shareID string) *QuarkUCShare {
	return &QuarkUCShare{
		Storage: model.Storage{ID: storageID},
		Addition: Addition{
			ShareId:  shareID,
			SharePwd: "share-password",
		},
		config: driver.Config{Name: "UCShare"},
	}
}

func ucUnlimitedTestFile() model.Obj {
	return &model.Object{
		ID:   "fid-original-token-parent",
		Name: "redacted",
		Size: int64(len(ucUnlimitedTestContent)),
	}
}

func quarkUnlimitedTestDriver(storageID uint, shareID string) *QuarkUCShare {
	return &QuarkUCShare{
		Storage: model.Storage{ID: storageID},
		Addition: Addition{
			ShareId:   shareID,
			SharePwd:  "share-password",
			APICookie: "__uid=mv-quark-account-secret; __puus=stale-lease",
		},
		config: driver.Config{Name: "QuarkShare"},
	}
}

func TestUCUnlimitedConfigForcesStableProxy(t *testing.T) {
	d := ucUnlimitedTestDriver(1, "share-a")
	if !d.Config().NoLinkURL {
		t.Fatal("UCShare must force the stable PowerList proxy URL")
	}
	quark := &QuarkUCShare{config: driver.Config{Name: "QuarkShare"}}
	if !quark.Config().NoLinkURL {
		t.Fatal("QuarkShare must force the stable PowerList proxy URL")
	}
}

func TestQuarkUnlimitedProviderUsesPCShareAPI(t *testing.T) {
	provider, err := unlimitedProviderFor(quarkUnlimitedTestDriver(2, "quark-share"))
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if provider.apiBase != "https://drive-pc.quark.cn/1/clouddrive" {
		t.Fatalf("unexpected Quark unlimited API base %q", provider.apiBase)
	}
}

func TestQuarkUnlimitedResolveUsesAPICookieAndLeaseOnlyCDNRange(t *testing.T) {
	mock := newUCUnlimitedMock(t)
	installUCUnlimitedTestKeeper(t, mock)
	d := quarkUnlimitedTestDriver(5, "quark-share")

	link, err := d.Link(context.Background(), ucUnlimitedTestFile(), model.LinkArgs{
		Header: http.Header{"Cookie": {"must-not-leak"}},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if link.URL != "" {
		t.Fatal("default Quark unlimited link exposed the signed CDN URL")
	}
	body, err := link.RangeReader.RangeRead(
		context.Background(), http_range.Range{Start: 3, Length: 4})
	if err != nil {
		t.Fatalf("range read: %v", err)
	}
	payload, _ := io.ReadAll(body)
	_ = body.Close()
	if string(payload) != "3456" {
		t.Fatalf("unexpected range payload %q", payload)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.saveCalls != 0 {
		t.Fatalf("Quark unlimited hit called save %d times", mock.saveCalls)
	}
	for _, value := range mock.apiCookies {
		if strings.Contains(value, "must-not-leak") {
			t.Fatal("request account Cookie leaked into Quark unlimited API")
		}
		if !strings.Contains(value, "__uid=mv-quark-account-secret") {
			t.Fatalf("MV Quark API Cookie was not attached to API request: %q", value)
		}
	}
	if !containsString(mock.apiPR, "ucpro") || !containsString(mock.apiEntries, "ft") {
		t.Fatalf("Quark unlimited API query missing: pr=%v entry=%v",
			mock.apiPR, mock.apiEntries)
	}
	if got := mock.downloadCookie["quark-share"]; !strings.Contains(
		got, "__puus=lease-quark-share",
	) || strings.Contains(got, "stale-lease") {
		t.Fatalf("Quark API did not rotate to the current lease Cookie: %q", got)
	}
	for _, value := range mock.cdnCookies {
		if value != "__puus=lease-quark-share" {
			t.Fatalf("Quark CDN received more than the lease Cookie: %q", value)
		}
	}
	for _, referer := range mock.cdnReferers {
		if referer != "https://pan.quark.cn/" {
			t.Fatalf("unexpected Quark CDN Referer %q", referer)
		}
	}
	for _, origin := range mock.cdnOrigins {
		if origin != "https://pan.quark.cn" {
			t.Fatalf("unexpected Quark CDN Origin %q", origin)
		}
	}
}

func TestQuarkUCUnlimitedRawLinkExposesOnlySafeLeaseHeaders(t *testing.T) {
	tests := []struct {
		name            string
		driver          func(uint, string) *QuarkUCShare
		expectedReferer func(string) string
		expectedOrigin  string
		expectedCookie  string
	}{
		{
			name:   "uc",
			driver: ucUnlimitedTestDriver,
			expectedReferer: func(rawURL string) string {
				return rawURL + "\\ "
			},
		},
		{
			name:   "quark",
			driver: quarkUnlimitedTestDriver,
			expectedReferer: func(string) string {
				return "https://pan.quark.cn/"
			},
			expectedOrigin: "https://pan.quark.cn",
			expectedCookie: "__puus=lease-share-raw-quark",
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mock := newUCUnlimitedMock(t)
			installUCUnlimitedTestKeeper(t, mock)
			d := test.driver(uint(index+30), "share-raw-"+test.name)

			link, err := d.Link(context.Background(), ucUnlimitedTestFile(), model.LinkArgs{
				Type:   model.LinkTypeAnonymousRaw,
				Header: http.Header{"Cookie": {"must-not-leak"}},
			})
			if err != nil {
				t.Fatalf("resolve raw: %v", err)
			}
			if link.URL == "" || link.RangeReader == nil {
				t.Fatalf("raw link is incomplete: %+v", link)
			}
			if got := link.Header.Get("Referer"); got != test.expectedReferer(link.URL) {
				t.Fatalf("unexpected raw Referer %q", got)
			}
			if got := link.Header.Get("Origin"); got != test.expectedOrigin {
				t.Fatalf("unexpected raw Origin %q", got)
			}
			if got := link.Header.Get("Cookie"); got != test.expectedCookie {
				t.Fatalf("unexpected safe RAW lease Cookie %q", got)
			}
			if link.Header.Get("Authorization") != "" ||
				strings.Contains(link.Header.Get("Cookie"), "mv-quark-account-secret") {
				t.Fatal("raw link exposed complete account credentials")
			}
			mock.mu.Lock()
			defer mock.mu.Unlock()
			if mock.saveCalls != 0 {
				t.Fatalf("raw anonymous link called save %d times", mock.saveCalls)
			}
		})
	}
}

func TestUCUnlimitedRawLinkNeverUsesTraditionalAccountFallback(t *testing.T) {
	mock := newUCUnlimitedMock(t)
	mock.strictFailure = true
	installUCUnlimitedTestKeeper(t, mock)
	original := resolveQuarkUCShareLink
	var fallbackCalls atomic.Int64
	resolveQuarkUCShareLink = func(
		context.Context,
		*QuarkUCShare,
		model.Obj,
		model.LinkArgs,
	) (*model.Link, error) {
		fallbackCalls.Add(1)
		return &model.Link{
			URL:    "https://account-backed.example/video",
			Header: http.Header{"Cookie": {"account-secret"}},
		}, nil
	}
	t.Cleanup(func() {
		resolveQuarkUCShareLink = original
	})

	d := ucUnlimitedTestDriver(40, "share-raw-failure")
	_, err := d.Link(context.Background(), ucUnlimitedTestFile(), model.LinkArgs{
		Type: model.LinkTypeAnonymousRaw,
	})
	if err == nil {
		t.Fatal("RAW link unexpectedly accepted a failed anonymous probe")
	}
	if fallbackCalls.Load() != 0 {
		t.Fatal("RAW link used the traditional account fallback")
	}
}

func TestQuarkUCUnlimitedOnlyNeverUsesTraditionalAccountFallback(t *testing.T) {
	tests := []struct {
		name   string
		driver func(uint, string) *QuarkUCShare
		args   model.LinkArgs
	}{
		{
			name:   "uc-proxy",
			driver: ucUnlimitedTestDriver,
		},
		{
			name:   "quark-proxy",
			driver: quarkUnlimitedTestDriver,
		},
		{
			name:   "quark-raw",
			driver: quarkUnlimitedTestDriver,
			args:   model.LinkArgs{Type: model.LinkTypeAnonymousRaw},
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mock := newUCUnlimitedMock(t)
			mock.strictFailure = true
			installUCUnlimitedTestKeeper(t, mock)

			original := resolveQuarkUCShareLink
			var fallbackCalls atomic.Int64
			resolveQuarkUCShareLink = func(
				context.Context,
				*QuarkUCShare,
				model.Obj,
				model.LinkArgs,
			) (*model.Link, error) {
				fallbackCalls.Add(1)
				return &model.Link{
					URL:    "https://account-backed.example/video",
					Header: http.Header{"Cookie": {"account-secret"}},
				}, nil
			}
			t.Cleanup(func() {
				resolveQuarkUCShareLink = original
			})

			d := test.driver(uint(index+50), "share-unlimited-only-"+test.name)
			d.UnlimitedOnly = true
			if _, err := d.Link(
				context.Background(),
				ucUnlimitedTestFile(),
				test.args,
			); err == nil {
				t.Fatal("unlimited-only mount accepted a failed anonymous link")
			}
			if fallbackCalls.Load() != 0 {
				t.Fatalf(
					"unlimited-only mount used the traditional account fallback %d times",
					fallbackCalls.Load(),
				)
			}
		})
	}
}

func TestQuarkUnlimitedRequiresDedicatedAPICookie(t *testing.T) {
	mock := newUCUnlimitedMock(t)
	installUCUnlimitedTestKeeper(t, mock)
	d := quarkUnlimitedTestDriver(59, "share-missing-api-cookie")
	d.APICookie = ""
	d.UnlimitedOnly = true

	if _, err := d.Link(
		context.Background(),
		ucUnlimitedTestFile(),
		model.LinkArgs{},
	); err == nil || !strings.Contains(err.Error(), "API Cookie is unavailable") {
		t.Fatalf("Quark unlimited accepted a missing API Cookie: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.tokenCalls != 0 || mock.downloadCalls != 0 || mock.saveCalls != 0 {
		t.Fatalf(
			"missing API Cookie unexpectedly reached upstream: token=%d download=%d save=%d",
			mock.tokenCalls,
			mock.downloadCalls,
			mock.saveCalls,
		)
	}
}

func TestQuarkUnlimitedRawFallsBackToCredentialSafeDirectLink(t *testing.T) {
	mock := newUCUnlimitedMock(t)
	mock.strictFailure = true
	installUCUnlimitedTestKeeper(t, mock)
	original := resolveQuarkUCShareLink
	var fallbackCalls atomic.Int64
	resolveQuarkUCShareLink = func(
		context.Context,
		*QuarkUCShare,
		model.Obj,
		model.LinkArgs,
	) (*model.Link, error) {
		fallbackCalls.Add(1)
		expiration := 30 * time.Minute
		return &model.Link{
			URL: mock.server.URL + "/traditional",
			Header: http.Header{
				"Cookie":        {"__puus=cdn-lease; __uid=account-secret"},
				"Authorization": {"Bearer secret"},
				"User-Agent":    {"quark-safe-raw-test"},
				"Referer":       {"https://pan.quark.cn/"},
			},
			Expiration: &expiration,
		}, nil
	}
	t.Cleanup(func() {
		resolveQuarkUCShareLink = original
	})

	d := quarkUnlimitedTestDriver(41, "share-quark-safe-raw")
	link, err := d.Link(context.Background(), ucUnlimitedTestFile(), model.LinkArgs{
		Type: model.LinkTypeAnonymousRaw,
	})
	if err != nil {
		t.Fatalf("resolve credential-safe raw: %v", err)
	}
	if link.URL != mock.server.URL+"/traditional" {
		t.Fatalf("unexpected safe raw URL %q", link.URL)
	}
	if link.Header.Get("Cookie") != "__puus=cdn-lease" ||
		link.Header.Get("Authorization") != "" {
		t.Fatalf("credential-safe raw headers were not reduced to the CDN lease: %v",
			link.Header)
	}
	if link.Header.Get("User-Agent") != "quark-safe-raw-test" {
		t.Fatalf("safe raw User-Agent was not preserved: %q", link.Header.Get("User-Agent"))
	}
	if link.ContentLength != int64(len(ucUnlimitedTestContent)) {
		t.Fatalf("unexpected safe raw size %d", link.ContentLength)
	}
	if fallbackCalls.Load() != 1 {
		t.Fatalf("unexpected traditional fallback calls %d", fallbackCalls.Load())
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.traditionalCookies) != 1 ||
		mock.traditionalCookies[0] != "__puus=cdn-lease" {
		t.Fatalf("raw probe did not use only the CDN lease: %v", mock.traditionalCookies)
	}
	if len(mock.traditionalRanges) != 1 || mock.traditionalRanges[0] != "bytes=0-0" {
		t.Fatalf("raw probe did not validate Range support: %v", mock.traditionalRanges)
	}
}

func containsString(items []string, expected string) bool {
	for _, item := range items {
		if item == expected {
			return true
		}
	}
	return false
}

func TestUCUnlimitedInitAndListDoNotUseAccountCookie(t *testing.T) {
	mock := newUCUnlimitedMock(t)
	installUCUnlimitedTestKeeper(t, mock)
	originalCookie := Cookie
	Cookie = "user-account-secret"
	t.Cleanup(func() { Cookie = originalCookie })
	d := ucUnlimitedTestDriver(6, "share-list")
	d.OrderBy = "file_name"
	d.OrderDirection = "asc"

	if err := d.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	items, err := d.List(context.Background(), &model.Object{ID: "0", IsFolder: true}, model.ListArgs{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 || items[0].GetID() == "" {
		t.Fatalf("unexpected anonymous list result: %+v", items)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	for _, cookie := range mock.apiCookies {
		if strings.Contains(cookie, "user-account-secret") {
			t.Fatal("UC account Cookie was used by anonymous share init/list")
		}
	}
	if mock.saveCalls != 0 {
		t.Fatal("anonymous share listing called save")
	}
}

func TestUCUnlimitedResolveAndStrictRangeWithoutAccountCookieOrSave(t *testing.T) {
	mock := newUCUnlimitedMock(t)
	installUCUnlimitedTestKeeper(t, mock)
	d := ucUnlimitedTestDriver(7, "share-a")

	link, err := d.Link(context.Background(), ucUnlimitedTestFile(), model.LinkArgs{
		Header: http.Header{"Cookie": {"user-account-secret"}},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if link.URL != "" || link.RangeReader == nil || link.ContentLength != int64(len(ucUnlimitedTestContent)) {
		t.Fatalf("unexpected unlimited link: %+v", link)
	}
	body, err := link.RangeReader.RangeRead(context.Background(), http_range.Range{Start: 2, Length: 5})
	if err != nil {
		t.Fatalf("range read: %v", err)
	}
	payload, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = body.Close()
	if string(payload) != "23456" {
		t.Fatalf("unexpected range payload %q", payload)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.saveCalls != 0 {
		t.Fatalf("unlimited hit called save %d times", mock.saveCalls)
	}
	if mock.tokenCalls != 1 || mock.downloadCalls != 1 {
		t.Fatalf("unexpected API calls token=%d download=%d", mock.tokenCalls, mock.downloadCalls)
	}
	for _, cookie := range mock.apiCookies {
		if strings.Contains(cookie, "user-account-secret") {
			t.Fatal("user account Cookie leaked into UC unlimited API")
		}
	}
	for _, cookie := range mock.cdnCookies {
		if cookie != "" {
			t.Fatalf("anonymous Cookie leaked into CDN request: %q", cookie)
		}
	}
	for _, referer := range mock.cdnReferers {
		if !strings.HasPrefix(referer, mock.server.URL+"/cdn/") ||
			!strings.HasSuffix(referer, "\\") {
			t.Fatalf("UC anonymous CDN Referer was not preserved: %q", referer)
		}
	}
	if !strings.Contains(mock.downloadCookie["share-a"], "__puus=lease-share-a") {
		t.Fatalf("storage-scoped anonymous Cookie was not reused: %q", mock.downloadCookie["share-a"])
	}
}

func TestUCUnlimitedRefreshesAndReplaysSameRangeAfter403(t *testing.T) {
	mock := newUCUnlimitedMock(t)
	mock.oldFails = true
	installUCUnlimitedTestKeeper(t, mock)
	d := ucUnlimitedTestDriver(8, "share-refresh")

	link, err := d.Link(context.Background(), ucUnlimitedTestFile(), model.LinkArgs{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	requestRange := http_range.Range{Start: 10, Length: 7}
	body, err := link.RangeReader.RangeRead(context.Background(), requestRange)
	if err != nil {
		t.Fatalf("range read after refresh: %v", err)
	}
	payload, _ := io.ReadAll(body)
	_ = body.Close()
	if !bytes.Equal(payload, ucUnlimitedTestContent[10:17]) {
		t.Fatalf("range was not replayed correctly: %q", payload)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.downloadCalls < 2 || mock.detailCalls < 1 || mock.tokenCalls < 2 {
		t.Fatalf("refresh path incomplete: token=%d detail=%d download=%d",
			mock.tokenCalls, mock.detailCalls, mock.downloadCalls)
	}
	if mock.saveCalls != 0 {
		t.Fatal("successful refresh unexpectedly used traditional save")
	}
}

func TestUCUnlimitedRejectsNon206RangeProbe(t *testing.T) {
	mock := newUCUnlimitedMock(t)
	mock.strictFailure = true
	keeper := installUCUnlimitedTestKeeper(t, mock)
	params := ucUnlimitedResolveParams{
		key:      ucUnlimitedLeaseKey{storageID: 1, shareID: "share-probe"},
		driver:   ucUnlimitedTestDriver(1, "share-probe"),
		provider: mustUnlimitedProvider(ucUnlimitedTestDriver(1, "share-probe")),
	}
	_, err := keeper.probe(context.Background(), params, mock.server.URL+"/cdn/current",
		int64(len(ucUnlimitedTestContent)))
	if err == nil {
		t.Fatal("strict probe accepted HTTP 200 for a Range request")
	}
}

func TestUCUnlimitedConcurrentResolveUsesSingleflight(t *testing.T) {
	mock := newUCUnlimitedMock(t)
	installUCUnlimitedTestKeeper(t, mock)
	d := ucUnlimitedTestDriver(9, "share-concurrent")
	var wait sync.WaitGroup
	var failures atomic.Int64
	for range 24 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			link, err := d.Link(context.Background(), ucUnlimitedTestFile(), model.LinkArgs{})
			if err != nil || link == nil || link.RangeReader == nil {
				failures.Add(1)
			}
		}()
	}
	wait.Wait()
	if failures.Load() != 0 {
		t.Fatalf("concurrent resolves failed: %d", failures.Load())
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.tokenCalls != 1 || mock.downloadCalls != 1 {
		t.Fatalf("singleflight did not merge calls: token=%d download=%d",
			mock.tokenCalls, mock.downloadCalls)
	}
}

func TestUCUnlimitedMaintenanceRefreshesActiveLeaseBeforeExpiry(t *testing.T) {
	mock := newUCUnlimitedMock(t)
	keeper := installUCUnlimitedTestKeeper(t, mock)
	baseTime := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	currentTime := baseTime
	keeper.now = func() time.Time { return currentTime }
	if _, err := itemDriverLink(10, "share-maintain"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	currentTime = baseTime.Add(16 * time.Minute)
	keeper.maintain(context.Background())
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.downloadCalls != 2 {
		t.Fatalf("proactive maintenance did not refresh URL: download calls=%d", mock.downloadCalls)
	}
	if mock.detailCalls != 0 {
		t.Fatalf("healthy token refresh unnecessarily scanned metadata: detail calls=%d", mock.detailCalls)
	}
}

func TestUCUnlimitedAnonymousCookieIsIsolatedByStorage(t *testing.T) {
	mock := newUCUnlimitedMock(t)
	installUCUnlimitedTestKeeper(t, mock)
	for _, item := range []struct {
		storageID uint
		shareID   string
	}{
		{storageID: 11, shareID: "share-one"},
		{storageID: 12, shareID: "share-two"},
	} {
		if _, err := itemDriverLink(item.storageID, item.shareID); err != nil {
			t.Fatalf("resolve %s: %v", item.shareID, err)
		}
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if got := mock.downloadCookie["share-one"]; !strings.Contains(got, "lease-share-one") ||
		strings.Contains(got, "lease-share-two") {
		t.Fatalf("storage one Cookie contamination: %q", got)
	}
	if got := mock.downloadCookie["share-two"]; !strings.Contains(got, "lease-share-two") ||
		strings.Contains(got, "lease-share-one") {
		t.Fatalf("storage two Cookie contamination: %q", got)
	}
}

func itemDriverLink(storageID uint, shareID string) (*model.Link, error) {
	return ucUnlimitedTestDriver(storageID, shareID).Link(
		context.Background(), ucUnlimitedTestFile(), model.LinkArgs{})
}

type capturedRangeReader struct {
	ctxValue any
	got      http_range.Range
}

func (r *capturedRangeReader) RangeRead(ctx context.Context, requestRange http_range.Range) (io.ReadCloser, error) {
	r.ctxValue = ctx.Value("request")
	r.got = requestRange
	return io.NopCloser(bytes.NewReader(ucUnlimitedTestContent[requestRange.Start : requestRange.Start+requestRange.Length])), nil
}

func TestUCUnlimitedFallbackUsesCurrentContextAndSameRange(t *testing.T) {
	mock := newUCUnlimitedMock(t)
	mock.apiFailure = true
	keeper := installUCUnlimitedTestKeeper(t, mock)
	fallbackReader := &capturedRangeReader{}
	key := ucUnlimitedLeaseKey{storageID: 20, shareID: "share-fallback", fileID: "fid"}
	driver := ucUnlimitedTestDriver(20, "share-fallback")
	lease := &ucUnlimitedLease{
		keeper: keeper,
		params: ucUnlimitedResolveParams{
			key:      key,
			driver:   driver,
			provider: mustUnlimitedProvider(driver),
			size:     int64(len(ucUnlimitedTestContent)),
			fallback: func(context.Context) (*model.Link, error) {
				return &model.Link{RangeReader: fallbackReader}, nil
			},
		},
		snapshot: ucUnlimitedSnapshot{
			url:        mock.server.URL + "/missing",
			size:       int64(len(ucUnlimitedTestContent)),
			refreshAt:  time.Now().Add(time.Hour),
			generation: 1,
		},
		lastAccess: time.Now(),
	}
	requestRange := http_range.Range{Start: 4, Length: 6}
	ctx := context.WithValue(context.Background(), "request", "current-context")
	body, err := (&ucUnlimitedRangeReader{lease: lease}).RangeRead(ctx, requestRange)
	if err != nil {
		t.Fatalf("fallback read: %v", err)
	}
	_, _ = io.ReadAll(body)
	_ = body.Close()
	if fallbackReader.ctxValue != "current-context" {
		t.Fatalf("fallback did not use current RangeRead context: %v", fallbackReader.ctxValue)
	}
	if fallbackReader.got != requestRange {
		t.Fatalf("fallback range changed: got %+v want %+v", fallbackReader.got, requestRange)
	}
}

func TestUCUnlimitedDropIsStorageScoped(t *testing.T) {
	mock := newUCUnlimitedMock(t)
	keeper := installUCUnlimitedTestKeeper(t, mock)
	first, err := itemDriverLink(31, "share-drop-one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := itemDriverLink(32, "share-drop-two")
	if err != nil {
		t.Fatal(err)
	}
	keeper.DropStorage(31)
	if _, err := first.RangeReader.RangeRead(context.Background(),
		http_range.Range{Start: 1, Length: 1}); err == nil {
		t.Fatal("dropped storage lease remained readable")
	}
	body, err := second.RangeReader.RangeRead(context.Background(),
		http_range.Range{Start: 1, Length: 1})
	if err != nil {
		t.Fatalf("other storage was affected by Drop: %v", err)
	}
	_ = body.Close()
}
