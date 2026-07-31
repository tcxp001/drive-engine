package thunder_share

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/thunder_browser"
	"github.com/OpenListTeam/OpenList/v4/internal/cache"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

func TestThunderShareLink_CachesByFileID(t *testing.T) {
	origCache := thunderShareLinkCache
	origResolver := resolveThunderShareLink
	thunderShareLinkCache = cache.NewKeyedCache[*model.Link](time.Hour)
	resolveCalls := 0
	resolveThunderShareLink = func(ctx context.Context, d *ThunderShare, file model.Obj, args model.LinkArgs) (*model.Link, error) {
		resolveCalls++
		return &model.Link{URL: "https://example.com/thunder/" + file.GetID()}, nil
	}
	t.Cleanup(func() {
		thunderShareLinkCache = origCache
		resolveThunderShareLink = origResolver
	})

	d := &ThunderShare{}
	file := &model.Object{ID: "file-1", Name: "video.mp4"}

	_, _ = d.Link(context.Background(), file, model.LinkArgs{})
	_, _ = d.Link(context.Background(), file, model.LinkArgs{})
	if resolveCalls != 1 {
		t.Fatalf("expected resolver once, got %d", resolveCalls)
	}
}

func TestThunderShareLink_DifferentFileIDsDoNotShareCache(t *testing.T) {
	origCache := thunderShareLinkCache
	origResolver := resolveThunderShareLink
	thunderShareLinkCache = cache.NewKeyedCache[*model.Link](time.Hour)
	resolveCalls := 0
	resolveThunderShareLink = func(ctx context.Context, d *ThunderShare, file model.Obj, args model.LinkArgs) (*model.Link, error) {
		resolveCalls++
		return &model.Link{URL: "https://example.com/thunder/" + file.GetID()}, nil
	}
	t.Cleanup(func() {
		thunderShareLinkCache = origCache
		resolveThunderShareLink = origResolver
	})

	d := &ThunderShare{}
	_, _ = d.Link(context.Background(), &model.Object{ID: "file-1", Name: "a.mp4"}, model.LinkArgs{})
	_, _ = d.Link(context.Background(), &model.Object{ID: "file-2", Name: "b.mp4"}, model.LinkArgs{})
	if resolveCalls != 2 {
		t.Fatalf("expected resolver twice for different file IDs, got %d", resolveCalls)
	}
}

func TestThunderShareLink_SameFileIDInDifferentSharesDoesNotShareCache(t *testing.T) {
	origCache := thunderShareLinkCache
	origResolver := resolveThunderShareLink
	thunderShareLinkCache = cache.NewKeyedCache[*model.Link](time.Hour)
	resolveCalls := 0
	resolveThunderShareLink = func(ctx context.Context, d *ThunderShare, file model.Obj, args model.LinkArgs) (*model.Link, error) {
		resolveCalls++
		return &model.Link{URL: "https://example.com/thunder/" + d.ShareId + "/" + file.GetID()}, nil
	}
	t.Cleanup(func() {
		thunderShareLinkCache = origCache
		resolveThunderShareLink = origResolver
	})

	file := &model.Object{ID: "file-1", Name: "video.mp4"}
	_, _ = (&ThunderShare{Addition: Addition{ShareId: "share-a"}}).Link(context.Background(), file, model.LinkArgs{})
	_, _ = (&ThunderShare{Addition: Addition{ShareId: "share-b"}}).Link(context.Background(), file, model.LinkArgs{})
	if resolveCalls != 2 {
		t.Fatalf("expected resolver twice for different shares, got %d", resolveCalls)
	}
}

func TestThunderShareLink_RefreshesAfterShortReuseWindow(t *testing.T) {
	origCache := thunderShareLinkCache
	origTTL := thunderShareLinkCacheTTL
	origResolver := resolveThunderShareLink
	thunderShareLinkCache = cache.NewKeyedCache[*model.Link](time.Hour)
	thunderShareLinkCacheTTL = 10 * time.Millisecond
	resolveCalls := 0
	resolveThunderShareLink = func(ctx context.Context, d *ThunderShare, file model.Obj, args model.LinkArgs) (*model.Link, error) {
		resolveCalls++
		return &model.Link{URL: "https://example.com/thunder/" + file.GetID()}, nil
	}
	t.Cleanup(func() {
		thunderShareLinkCache = origCache
		thunderShareLinkCacheTTL = origTTL
		resolveThunderShareLink = origResolver
	})

	d := &ThunderShare{Addition: Addition{ShareId: "share-a"}}
	file := &model.Object{ID: "file-1", Name: "video.mp4"}
	_, _ = d.Link(context.Background(), file, model.LinkArgs{})
	time.Sleep(20 * time.Millisecond)
	_, _ = d.Link(context.Background(), file, model.LinkArgs{})
	if resolveCalls != 2 {
		t.Fatalf("expected resolver refresh after cache expiry, got %d calls", resolveCalls)
	}
}

