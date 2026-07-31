package thunder_share

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/thunder_browser"
	"github.com/OpenListTeam/OpenList/v4/internal/cache"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	log "github.com/sirupsen/logrus"
)

type ThunderShare struct {
	model.Storage
	Addition
}

type videoPreviewRoute struct {
	Name     string    `json:"name"`
	URL      string    `json:"url"`
	Expire   time.Time `json:"expire,omitempty"`
	Original bool      `json:"original,omitempty"`
}

type videoPreviewResult struct {
	Routes []videoPreviewRoute `json:"routes"`
	Header map[string]string   `json:"header"`
}

func (d *ThunderShare) accountCount() int {
	if d.AccountStorageID > 0 {
		return 1
	}
	return op.GetDriverCount("ThunderBrowser")
}

func (d *ThunderShare) accountDriver(index int) driver.Driver {
	if d.AccountStorageID > 0 {
		return op.GetDriverByStorageID("ThunderBrowser", d.AccountStorageID)
	}
	return op.GetFirstDriver("ThunderBrowser", index)
}

type restoredThunderFile struct {
	FileID string
}

var restoredThunderFiles = cache.NewKeyedCache[restoredThunderFile](24 * time.Hour)
var restoredThunderFilesMu sync.Mutex

var restoreThunderShareFile = func(
	ctx context.Context,
	d *ThunderShare,
	thunder *thunder_browser.ThunderBrowser,
	file model.Obj,
) (string, error) {
	return d.saveFile(ctx, thunder, file)
}

var scheduleThunderShareCleanup = func(
	d *ThunderShare,
	ctx context.Context,
	thunder *thunder_browser.ThunderBrowser,
	file model.Obj,
	delay time.Duration,
) {
	go d.deleteFileAfter(ctx, thunder, file, delay)
}

var thunderTemporaryFileTTL = func() time.Duration {
	seconds := setting.GetInt(conf.DeleteDelayTime, 900)
	if seconds <= 0 {
		return 24 * time.Hour
	}
	return time.Duration(seconds) * time.Second
}

func (d *ThunderShare) restoredFile(
	ctx context.Context,
	thunder *thunder_browser.ThunderBrowser,
	file model.Obj,
) (string, error) {
	key := fmt.Sprintf("%s\x00%s\x00%d", d.ShareId, file.GetID(), thunder.ID)
	restoredThunderFilesMu.Lock()
	defer restoredThunderFilesMu.Unlock()
	if saved, ok := restoredThunderFiles.Get(key); ok && saved.FileID != "" {
		return saved.FileID, nil
	}
	fileID, err := restoreThunderShareFile(ctx, d, thunder, file)
	if err != nil {
		return "", err
	}
	ttl := thunderTemporaryFileTTL()
	restoredThunderFiles.SetWithTTL(key, restoredThunderFile{FileID: fileID}, ttl)
	scheduleThunderShareCleanup(
		d,
		thunderShareCleanupContext(ctx),
		thunder,
		&thunder_browser.Files{ID: fileID, Space: ""},
		ttl,
	)
	return fileID, nil
}

func buildVideoPreviewResult(
	file *thunder_browser.Files,
	downloadUserAgent string,
) videoPreviewResult {
	routes := make([]videoPreviewRoute, 0, len(file.Medias)+1)
	raw := file.Links["application/octet-stream"]
	rawURL := strings.TrimSpace(raw.URL)
	rawExpire := raw.Expire
	if rawURL == "" {
		rawURL = strings.TrimSpace(file.WebContentLink)
		if rawURL != "" {
			rawExpire = time.Now().Add(15 * time.Minute)
		}
	}
	if rawURL != "" {
		routes = append(routes, videoPreviewRoute{
			Name:     "RAW",
			URL:      rawURL,
			Expire:   rawExpire,
			Original: true,
		})
	}
	for _, media := range file.Medias {
		url := strings.TrimSpace(media.Link.URL)
		if url == "" {
			continue
		}
		name := strings.TrimSpace(media.ResolutionName)
		if name == "" {
			name = "other"
		}
		routes = append(routes, videoPreviewRoute{
			Name:   name,
			URL:    url,
			Expire: media.Link.Expire,
		})
	}
	userAgent := strings.TrimSpace(downloadUserAgent)
	if userAgent == "" {
		userAgent = thunder_browser.DownloadUserAgent
	}
	return videoPreviewResult{
		Routes: routes,
		Header: map[string]string{
			"user-agent": userAgent,
		},
	}
}

