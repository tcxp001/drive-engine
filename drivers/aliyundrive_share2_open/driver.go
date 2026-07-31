package aliyundrive_share2_open

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_115 "github.com/OpenListTeam/OpenList/v4/drivers/115"
	"github.com/OpenListTeam/OpenList/v4/drivers/aliyundrive_open"
	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/cache"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/OpenListTeam/OpenList/v4/pkg/cron"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
)

type AliyundriveShare2Open struct {
	base string
	model.Storage
	Addition
	cron *cron.Cron
}

type nativePlaybackRoute struct {
	Name     string            `json:"name"`
	URL      string            `json:"url"`
	Expire   time.Time         `json:"expire,omitempty"`
	Header   map[string]string `json:"header,omitempty"`
	Original bool              `json:"original,omitempty"`
}

type nativePlaybackResult struct {
	Routes []nativePlaybackRoute `json:"routes"`
}

type savedAliFile struct {
	FileID string
}

var aliyundriveShareLinkCache = cache.NewKeyedCache[*model.Link](time.Hour)
var aliyundriveShareLinkCacheTTL = 30 * time.Second
var savedAliFiles = cache.NewKeyedCache[savedAliFile](24 * time.Hour)
var savedAliFilesMu sync.Mutex

var aliyundriveShareAliTo115Enabled = func() bool {
	return setting.GetBool(conf.AliTo115)
}

func (d *AliyundriveShare2Open) aliTo115Enabled() bool {
	return !d.NativeOnly && aliyundriveShareAliTo115Enabled()
}

func (d *AliyundriveShare2Open) accountCount() int {
	if d.AccountStorageID > 0 {
		return 1
	}
	return op.GetDriverCount("AliyundriveOpen")
}

func (d *AliyundriveShare2Open) accountDriver(index int) (*aliyundrive_open.AliyundriveOpen, error) {
	if d.AccountStorageID == 0 {
		return getAliOpenDriver(index)
	}
	storage := op.GetDriverByStorageID("AliyundriveOpen", d.AccountStorageID)
	if storage == nil {
		return nil, errors.New("找不到阿里云盘帐号")
	}
	ali, ok := storage.(*aliyundrive_open.AliyundriveOpen)
	if !ok {
		return nil, errors.New("阿里云盘帐号类型错误")
	}
	return ali, nil
}

func (d *AliyundriveShare2Open) linkCacheKey(fileID string) string {
	return d.ShareId + "|" +
		strconv.FormatUint(uint64(d.AccountStorageID), 10) + "|" +
		fileID + "|" + strconv.FormatBool(d.aliTo115Enabled())
}

func playbackTemporaryFileTTL() time.Duration {
	seconds := setting.GetInt(conf.DeleteDelayTime, 900)
	if seconds <= 0 {
		return 24 * time.Hour
	}
	return time.Duration(seconds) * time.Second
}

func (d *AliyundriveShare2Open) savedFile(
	ali *aliyundrive_open.AliyundriveOpen,
	file model.Obj,
) (string, error) {
	key := d.ShareId + "\x00" + strconv.FormatUint(uint64(ali.ID), 10) + "\x00" + file.GetID()
	savedAliFilesMu.Lock()
	defer savedAliFilesMu.Unlock()
	if saved, ok := savedAliFiles.Get(key); ok && saved.FileID != "" {
		return saved.FileID, nil
	}
	fileID, err := d.saveFile(ali, file.GetID())
	if err != nil {
		return "", err
	}
	ttl := playbackTemporaryFileTTL()
	savedAliFiles.SetWithTTL(key, savedAliFile{FileID: fileID}, ttl)
	go d.deleteOpenAfter(ali, fileID, ttl)
	return fileID, nil
}

func flattenedPlaybackHeader(header http.Header) map[string]string {
	result := make(map[string]string, len(header))
	for name, values := range header {
		separator := ", "
		if strings.EqualFold(name, "cookie") {
			separator = "; "
		}
		if value := strings.TrimSpace(strings.Join(values, separator)); value != "" {
			result[name] = value
		}
	}
	return result
}

func absoluteLinkExpiry(link *model.Link) time.Time {
	if link == nil || link.Expiration == nil || *link.Expiration <= 0 {
		return time.Time{}
	}
	return time.Now().Add(*link.Expiration)
}

func buildAliNativePlaybackResult(
	rawLink *model.Link,
	preview *VideoPreviewResponse,
	previewExpire time.Time,
) nativePlaybackResult {
	result := nativePlaybackResult{
		Routes: []nativePlaybackRoute{
			{
				Name:     "RAW",
				URL:      rawLink.URL,
				Expire:   absoluteLinkExpiry(rawLink),
				Header:   flattenedPlaybackHeader(rawLink.Header),
				Original: true,
			},
		},
	}
	if preview == nil {
		return result
	}

	videos := make([]LiveTranscoding, 0, len(preview.PlayInfo.Videos))
	for _, video := range preview.PlayInfo.Videos {
		if strings.EqualFold(strings.TrimSpace(video.Status), "finished") &&
			strings.TrimSpace(video.TemplateId) != "" &&
			strings.TrimSpace(video.Url) != "" {
			videos = append(videos, video)
		}
	}
	sort.SliceStable(videos, func(i, j int) bool {
		return videos[i].TemplateHeight > videos[j].TemplateHeight
	})
	seen := map[string]struct{}{"RAW": {}}
	for _, video := range videos {
		name := strings.TrimSpace(video.TemplateId)
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		result.Routes = append(result.Routes, nativePlaybackRoute{
			Name:   name,
			URL:    strings.TrimSpace(video.Url),
			Expire: previewExpire,
			Header: map[string]string{
				"Referer": "https://www.alipan.com/",
			},
		})
	}
	return result
}