func TestThunderShareLink_DoesNotCacheErrors(t *testing.T) {
	origCache := thunderShareLinkCache
	origResolver := resolveThunderShareLink
	thunderShareLinkCache = cache.NewKeyedCache[*model.Link](time.Hour)
	resolveCalls := 0
	resolveThunderShareLink = func(ctx context.Context, d *ThunderShare, file model.Obj, args model.LinkArgs) (*model.Link, error) {
		resolveCalls++
		return nil, errors.New("boom")
	}
	t.Cleanup(func() {
		thunderShareLinkCache = origCache
		resolveThunderShareLink = origResolver
	})

	d := &ThunderShare{}
	file := &model.Object{ID: "file-1", Name: "video.mp4"}

	_, _ = d.Link(context.Background(), file, model.LinkArgs{})
	_, _ = d.Link(context.Background(), file, model.LinkArgs{})
	if resolveCalls != 2 {
		t.Fatalf("expected resolver twice after errors, got %d", resolveCalls)
	}
}

func TestBuildVideoPreviewResultPreservesNativeOrderAndNames(t *testing.T) {
	rawExpire := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	firstExpire := time.Now().Add(5 * time.Minute).UTC().Truncate(time.Second)
	secondExpire := time.Now().Add(7 * time.Minute).UTC().Truncate(time.Second)
	file := &thunder_browser.Files{
		Links: map[string]thunder_browser.Link{
			"application/octet-stream": {
				URL:    "https://cdn.example/raw",
				Expire: rawExpire,
			},
		},
		Medias: []thunder_browser.Media{
			{
				ResolutionName: "4K",
				Link: thunder_browser.Link{
					URL:    "https://cdn.example/4k",
					Expire: firstExpire,
				},
			},
			{
				ResolutionName: "",
				Link: thunder_browser.Link{
					URL:    "https://cdn.example/other",
					Expire: secondExpire,
				},
			},
		},
	}

	result := buildVideoPreviewResult(file, "account-specific-agent")

	if len(result.Routes) != 3 {
		t.Fatalf("route count = %d, want 3", len(result.Routes))
	}
	if result.Routes[0].Name != "RAW" ||
		result.Routes[0].URL != file.Links["application/octet-stream"].URL ||
		!result.Routes[0].Original {
		t.Fatalf("raw route = %#v", result.Routes[0])
	}
	if !result.Routes[0].Expire.Equal(rawExpire) {
		t.Fatalf("raw expiry = %v, want %v", result.Routes[0].Expire, rawExpire)
	}
	if result.Routes[1].Name != "4K" {
		t.Fatalf("first media name = %q", result.Routes[1].Name)
	}
	if !result.Routes[1].Expire.Equal(firstExpire) {
		t.Fatalf(
			"first media expiry = %v, want %v",
			result.Routes[1].Expire,
			firstExpire,
		)
	}
	if result.Routes[2].Name != "other" {
		t.Fatalf("fallback media name = %q", result.Routes[2].Name)
	}
	if !result.Routes[2].Expire.Equal(secondExpire) {
		t.Fatalf(
			"second media expiry = %v, want %v",
			result.Routes[2].Expire,
			secondExpire,
		)
	}
	if result.Header["user-agent"] != "account-specific-agent" {
		t.Fatalf("user agent = %q", result.Header["user-agent"])
	}
}