var thunderShareLinkCache = cache.NewKeyedCache[*model.Link](time.Hour)
var thunderShareLinkCacheTTL = 30 * time.Second

var resolveThunderShareLink = func(ctx context.Context, d *ThunderShare, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	count := d.accountCount()
	var lastErr error
	for i := 0; i < count; i++ {
		link, err := d.link(ctx, file, args)
		if err == nil {
			return link, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (d *ThunderShare) Config() driver.Config {
	return config
}

func (d *ThunderShare) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *ThunderShare) Init(ctx context.Context) error {
	if conf.LazyLoad && !conf.StoragesLoaded {
		return nil
	}

	return d.Validate()
}

func (d *ThunderShare) Drop(ctx context.Context) error {
	return nil
}

func (d *ThunderShare) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	files, err := d.listShareFiles(ctx, dir)
	if err != nil {
		log.Warnf("list Thunder files error: %v", err)
		return nil, err
	}
	return files, err
}

func (d *ThunderShare) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	// Buye re-expanded the selected share on every playback request. Keep only
	// a short, share-scoped reuse window for duplicate resolution attempts,
	// without serving a 15-minute Thunder CDN URL from the old one-hour cache.
	key := fmt.Sprintf("%s\x00%s\x00%d", d.ShareId, file.GetID(), d.AccountStorageID)
	if link, ok := thunderShareLinkCache.Get(key); ok {
		return link, nil
	}

	link, err := resolveThunderShareLink(ctx, d, file, args)
	if err == nil && link != nil {
		thunderShareLinkCache.SetWithTTL(key, link, thunderShareLinkCacheTTL)
	}
	return link, err
}

func (d *ThunderShare) Other(ctx context.Context, args model.OtherArgs) (interface{}, error) {
	if args.Method != "video_preview" {
		return nil, errors.New("unsupported ThunderShare other method")
	}
	count := d.accountCount()
	var lastErr error
	for i := 0; i < count; i++ {
		storage := d.accountDriver(idx)
		idx++
		if storage == nil {
			lastErr = errors.New("找不到迅雷云盘帐号")
			continue
		}
		thunder := storage.(*thunder_browser.ThunderBrowser)
		fileID, err := d.restoredFile(ctx, thunder, args.Obj)
		if err != nil {
			lastErr = err
			continue
		}
		info, err := thunder.GetFileInfo(ctx, fileID, "")
		if err != nil {
			lastErr = err
			continue
		}
		return buildVideoPreviewResult(
			info,
			thunder.XunLeiBrowserCommon.DownloadUserAgent,
		), nil
	}
	return nil, lastErr
}

func (d *ThunderShare) link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	storage := d.accountDriver(idx)
	idx++
	if storage == nil {
		return nil, errors.New("找不到迅雷云盘帐号")
	}
	thunder := storage.(*thunder_browser.ThunderBrowser)
	log.Infof("[%v] 获取迅雷云盘文件直链 %v %v %v", thunder.ID, file.GetName(), file.GetID(), file.GetSize())
	fileId, err := d.restoredFile(ctx, thunder, file)
	if err != nil {
		log.Warnf("[%v] 保存迅雷文件失败: %v", thunder.ID, err)
		return nil, err
	}

	link, err := d.getDownloadUrl(ctx, thunder, fileId)
	return link, err
}

var _ driver.Driver = (*ThunderShare)(nil)