var resolveAliyundriveShareLink = func(ctx context.Context, d *AliyundriveShare2Open, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	count := d.accountCount()
	var lastErr error
	for i := 0; i < count; i++ {
		link, myFile, err := d.aliLink(file)
		if err == nil {
			if strings.HasSuffix(file.GetName(), ".md") || !d.aliTo115Enabled() {
				return link, nil
			}

			return d.p115Link(ctx, link, myFile, args)
		}
		lastErr = err
	}
	return nil, lastErr
}

func (d *AliyundriveShare2Open) Config() driver.Config {
	return config
}

func (d *AliyundriveShare2Open) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *AliyundriveShare2Open) Init(ctx context.Context) error {
	if conf.LazyLoad && !conf.StoragesLoaded {
		return nil
	}

	err := d.Validate()
	time.Sleep(1500 * time.Millisecond)
	return err
}

func (d *AliyundriveShare2Open) Drop(ctx context.Context) error {
	if d.cron != nil {
		d.cron.Stop()
	}
	return nil
}

func (d *AliyundriveShare2Open) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	return d.list(ctx, dir)
}

func (d *AliyundriveShare2Open) list(ctx context.Context, dir model.Obj) ([]model.Obj, error) {
	if d.ShareToken == "" {
		err := d.getShareToken()
		if err != nil {
			log.Warnf("getShareToken error: %v", err)
			return nil, err
		}
	}

	files, err := d.getFiles(dir.GetID())
	if err != nil {
		log.Warnf("list files error: %v", err)
		return nil, err
	}
	return utils.SliceConvert(files, func(src File) (model.Obj, error) {
		return fileToObj(src), nil
	})
}

func (d *AliyundriveShare2Open) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	key := d.linkCacheKey(file.GetID())
	if link, ok := aliyundriveShareLinkCache.Get(key); ok {
		return link, nil
	}

	link, err := resolveAliyundriveShareLink(ctx, d, file, args)
	if err == nil && link != nil {
		aliyundriveShareLinkCache.SetWithTTL(key, link, aliyundriveShareLinkCacheTTL)
	}
	return link, err
}

func (d *AliyundriveShare2Open) aliLink(file model.Obj) (*model.Link, *MyFile, error) {
	ali, err := d.accountDriver(idx)
	idx++
	if err != nil {
		return nil, nil, err
	}
	log.Infof("[%v] 获取阿里云盘文件直链 %v %v %v %v", ali.ID, ali.DriveId, file.GetName(), file.GetID(), file.GetSize())
	fileId, err := d.savedFile(ali, file)
	if err != nil {
		return nil, nil, err
	}

	newFile := MyFile{
		FileId: fileId,
		Name:   "livp",
	}

	link, hash, err := d.getOpenLink(ali, newFile)
	if err != nil {
		return nil, nil, err
	}

	myFile := MyFile{
		FileId:   fileId,
		Name:     file.GetName(),
		Size:     file.GetSize(),
		HashInfo: utils.NewHashInfo(utils.SHA1, hash),
	}

	return link, &myFile, nil
}

func (d *AliyundriveShare2Open) p115Link(ctx context.Context, link *model.Link, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	count := op.GetDriverCount("115 Cloud")
	for i := 0; i < count; i++ {
		driver115 := op.Get115Driver(idx2)
		idx2++
		if driver115 != nil {
			link115, err2 := d.saveTo115(ctx, driver115.(*_115.Pan115), file, link, args)
			if err2 == nil {
				return link115, nil
			}
		} else {
			break
		}
	}
	return link, nil
}

func (d *AliyundriveShare2Open) Other(ctx context.Context, args model.OtherArgs) (interface{}, error) {
	ali, err := d.accountDriver(idx)
	if err != nil {
		return nil, err
	}

	if args.Method == "share_info" {
		d.getShareToken()
		data := base.Json{
			"shareId":    d.ShareId,
			"sharePwd":   d.SharePwd,
			"shareToken": d.ShareToken,
			"fileId":     args.Obj.GetID(),
		}
		return data, nil
	}

	if args.Method != "video_preview" {
		return nil, errs.NotSupport
	}

	log.Infof("[%v] 获取文件链接 %v %v %v %v", ali.ID, ali.DriveId, args.Obj.GetID(), args.Obj.GetName(), args.Obj.GetSize())
	fileId, err := d.savedFile(ali, args.Obj)
	idx++
	if err != nil {
		return nil, err
	}

	rawLink, _, err := d.getOpenLink(ali, MyFile{FileId: fileId, Name: args.Obj.GetName()})
	if err != nil {
		return nil, err
	}
	result := buildAliNativePlaybackResult(rawLink, nil, time.Time{})

	var resp VideoPreviewResponse
	data := base.Json{
		"drive_id":       ali.DriveId,
		"file_id":        fileId,
		"category":       "live_transcoding",
		"url_expire_sec": 14400,
	}
	_, err = ali.Request("/adrive/v1.0/openFile/getVideoPreviewPlayInfo", http.MethodPost, func(req *resty.Request) {
		req.SetBody(data).SetResult(&resp)
	})
	if err != nil {
		// Preview is optional. A valid account-side RAW route remains the
		// complete native result when live transcoding is unavailable.
		log.Warnf("获取阿里转码线路失败，保留 RAW：%v", err)
		return result, nil
	}
	return buildAliNativePlaybackResult(
		rawLink,
		&resp,
		time.Now().Add(4*time.Hour),
	), nil
}

var _ driver.Driver = (*AliyundriveShare2Open)(nil)
