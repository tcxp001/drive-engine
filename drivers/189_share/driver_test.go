package _189_share

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	_189pc "github.com/OpenListTeam/OpenList/v4/drivers/189pc"
	"github.com/OpenListTeam/OpenList/v4/internal/cache"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

func TestCloud189ShareLink_CachesByFileID(t *testing.T) {
	origCache := cloud189ShareLinkCache
	origResolver := resolveCloud189ShareLink
	cloud189ShareLinkCache = cache.NewKeyedCache[*model.Link](time.Hour)
	resolveCalls := 0
	resolveCloud189ShareLink = func(ctx context.Context, d *Cloud189Share, file model.Obj) (*model.Link, error) {
		resolveCalls++
		return &model.Link{URL: "https://example.com/189/" + file.GetID()}, nil
	}
	t.Cleanup(func() {
		cloud189ShareLinkCache = origCache
		resolveCloud189ShareLink = origResolver
	})

	d := &Cloud189Share{}
	file := &FileObj{ObjThumb: model.ObjThumb{Object: model.Object{ID: "file-1", Name: "video.mp4"}}}

	first, err := d.Link(context.Background(), file, model.LinkArgs{})
	if err != nil {
		t.Fatalf("first link: %v", err)
	}
	second, err := d.Link(context.Background(), file, model.LinkArgs{Type: "ignored"})
	if err != nil {
		t.Fatalf("second link: %v", err)
	}
	if first.URL != second.URL {
		t.Fatalf("expected cached URL reuse, got %q and %q", first.URL, second.URL)
	}
	if resolveCalls != 1 {
		t.Fatalf("expected resolver once, got %d", resolveCalls)
	}
}

func TestCloud189ShareLink_UsesDifferentKeysForDifferentFiles(t *testing.T) {
	origCache := cloud189ShareLinkCache
	origResolver := resolveCloud189ShareLink
	cloud189ShareLinkCache = cache.NewKeyedCache[*model.Link](time.Hour)
	resolveCalls := 0
	resolveCloud189ShareLink = func(ctx context.Context, d *Cloud189Share, file model.Obj) (*model.Link, error) {
		resolveCalls++
		return &model.Link{URL: "https://example.com/189/" + file.GetID()}, nil
	}
	t.Cleanup(func() {
		cloud189ShareLinkCache = origCache
		resolveCloud189ShareLink = origResolver
	})

	d := &Cloud189Share{}
	file1 := &FileObj{ObjThumb: model.ObjThumb{Object: model.Object{ID: "file-1", Name: "a.mp4"}}}
	file2 := &FileObj{ObjThumb: model.ObjThumb{Object: model.Object{ID: "file-2", Name: "b.mp4"}}}

	_, _ = d.Link(context.Background(), file1, model.LinkArgs{})
	_, _ = d.Link(context.Background(), file2, model.LinkArgs{})
	if resolveCalls != 2 {
		t.Fatalf("expected resolver twice for different file IDs, got %d", resolveCalls)
	}
}

func TestCloud189ShareLink_SameFileIDInDifferentSharesDoesNotShareCache(t *testing.T) {
	origCache := cloud189ShareLinkCache
	origResolver := resolveCloud189ShareLink
	cloud189ShareLinkCache = cache.NewKeyedCache[*model.Link](time.Hour)
	resolveCalls := 0
	resolveCloud189ShareLink = func(ctx context.Context, d *Cloud189Share, file model.Obj) (*model.Link, error) {
		resolveCalls++
		return &model.Link{URL: "https://example.com/189/" + d.ShareId + "/" + file.GetID()}, nil
	}
	t.Cleanup(func() {
		cloud189ShareLinkCache = origCache
		resolveCloud189ShareLink = origResolver
	})

	file := &FileObj{ObjThumb: model.ObjThumb{Object: model.Object{ID: "file-1", Name: "video.mp4"}}}
	_, _ = (&Cloud189Share{Addition: Addition{ShareId: "share-a"}}).Link(context.Background(), file, model.LinkArgs{})
	_, _ = (&Cloud189Share{Addition: Addition{ShareId: "share-b"}}).Link(context.Background(), file, model.LinkArgs{})
	if resolveCalls != 2 {
		t.Fatalf("expected resolver twice for different shares, got %d", resolveCalls)
	}
}

