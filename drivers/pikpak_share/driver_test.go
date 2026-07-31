package pikpak_share

import (
	"context"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/pikpak"
	"github.com/OpenListTeam/OpenList/v4/internal/cache"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

func TestBuildAccountPlaybackResultPreservesRoutesHeadersAndExpiry(t *testing.T) {
	rawExpire := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	firstExpire := time.Now().Add(5 * time.Minute).UTC().Truncate(time.Second)
	secondExpire := time.Now().Add(7 * time.Minute).UTC().Truncate(time.Second)
	info := &pikpak.File{
		WebContentLink: "https://cdn.example/web-content",
		Links: map[string]pikpak.MediaLink{
			"application/octet-stream": {
				Url:    "https://cdn.example/raw",
				Expire: rawExpire,
			},
		},
		Medias: []pikpak.Media{
			{
				MediaName: "超清",
				Link: pikpak.MediaLink{
					Url:    "https://cdn.example/super",
					Expire: firstExpire,
				},
			},
			{
				ResolutionName: "1080P",
				Link: pikpak.MediaLink{
					Url:    "https://cdn.example/1080p",
					Expire: secondExpire,
				},
			},
			{
				Link: pikpak.MediaLink{
					Url: "https://cdn.example/other",
				},
			},
			{
				MediaName: "empty-url-is-filtered",
			},
		},
	}

	result, err := buildAccountPlaybackResult(
		info,
		"account-specific-agent",
		"account-device-id",
	)
	if err != nil {
		t.Fatalf("buildAccountPlaybackResult() error = %v", err)
	}
	if len(result.Routes) != 4 {
		t.Fatalf("route count = %d, want 4", len(result.Routes))
	}
	wantNames := []string{"RAW", "超清", "1080P", "other"}
	wantURLs := []string{
		"https://cdn.example/raw",
		"https://cdn.example/super",
		"https://cdn.example/1080p",
		"https://cdn.example/other",
	}
	for index := range wantNames {
		if result.Routes[index].Name != wantNames[index] {
			t.Fatalf(
				"route[%d].Name = %q, want %q",
				index,
				result.Routes[index].Name,
				wantNames[index],
			)
		}
		if result.Routes[index].URL != wantURLs[index] {
			t.Fatalf(
				"route[%d].URL = %q, want %q",
				index,
				result.Routes[index].URL,
				wantURLs[index],
			)
		}
	}
	if !result.Routes[0].Original {
		t.Fatal("RAW route is not marked original")
	}
	if !result.Routes[0].Expire.Equal(rawExpire) {
		t.Fatalf("RAW expiry = %v, want %v", result.Routes[0].Expire, rawExpire)
	}
	if !result.Routes[1].Expire.Equal(firstExpire) {
		t.Fatalf(
			"first media expiry = %v, want %v",
			result.Routes[1].Expire,
			firstExpire,
		)
	}
	if !result.Routes[2].Expire.Equal(secondExpire) {
		t.Fatalf(
			"second media expiry = %v, want %v",
			result.Routes[2].Expire,
			secondExpire,
		)
	}
	if result.Header["user-agent"] != "account-specific-agent" {
		t.Fatalf("user-agent = %q", result.Header["user-agent"])
	}
	if result.Header["x-device-id"] != "account-device-id" {
		t.Fatalf("x-device-id = %q", result.Header["x-device-id"])
	}
}

func TestBuildAccountPlaybackResultUsesWebContentRawFallback(t *testing.T) {
	result, err := buildAccountPlaybackResult(
		&pikpak.File{
			WebContentLink: "https://cdn.example/web-content",
			Links:          map[string]pikpak.MediaLink{},
		},
		"agent",
		"device",
	)
	if err != nil {
		t.Fatalf("buildAccountPlaybackResult() error = %v", err)
	}
	if len(result.Routes) != 1 ||
		result.Routes[0].Name != "RAW" ||
		result.Routes[0].URL != "https://cdn.example/web-content" ||
		!result.Routes[0].Original {
		t.Fatalf("routes = %#v", result.Routes)
	}
}

func TestRestoredFileReusesOneSavedIDAndCleanupLifetime(t *testing.T) {
	originalCache := restoredPikPakFiles
	originalRestore := restorePikPakShareFile
	originalSchedule := schedulePikPakAccountFileCleanup
	originalTTL := pikPakTemporaryFileTTL
	restoredPikPakFiles = cache.NewKeyedCache[restoredPikPakFile](24 * time.Hour)
	pikPakTemporaryFileTTL = func() time.Duration {
		return 42 * time.Second
	}
	restoreCalls := 0
	restorePikPakShareFile = func(
		_ context.Context,
		_ *pikpak.PikPak,
		shareID string,
		passCodeToken string,
		sourceFileID string,
		name string,
		size int64,
	) (string, error) {
		restoreCalls++
		if shareID != "share-id" ||
			passCodeToken != "pass-token" ||
			sourceFileID != "source-file" ||
			name != "video.mkv" ||
			size != 1024 {
			t.Fatalf(
				"unexpected restore arguments: %q %q %q %q %d",
				shareID,
				passCodeToken,
				sourceFileID,
				name,
				size,
			)
		}
		return "saved-file", nil
	}
	scheduleCalls := 0
	var scheduledDelay time.Duration
	schedulePikPakAccountFileCleanup = func(
		_ *PikPakShare,
		_ *pikpak.PikPak,
		fileID string,
		delay time.Duration,
	) {
		scheduleCalls++
		if fileID != "saved-file" {
			t.Fatalf("cleanup file id = %q", fileID)
		}
		scheduledDelay = delay
	}
	t.Cleanup(func() {
		restoredPikPakFiles = originalCache
		restorePikPakShareFile = originalRestore
		schedulePikPakAccountFileCleanup = originalSchedule
		pikPakTemporaryFileTTL = originalTTL
	})

	account := &pikpak.PikPak{
		Storage: model.Storage{ID: 42},
	}
	d := &PikPakShare{
		Addition: Addition{
			ShareId: "share-id",
		},
		PassCodeToken: "pass-token",
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
