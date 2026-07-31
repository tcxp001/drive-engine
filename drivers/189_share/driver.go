package _189_share

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	_189pc "github.com/OpenListTeam/OpenList/v4/drivers/189pc"
	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/cache"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
)

type Cloud189Share struct {
	model.Storage
	Addition
	client *resty.Client
}

type videoPreviewResult struct {
	Routes []_189pc.ShareVideoRoute `json:"routes"`
	Header map[string]string        `json:"header"`
}

var cloud189ShareLinkCache = cache.NewKeyedCache[*model.Link](time.Hour)
var cloud189ShareLinkCacheTTL = 30 * time.Second

func (d *Cloud189Share) accountCount() int {
	if d.AccountStorageID > 0 {
		return 1
	}
	return op.GetDriverCount("189CloudPC")
}

func (d *Cloud189Share) accountDriver(index int) driver.Driver {
	if d.AccountStorageID > 0 {
		return op.GetDriverByStorageID("189CloudPC", d.AccountStorageID)
	}
	return op.GetFirstDriver("189CloudPC", index)
}

func applyCloud189NativePlaybackHeader(link *model.Link) *model.Link {
	if link == nil {
		return nil
	}
	if link.Header == nil {
		link.Header = make(http.Header)
	}
	link.Header.Set("Referer", "https://cloud.189.cn/")
	return link
}

func cloud189LinkPreviewResult(link *model.Link) (videoPreviewResult, error) {
	if link == nil || strings.TrimSpace(link.URL) == "" {
		return videoPreviewResult{}, errors.New("189 native download route is unavailable")
	}
	route := _189pc.ShareVideoRoute{
		Name:     "RAW",
		URL:      strings.TrimSpace(link.URL),
		Original: true,
	}
	if link.Expiration != nil && *link.Expiration > 0 {
		route.Expire = time.Now().Add(*link.Expiration)
	}
	headers := make(map[string]string, len(link.Header)+1)
	for name, values := range link.Header {
		separator := ", "
		if strings.EqualFold(name, "cookie") {
			separator = "; "
		}
		if value := strings.TrimSpace(strings.Join(values, separator)); value != "" {
			headers[name] = value
		}
	}
	headers["Referer"] = "https://cloud.189.cn/"
	return videoPreviewResult{
		Routes: []_189pc.ShareVideoRoute{route},
		Header: headers,
	}, nil
}