func TestCloud189ShareLink_RefreshesAfterShortReuseWindow(t *testing.T) {
	origCache := cloud189ShareLinkCache
	origTTL := cloud189ShareLinkCacheTTL
	origResolver := resolveCloud189ShareLink
	cloud189ShareLinkCache = cache.NewKeyedCache[*model.Link](time.Hour)
	cloud189ShareLinkCacheTTL = 10 * time.Millisecond
	resolveCalls := 0
	resolveCloud189ShareLink = func(ctx context.Context, d *Cloud189Share, file model.Obj) (*model.Link, error) {
		resolveCalls++
		return &model.Link{URL: "https://example.com/189/" + file.GetID()}, nil
	}
	t.Cleanup(func() {
		cloud189ShareLinkCache = origCache
		cloud189ShareLinkCacheTTL = origTTL
		resolveCloud189ShareLink = origResolver
	})

	d := &Cloud189Share{Addition: Addition{ShareId: "share-a"}}
	file := &FileObj{ObjThumb: model.ObjThumb{Object: model.Object{ID: "file-1", Name: "video.mp4"}}}
	_, _ = d.Link(context.Background(), file, model.LinkArgs{})
	time.Sleep(20 * time.Millisecond)
	_, _ = d.Link(context.Background(), file, model.LinkArgs{})
	if resolveCalls != 2 {
		t.Fatalf("expected resolver refresh after cache expiry, got %d calls", resolveCalls)
	}
}

func TestCloud189ShareLink_DoesNotCacheErrors(t *testing.T) {
	origCache := cloud189ShareLinkCache
	origResolver := resolveCloud189ShareLink
	cloud189ShareLinkCache = cache.NewKeyedCache[*model.Link](time.Hour)
	resolveCalls := 0
	resolveCloud189ShareLink = func(ctx context.Context, d *Cloud189Share, file model.Obj) (*model.Link, error) {
		resolveCalls++
		return nil, errors.New("boom")
	}
	t.Cleanup(func() {
		cloud189ShareLinkCache = origCache
		resolveCloud189ShareLink = origResolver
	})

	d := &Cloud189Share{}
	file := &FileObj{ObjThumb: model.ObjThumb{Object: model.Object{ID: "file-1", Name: "video.mp4"}}}

	_, _ = d.Link(context.Background(), file, model.LinkArgs{})
	_, _ = d.Link(context.Background(), file, model.LinkArgs{})
	if resolveCalls != 2 {
		t.Fatalf("expected resolver twice after errors, got %d", resolveCalls)
	}
}

func TestApplyCloud189NativePlaybackHeaderAddsRefererWithoutDroppingUserAgent(t *testing.T) {
	link := &model.Link{
		URL: "https://cdn.example/video",
		Header: http.Header{
			"User-Agent": []string{"account-driver-agent"},
		},
	}

	result := applyCloud189NativePlaybackHeader(link)

	if got := result.Header.Get("Referer"); got != "https://cloud.189.cn/" {
		t.Fatalf("Referer = %q", got)
	}
	if got := result.Header.Get("User-Agent"); got != "account-driver-agent" {
		t.Fatalf("User-Agent = %q", got)
	}
}