func TestBuildVideoPreviewResultUsesWebContentLinkAsRawFallback(t *testing.T) {
	file := &thunder_browser.Files{
		WebContentLink: "https://cdn.example/raw-fallback",
		Medias: []thunder_browser.Media{
			{
				ResolutionName: "1080P",
				Link:           thunder_browser.Link{URL: "https://cdn.example/1080p"},
			},
		},
	}

	result := buildVideoPreviewResult(file, "")

	if len(result.Routes) != 2 {
		t.Fatalf("route count = %d, want 2", len(result.Routes))
	}
	if result.Routes[0].Name != "RAW" ||
		result.Routes[0].URL != file.WebContentLink ||
		!result.Routes[0].Original {
		t.Fatalf("route = %#v", result.Routes[0])
	}
	if result.Routes[1].Name != "1080P" {
		t.Fatalf("media route = %#v", result.Routes[1])
	}
	if result.Header["user-agent"] != thunder_browser.DownloadUserAgent {
		t.Fatalf("fallback user agent = %q", result.Header["user-agent"])
	}
}

func TestRestoredFileReusesOneSavedIDAndCleanupLifetime(t *testing.T) {
	originalCache := restoredThunderFiles
	originalRestore := restoreThunderShareFile
	originalSchedule := scheduleThunderShareCleanup
	originalTTL := thunderTemporaryFileTTL
	restoredThunderFiles = cache.NewKeyedCache[restoredThunderFile](24 * time.Hour)
	thunderTemporaryFileTTL = func() time.Duration {
		return 42 * time.Second
	}
	restoreCalls := 0
	restoreThunderShareFile = func(
		_ context.Context,
		_ *ThunderShare,
		account *thunder_browser.ThunderBrowser,
		file model.Obj,
	) (string, error) {
		restoreCalls++
		if account.ID != 42 ||
			file.GetID() != "source-file" ||
			file.GetName() != "video.mkv" ||
			file.GetSize() != 1024 {
			t.Fatalf("unexpected restore account/file: %#v %#v", account, file)
		}
		return "saved-file", nil
	}
	scheduleCalls := 0
	var scheduledDelay time.Duration
	scheduleThunderShareCleanup = func(
		_ *ThunderShare,
		cleanupContext context.Context,
		account *thunder_browser.ThunderBrowser,
		file model.Obj,
		delay time.Duration,
	) {
		scheduleCalls++
		if cleanupContext == nil {
			t.Fatal("cleanup context is nil")
		}
		if account.ID != 42 || file.GetID() != "saved-file" {
			t.Fatalf("unexpected cleanup account/file: %#v %#v", account, file)
		}
		scheduledDelay = delay
	}
	t.Cleanup(func() {
		restoredThunderFiles = originalCache
		restoreThunderShareFile = originalRestore
		scheduleThunderShareCleanup = originalSchedule
		thunderTemporaryFileTTL = originalTTL
	})

	account := &thunder_browser.ThunderBrowser{
		XunLeiBrowserCommon: &thunder_browser.XunLeiBrowserCommon{
			Common: &thunder_browser.Common{},
		},
		Storage: model.Storage{ID: 42},
	}
	d := &ThunderShare{
		Addition: Addition{
			ShareId: "share-id",
		},
	}
	file := &model.Object{
		ID:   "source-file",
		Name: "video.mkv",
		Size: 1024,
	}

	first, err := d.restoredFile(context.Background(), account, file)
	if err != nil {
		t.Fatalf("first restoredFile() error = %v", err)
	}
	second, err := d.restoredFile(context.Background(), account, file)
	if err != nil {
		t.Fatalf("second restoredFile() error = %v", err)
	}
	if first != "saved-file" || second != "saved-file" {
		t.Fatalf("saved ids = %q, %q", first, second)
	}
	if restoreCalls != 1 {
		t.Fatalf("restore calls = %d, want 1", restoreCalls)
	}
	if scheduleCalls != 1 {
		t.Fatalf("cleanup schedules = %d, want 1", scheduleCalls)
	}
	if scheduledDelay != 42*time.Second {
		t.Fatalf("cleanup delay = %v, want 42s", scheduledDelay)
	}
}

func TestThunderShareRestoreResponseUsesTraceFileMapping(t *testing.T) {
	response := thunderShareRestoreResponse{}
	response.Params.TraceFileIDs = `{"source-file":"saved-file"}`

	fileID, ok := response.RestoredFileID("source-file")

	if !ok || fileID != "saved-file" {
		t.Fatalf("restored mapping = %q, %v", fileID, ok)
	}
}