var resolveCloud189ShareLink = func(ctx context.Context, d *Cloud189Share, file model.Obj) (*model.Link, error) {
	count := d.accountCount()
	var lastErr error
	for i := 0; i < count; i++ {
		link, err := d.link(ctx, file)
		if err == nil {
			return link, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

var transferCloud189ShareFile = func(
	ctx context.Context,
	account *_189pc.Cloud189PC,
	shareID int,
	fileID string,
	fileName string,
) (*model.Link, error) {
	return account.Transfer(ctx, shareID, fileID, fileName)
}

var resolveCloud189ShareVideoRoutes = func(
	ctx context.Context,
	account *_189pc.Cloud189PC,
	shareID int,
	file model.Obj,
) ([]_189pc.ShareVideoRoute, error) {
	return account.GetShareVideoRoutes(ctx, shareID, file)
}

func resolveCloud189VideoPreview(
	ctx context.Context,
	account *_189pc.Cloud189PC,
	shareID int,
	file *FileObj,
) (videoPreviewResult, error) {
	if strings.TrimSpace(string(file.mimeType)) == "0" {
		link, err := transferCloud189ShareFile(
			ctx,
			account,
			shareID,
			file.ID,
			file.oldName,
		)
		if err != nil {
			return videoPreviewResult{}, err
		}
		return cloud189LinkPreviewResult(
			applyCloud189NativePlaybackHeader(link),
		)
	}

	routes, err := resolveCloud189ShareVideoRoutes(
		ctx,
		account,
		shareID,
		file,
	)
	if err != nil {
		return videoPreviewResult{}, err
	}
	return videoPreviewResult{
		Routes: routes,
		Header: map[string]string{
			"referer":    "https://cloud.189.cn/",
			"user-agent": base.UserAgent,
		},
	}, nil
}

func (d *Cloud189Share) Config() driver.Config {
	return config
}

func (d *Cloud189Share) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *Cloud189Share) Init(ctx context.Context) error {
	d.client = base.NewRestyClient().SetHeaders(map[string]string{
		"Accept":  "application/json;charset=UTF-8",
		"Referer": "https://cloud.189.cn",
	})

	if conf.LazyLoad && !conf.StoragesLoaded {
		return nil
	}

	return d.Validate()
}

func (d *Cloud189Share) Drop(ctx context.Context) error {
	return nil
}

func (d *Cloud189Share) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	files, err := d.getShareFiles(ctx, dir)
	if err != nil {
		return nil, err
	}
	return utils.SliceConvert(files, func(src FileObj) (model.Obj, error) {
		src.Path = filepath.Join(dir.GetPath(), src.GetID())
		return &src, nil
	})
}

func (d *Cloud189Share) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	err := limiter.WaitN(ctx, 1)
	if err != nil {
		return nil, err
	}

	_, ok := file.(*FileObj)
	if !ok {
		return nil, errors.New("文件格式错误")
	}

	// Preserve Buye's fresh share expansion while allowing duplicate native
	// link requests to reuse one short upstream operation. The old one-hour,
	// file-only key could return an expired link or collide across shares.
	key := d.ShareId + "\x00" + file.GetID()
	if link, ok := cloud189ShareLinkCache.Get(key); ok {
		return link, nil
	}

	link, err := resolveCloud189ShareLink(ctx, d, file)
	if err == nil && link != nil {
		cloud189ShareLinkCache.SetWithTTL(key, link, cloud189ShareLinkCacheTTL)
	}
	return link, err
}

func (d *Cloud189Share) Other(ctx context.Context, args model.OtherArgs) (interface{}, error) {
	if args.Method != "video_preview" {
		return nil, errors.New("unsupported 189Share other method")
	}
	count := d.accountCount()
	var lastErr error
	for i := 0; i < count; i++ {
		storage := d.accountDriver(idx)
		idx++
		if storage == nil {
			lastErr = errors.New("找不到天翼云盘帐号")
			continue
		}
		shareInfo, err := d.getShareInfo()
		if err != nil {
			lastErr = err
			continue
		}
		fileObject, ok := args.Obj.(*FileObj)
		if !ok {
			lastErr = errors.New("文件格式错误")
			continue
		}
		result, err := resolveCloud189VideoPreview(
			ctx,
			storage.(*_189pc.Cloud189PC),
			shareInfo.ShareId,
			fileObject,
		)
		if err != nil {
			lastErr = err
			continue
		}
		return result, nil
	}
	return nil, lastErr
}

func (d *Cloud189Share) link(ctx context.Context, file model.Obj) (*model.Link, error) {
	storage := d.accountDriver(idx)
	idx++
	if storage == nil {
		return nil, errors.New("找不到天翼云盘帐号")
	}
	cloud189PC := storage.(*_189pc.Cloud189PC)
	log.Infof("[%v] 获取天翼云盘文件直链 %v %v %v", cloud189PC.ID, file.GetName(), file.GetID(), file.GetSize())

	shareInfo, err := d.getShareInfo()
	if err != nil {
		return nil, err
	}

	link, err := cloud189PC.GetShareLink(shareInfo.ShareId, file)
	if link != nil {
		return applyCloud189NativePlaybackHeader(link), nil
	} else {
		log.Warnf("[%v] Get share link error: %v", cloud189PC.ID, err)
	}

	fileObject, _ := file.(*FileObj)
	log.Infof("[%v] 获取天翼云盘转存链接 %v %v", cloud189PC.ID, file.GetName(), file.GetID())
	link, err = cloud189PC.Transfer(ctx, shareInfo.ShareId, fileObject.ID, fileObject.oldName)
	return applyCloud189NativePlaybackHeader(link), err
}

var _ driver.Driver = (*Cloud189Share)(nil)