func TestResolveCloud189VideoPreviewUsesTransferForMimeTypeZero(t *testing.T) {
	originalTransfer := transferCloud189ShareFile
	originalRoutes := resolveCloud189ShareVideoRoutes
	transferCalls := 0
	transferCloud189ShareFile = func(
		_ context.Context,
		_ *_189pc.Cloud189PC,
		shareID int,
		fileID string,
		fileName string,
	) (*model.Link, error) {
		transferCalls++
		if shareID != 123 || fileID != "file-id" || fileName != "old-name.mkv" {
			t.Fatalf(
				"unexpected transfer arguments: %d %q %q",
				shareID,
				fileID,
				fileName,
			)
		}
		expiry := 30 * time.Minute
		return &model.Link{
			URL:        "https://cdn.example/transferred-raw",
			Expiration: &expiry,
			Header: http.Header{
				"User-Agent": {"account-agent"},
			},
		}, nil
	}
	resolveCloud189ShareVideoRoutes = func(
		_ context.Context,
		_ *_189pc.Cloud189PC,
		_ int,
		_ model.Obj,
	) ([]_189pc.ShareVideoRoute, error) {
		t.Fatal("mimeType=0 unexpectedly requested video routes")
		return nil, nil
	}
	t.Cleanup(func() {
		transferCloud189ShareFile = originalTransfer
		resolveCloud189ShareVideoRoutes = originalRoutes
	})

	before := time.Now().Add(30 * time.Minute)
	result, err := resolveCloud189VideoPreview(
		context.Background(),
		&_189pc.Cloud189PC{},
		123,
		&FileObj{
			ObjThumb: model.ObjThumb{
				Object: model.Object{
					ID:   "file-id",
					Name: "video.mkv",
				},
			},
			oldName:  "old-name.mkv",
			mimeType: String("0"),
		},
	)
	after := time.Now().Add(30 * time.Minute)
	if err != nil {
		t.Fatalf("resolveCloud189VideoPreview() error = %v", err)
	}
	if transferCalls != 1 {
		t.Fatalf("transfer calls = %d, want 1", transferCalls)
	}
	if len(result.Routes) != 1 {
		t.Fatalf("route count = %d, want 1", len(result.Routes))
	}
	route := result.Routes[0]
	if route.Name != "RAW" ||
		route.URL != "https://cdn.example/transferred-raw" ||
		!route.Original {
		t.Fatalf("transfer route = %#v", route)
	}
	if route.Expire.Before(before) || route.Expire.After(after) {
		t.Fatalf("route expiry = %v, want between %v and %v", route.Expire, before, after)
	}
	if result.Header["User-Agent"] != "account-agent" {
		t.Fatalf("User-Agent = %q", result.Header["User-Agent"])
	}
	if result.Header["Referer"] != "https://cloud.189.cn/" {
		t.Fatalf("Referer = %q", result.Header["Referer"])
	}
}

func TestResolveCloud189VideoPreviewDoesNotHidePreviewErrors(t *testing.T) {
	originalTransfer := transferCloud189ShareFile
	originalRoutes := resolveCloud189ShareVideoRoutes
	sentinel := errors.New("preview unavailable")
	transferCloud189ShareFile = func(
		_ context.Context,
		_ *_189pc.Cloud189PC,
		_ int,
		_ string,
		_ string,
	) (*model.Link, error) {
		t.Fatal("normal video unexpectedly used transfer fallback")
		return nil, nil
	}
	resolveCloud189ShareVideoRoutes = func(
		_ context.Context,
		_ *_189pc.Cloud189PC,
		_ int,
		_ model.Obj,
	) ([]_189pc.ShareVideoRoute, error) {
		return nil, sentinel
	}
	t.Cleanup(func() {
		transferCloud189ShareFile = originalTransfer
		resolveCloud189ShareVideoRoutes = originalRoutes
	})

	_, err := resolveCloud189VideoPreview(
		context.Background(),
		&_189pc.Cloud189PC{},
		123,
		&FileObj{
			ObjThumb: model.ObjThumb{
				Object: model.Object{
					ID:   "file-id",
					Name: "video.mkv",
				},
			},
			mimeType: String("video/mp4"),
		},
	)

	if !errors.Is(err, sentinel) {
		t.Fatalf("expected preview error, got %v", err)
	}
}
