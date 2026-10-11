package baidu_share

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/drivers/baidu_netdisk"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

func TestBaiduPlaybackMissingMetadataStopsBeforeResolverAndCache(t *testing.T) {
	original := resolveBaiduShareLink
	resolveBaiduShareLink = func(context.Context, *BaiduShare2, model.Obj, model.LinkArgs) (*model.Link, error) {
		t.Fatal("must not resolve incomplete playback metadata")
		return nil, nil
	}
	t.Cleanup(func() { resolveBaiduShareLink = original })
	file := &model.Object{ID: "missing-metadata-file", Name: "video.mp4"}
	baiduShareLinkCache.Delete(file.ID)
	t.Cleanup(func() { baiduShareLinkCache.Delete(file.ID) })
	baiduShareLinkCache.Set(file.ID, &model.Link{URL: "https://cdn.example/cached"})
	for _, fields := range [][2]string{{"", ""}, {"sid", ""}, {"", "uk"}} {
		d := &BaiduShare2{
			Addition: Addition{AccountStorageID: 1},
			ShareId:  fields[0], ShareUk: fields[1],
		}
		link, err := d.Link(context.Background(), file, model.LinkArgs{})
		if link != nil || err == nil || !strings.Contains(err.Error(), "[baidu_share_incomplete]") {
			t.Fatalf("expected safe metadata error, got link=%v err=%v", link, err)
		}
	}
}

func TestBaiduPlaybackCompleteMetadataKeepsNormalResolver(t *testing.T) {
	original := resolveBaiduShareLink
	calls := 0
	resolveBaiduShareLink = func(context.Context, *BaiduShare2, model.Obj, model.LinkArgs) (*model.Link, error) {
		calls++
		return &model.Link{URL: "https://cdn.example/complete"}, nil
	}
	t.Cleanup(func() { resolveBaiduShareLink = original })
	d := &BaiduShare2{Addition: Addition{AccountStorageID: 1}, ShareId: "sid", ShareUk: "uk"}
	file := &model.Object{ID: "complete-metadata-file"}
	baiduShareLinkCache.Delete(file.ID)
	t.Cleanup(func() { baiduShareLinkCache.Delete(file.ID) })
	link, err := d.Link(context.Background(), file, model.LinkArgs{})
	if err != nil || link == nil || calls != 1 {
		t.Fatalf("normal resolution changed: link=%v err=%v calls=%d", link, err, calls)
	}
}

func TestBaiduPlaybackMissingFileIsNotShareRecovery(t *testing.T) {
	d := &BaiduShare2{Addition: Addition{AccountStorageID: 1}}
	_, err := d.Link(context.Background(), &model.Object{}, model.LinkArgs{})
	if err == nil || !strings.Contains(err.Error(), "[baidu_file_missing]") {
		t.Fatalf("expected file error, got %v", err)
	}
}

func TestBaiduPlaybackIncompleteNativeNeverTransfers(t *testing.T) {
	d := &BaiduShare2{ShareId: "sid"}
	// A nil account/client deliberately fails if validation reaches transfer I/O.
	_, err := d.saveFile("fid", nil)
	if err == nil || !strings.Contains(err.Error(), "[baidu_share_incomplete]") {
		t.Fatalf("expected metadata error, got %v", err)
	}
}

func TestBaiduPlaybackPinnedIncompleteDoesNotRepeatValidate(t *testing.T) {
	original := validateBaiduShare
	validateBaiduShare = func(*BaiduShare2) error {
		t.Fatal("MV owns the single startup recovery budget")
		return nil
	}
	t.Cleanup(func() { validateBaiduShare = original })
	d := &BaiduShare2{Addition: Addition{AccountStorageID: 1}}
	_, err := d.unlimitedLink(context.Background(), &model.Object{ID: "fid"}, &baidu_netdisk.BaiduNetdisk{})
	if err == nil || !strings.Contains(err.Error(), "[baidu_share_incomplete]") {
		t.Fatalf("expected metadata error, got %v", err)
	}
}

func TestBaiduPlaybackLoginClassificationRequiresExplicitErrno(t *testing.T) {
	err := (&baiduDlnaAPIError{errno: -6}).Error()
	if !strings.Contains(err, "[baidu_login_invalid]") {
		t.Fatalf("explicit login rejection not classified: %s", err)
	}
	for _, errno := range []int64{-9, -62, 2} {
		if strings.Contains((&baiduDlnaAPIError{errno: errno}).Error(), "[baidu_login_invalid]") {
			t.Fatalf("misclassified errno %d as account invalid", errno)
		}
	}
	client := baiduRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return baiduJSONResponse(http.StatusOK, `<html>unexpected response</html>`), nil
	})
	_, uidErr := fetchBaiduAccountUID(context.Background(), client, "BDUSS=synthetic")
	if uidErr == nil || strings.Contains(uidErr.Error(), "Cookie") || strings.Contains(uidErr.Error(), "CK") {
		t.Fatalf("missing UID must not diagnose an expired Cookie: %v", uidErr)
	}
}
