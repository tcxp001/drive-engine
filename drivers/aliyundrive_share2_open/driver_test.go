package aliyundrive_share2_open

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/cache"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

func TestAliyundriveShare2OpenLink_CachesByFileIDAndAliTo115State(t *testing.T) {
	origCache := aliyundriveShareLinkCache
	origResolver := resolveAliyundriveShareLink
	origAliTo115 := aliyundriveShareAliTo115Enabled
	aliyundriveShareLinkCache = cache.NewKeyedCache[*model.Link](time.Hour)
	resolveCalls := 0
	resolveAliyundriveShareLink = func(ctx context.Context, d *AliyundriveShare2Open, file model.Obj, args model.LinkArgs) (*model.Link, error) {
		resolveCalls++
		return &model.Link{URL: "https://example.com/aliyun/" + d.linkCacheKey(file.GetID())}, nil
	}
	t.Cleanup(func() {
		aliyundriveShareLinkCache = origCache
		resolveAliyundriveShareLink = origResolver
		aliyundriveShareAliTo115Enabled = origAliTo115
	})

	d := &AliyundriveShare2Open{}
	file := &model.Object{ID: "file-1", Name: "video.mp4"}

	aliyundriveShareAliTo115Enabled = func() bool { return false }
	first, err := d.Link(context.Background(), file, model.LinkArgs{})
	if err != nil {
		t.Fatalf("first link: %v", err)
	}
	second, err := d.Link(context.Background(), file, model.LinkArgs{})
	if err != nil {
		t.Fatalf("second link: %v", err)
	}

	aliyundriveShareAliTo115Enabled = func() bool { return true }
	third, err := d.Link(context.Background(), file, model.LinkArgs{})
	if err != nil {
		t.Fatalf("third link: %v", err)
	}
	fourth, err := d.Link(context.Background(), file, model.LinkArgs{})
	if err != nil {
		t.Fatalf("fourth link: %v", err)
	}

	if first.URL != second.URL {
		t.Fatalf("expected cached false-state URL reuse, got %q and %q", first.URL, second.URL)
	}
	if third.URL != fourth.URL {
		t.Fatalf("expected cached true-state URL reuse, got %q and %q", third.URL, fourth.URL)
	}
	if first.URL == third.URL {
		t.Fatalf("expected different cache buckets for AliTo115 states, got %q", first.URL)
	}
	if resolveCalls != 2 {
		t.Fatalf("expected resolver twice for two setting states, got %d", resolveCalls)
	}
}

func TestAliyundriveShare2OpenLink_DoesNotCacheErrors(t *testing.T) {
	origCache := aliyundriveShareLinkCache
	origResolver := resolveAliyundriveShareLink
	origAliTo115 := aliyundriveShareAliTo115Enabled
	aliyundriveShareLinkCache = cache.NewKeyedCache[*model.Link](time.Hour)
	resolveCalls := 0
	resolveAliyundriveShareLink = func(ctx context.Context, d *AliyundriveShare2Open, file model.Obj, args model.LinkArgs) (*model.Link, error) {
		resolveCalls++
		return nil, errors.New("boom")
	}
	t.Cleanup(func() {
		aliyundriveShareLinkCache = origCache
		resolveAliyundriveShareLink = origResolver
		aliyundriveShareAliTo115Enabled = origAliTo115
	})

	aliyundriveShareAliTo115Enabled = func() bool { return false }
	d := &AliyundriveShare2Open{}
	file := &model.Object{ID: "file-1", Name: "video.mp4"}

	_, _ = d.Link(context.Background(), file, model.LinkArgs{})
	_, _ = d.Link(context.Background(), file, model.LinkArgs{})
	if resolveCalls != 2 {
		t.Fatalf("expected resolver twice after errors, got %d", resolveCalls)
	}
}

func TestBuildAliNativePlaybackResultPreservesRawWhenPreviewUnavailable(
	t *testing.T,
) {
	rawTTL := 90 * time.Minute
	before := time.Now().Add(rawTTL)
	result := buildAliNativePlaybackResult(
		&model.Link{
			URL:        "https://cdn.example/raw",
			Expiration: &rawTTL,
			Header: http.Header{
				"Cookie":     {"lease=a", "session=b"},
				"Referer":    {"https://www.alipan.com/"},
				"User-Agent": {"account-agent"},
			},
		},
		nil,
		time.Time{},
	)
	after := time.Now().Add(rawTTL)

	if len(result.Routes) != 1 {
		t.Fatalf("route count = %d, want 1", len(result.Routes))
	}
	raw := result.Routes[0]
	if raw.Name != "RAW" ||
		raw.URL != "https://cdn.example/raw" ||
		!raw.Original {
		t.Fatalf("RAW route = %#v", raw)
	}
	if raw.Expire.Before(before) || raw.Expire.After(after) {
		t.Fatalf("RAW expiry = %v, want between %v and %v", raw.Expire, before, after)
	}
	if raw.Header["Cookie"] != "lease=a; session=b" {
		t.Fatalf("RAW Cookie header = %q", raw.Header["Cookie"])
	}
	if raw.Header["Referer"] != "https://www.alipan.com/" {
		t.Fatalf("RAW Referer = %q", raw.Header["Referer"])
	}
	if raw.Header["User-Agent"] != "account-agent" {
		t.Fatalf("RAW User-Agent = %q", raw.Header["User-Agent"])
	}
}

func TestBuildAliNativePlaybackResultFiltersSortsAndNamesFinishedPreview(
	t *testing.T,
) {
	previewExpire := time.Now().Add(4 * time.Hour).UTC().Truncate(time.Second)
	preview := &VideoPreviewResponse{
		PlayInfo: VideoPreviewPlayInfo{
			Videos: []LiveTranscoding{
				{
					TemplateId:     "1080P",
					TemplateHeight: 1080,
					Status:         "finished",
					Url:            "https://cdn.example/1080p",
				},
				{
					TemplateId:     "4K",
					TemplateHeight: 2160,
					Status:         "finished",
					Url:            "https://cdn.example/4k",
				},
				{
					TemplateId:     "running",
					TemplateHeight: 4320,
					Status:         "running",
					Url:            "https://cdn.example/running",
				},
				{
					TemplateId:     "4K",
					TemplateHeight: 1440,
					Status:         "finished",
					Url:            "https://cdn.example/duplicate",
				},
				{
					TemplateId:     "",
					TemplateHeight: 720,
					Status:         "finished",
					Url:            "https://cdn.example/no-name",
				},
			},
		},
	}

	result := buildAliNativePlaybackResult(
		&model.Link{URL: "https://cdn.example/raw"},
		preview,
		previewExpire,
	)

	if len(result.Routes) != 3 {
		t.Fatalf("route count = %d, want 3", len(result.Routes))
	}
	wantNames := []string{"RAW", "4K", "1080P"}
	wantURLs := []string{
		"https://cdn.example/raw",
		"https://cdn.example/4k",
		"https://cdn.example/1080p",
	}
	for index := range wantNames {
		if result.Routes[index].Name != wantNames[index] ||
			result.Routes[index].URL != wantURLs[index] {
			t.Fatalf("route[%d] = %#v", index, result.Routes[index])
		}
	}
	for _, route := range result.Routes[1:] {
		if !route.Expire.Equal(previewExpire) {
			t.Fatalf("preview expiry = %v, want %v", route.Expire, previewExpire)
		}
		if route.Header["Referer"] != "https://www.alipan.com/" {
			t.Fatalf("preview Referer = %q", route.Header["Referer"])
		}
	}
}
