package baidu_share

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/baidu_netdisk"
	"github.com/OpenListTeam/OpenList/v4/internal/cache"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/OpenListTeam/OpenList/v4/pkg/cookie"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
)

var idx = 0
var baiduShareLinkCache = cache.NewKeyedCache[*model.Link](time.Hour)
var baiduNetdiskDriverCount = func() int {
	return op.GetDriverCount("BaiduNetdisk")
}
var nextBaiduNetdiskDriver = func(index int) driver.Driver {
	return op.GetFirstDriver("BaiduNetdisk", index)
}
var validateBaiduShare = func(d *BaiduShare2) error {
	return d.Validate()
}
var resolveBaiduUnlimitedAccountLink = func(
	ctx context.Context,
	d *BaiduShare2,
	file model.Obj,
	bd *baidu_netdisk.BaiduNetdisk,
) (*model.Link, error) {
	return d.unlimitedLink(ctx, file, bd)
}

func (d *BaiduShare2) accountCount() int {
	if d.AccountStorageID > 0 {
		return 1
	}
	return baiduNetdiskDriverCount()
}

func (d *BaiduShare2) accountDriver(index int) driver.Driver {
	if d.AccountStorageID > 0 {
		return op.GetDriverByStorageID("BaiduNetdisk", d.AccountStorageID)
	}
	return nextBaiduNetdiskDriver(index)
}

var resolveBaiduShareLink = func(ctx context.Context, d *BaiduShare2, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	count := d.accountCount()
	if count == 0 {
		return nil, errors.New("找不到百度网盘帐号")
	}
	var lastErr error
	for i := 0; i < count; i++ {
		storage := d.accountDriver(idx)
		idx++
		if storage == nil {
			lastErr = errors.New("找不到百度网盘帐号")
			continue
		}
		bd, ok := storage.(*baidu_netdisk.BaiduNetdisk)
		if !ok {
			lastErr = errors.New("百度网盘帐号类型异常")
			continue
		}
		if d.Token == "" {
			d.Validate()
		}

		// 首选：百度原画(无限)免转存 dlna 直链。免占账号空间、免延迟删除、
		// 且最终 CDN 直链免 Cookie、Range 完美、不受“每账号 dlink 限速”影响。
		if link, err := resolveBaiduUnlimitedAccountLink(ctx, d, file, bd); err == nil && link != nil {
			return link, nil
		} else if err != nil {
			if d.UnlimitedOnly {
				log.Warnf("[%v] 百度原画(无限)失败，尝试下一帐号: %v", bd.ID, err)
			} else {
				log.Warnf("[%v] 百度原画(无限)失败，回退转存: %v", bd.ID, err)
			}
			lastErr = err
		}
		if d.UnlimitedOnly {
			if lastErr == nil {
				lastErr = errors.New("百度原画(无限)未返回可播放直链")
			}
			continue
		}

		// 兜底：saveFile 转存-删除方案(dlna 未命中/账号无 BDUSS/受限分享时使用)。
		link, err := d.link(ctx, file, args, bd)
		if err == nil {
			return link, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

type BaiduShare2 struct {
	model.Storage
	Addition
	client *resty.Client

	ShareId string
	ShareUk string
	Token   string
}

func (d *BaiduShare2) Config() driver.Config {
	return config
}

func (d *BaiduShare2) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *BaiduShare2) Init(ctx context.Context) error {
	d.client = resty.New().
		SetBaseURL("https://pan.baidu.com").
		SetHeader("User-Agent", "netdisk").
		SetHeader("Referer", "https://pan.baidu.com")

	if conf.LazyLoad && !conf.StoragesLoaded {
		return nil
	}

	return d.Validate()
}

func (d *BaiduShare2) Drop(ctx context.Context) error {
	return nil
}

func (d *BaiduShare2) Validate() error {
	if d.Pwd != "" {
		api := "/share/verify?channel=chunlei&clienttype=0&web=1&app_id=250528&surl=" + d.Surl[1:]
		data := map[string]string{
			"pwd": d.Pwd,
		}
		respJson := struct {
			Errno   int64  `json:"errno"`
			Message string `json:"err_msg"`
			Token   string `json:"randsk"`
		}{}
		res, err := d.client.R().
			SetFormData(data).
			SetHeader("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8").
			SetResult(&respJson).
			Post(api)
		if err != nil {
			return err
		}
		log.Debugf("Baidu share verify response: %v", respJson)
		if respJson.Errno != 0 {
			msg := respJson.Message
			if msg == "" {
				msg = res.String()
			}
			return errors.New(msg)
		}
		d.Token = respJson.Token
		log.Debugf("Baidu Share Token: %v", d.Token)
	}

	return d.getInfo()
}

func (d *BaiduShare2) getInfo() error {
	api := "/s/" + d.Surl
	res, err := d.client.R().
		Get(api)
	if err != nil {
		return err
	}
	BDCLND := cookie.GetCookie(res.Cookies(), "BDCLND")
	if BDCLND != nil {
		d.Token = BDCLND.Value
	}

	re := regexp.MustCompile(`shareid:\s*"(\d+)"`)
	matches := re.FindStringSubmatch(res.String())
	if len(matches) >= 2 {
		d.ShareId = matches[1]
		log.Debugf("Share ID: %v", d.ShareId)
	} else {
		log.Warn("Share ID not found")
	}

	re = regexp.MustCompile(`share_uk:\s*"(\d+)"`)
	matches = re.FindStringSubmatch(res.String())
	if len(matches) >= 2 {
		d.ShareUk = matches[1]
		log.Debugf("Share UK: %v", d.ShareUk)
	} else {
		log.Warn("Share UK not found")
	}

	log.Debugf("Share Token: %v", d.Token)
	return nil
}

func (d *BaiduShare2) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	if d.Token == "" {
		d.Validate()
	}
	reqDir := dir.GetPath()
	isRoot := "0"
	if reqDir == d.RootFolderPath {
		reqDir = path.Join("/", reqDir)
	}
	if reqDir == "/" {
		isRoot = "1"
		reqDir = ""
	}
	objs := []model.Obj{}
	var err error
	var page = 1
	more := true
	for more && err == nil {
		respJson := struct {
			Errno int64 `json:"errno"`
			List  []struct {
				Fsid  json.Number `json:"fs_id"`
				Isdir json.Number `json:"isdir"`
				Path  string      `json:"path"`
				Name  string      `json:"server_filename"`
				Mtime json.Number `json:"server_mtime"`
				Size  json.Number `json:"size"`
			} `json:"list"`
		}{}
		query := map[string]string{
			"app_id":     "250528",
			"channel":    "chunlei",
			"clienttype": "0",
			"desc":       "0",
			"showempty":  "0",
			"web":        "1",
			"view_mode":  "1",
			"num":        "100",
			"order":      "name",
			"root":       isRoot,
			"dir":        reqDir,
			"shorturl":   d.Surl[1:],
			"page":       fmt.Sprint(page),
		}
		log.Debugf("Baidu Share List: %v", page)
		res, e := d.client.R().
			SetCookie(&http.Cookie{Name: "BDCLND", Value: d.Token}).
			SetResult(&respJson).
			SetQueryParams(query).
			Get("/share/list")
		err = e
		log.Debugf("%v result: %v", reqDir, res.String())
		more = false
		if err == nil {
			if res.IsSuccess() && respJson.Errno == 0 {
				page++
				for _, v := range respJson.List {
					size, _ := v.Size.Int64()
					mtime, _ := v.Mtime.Int64()
					objs = append(objs, &model.Object{
						ID:       v.Fsid.String(),
						Path:     v.Path,
						Name:     v.Name,
						Size:     size,
						Modified: time.Unix(mtime, 0),
						IsFolder: v.Isdir.String() == "1",
					})
				}
				if len(respJson.List) >= 100 {
					more = true
				}
			} else {
				err = fmt.Errorf("%s", res.Body())
			}
		}
	}
	return objs, err
}

func baiduPlaybackParamsError(shareID, shareUK, fid string) error {
	if fid == "" {
		return errors.New("[baidu_file_missing] 百度分享文件标识缺失")
	}
	if shareID == "" || shareUK == "" {
		return fmt.Errorf("[baidu_share_incomplete] shareid=%v uk=%v fid=true", shareID != "", shareUK != "")
	}
	return nil
}

func (d *BaiduShare2) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	// MV owns the request-wide recovery budget. Browsing and unpinned mounts
	// retain their initialization behavior; an incomplete playback cannot use a cache hit.
	if d.AccountStorageID > 0 {
		if err := baiduPlaybackParamsError(d.ShareId, d.ShareUk, file.GetID()); err != nil {
			return nil, err
		}
	}
	key := file.GetID()
	if link, ok := baiduShareLinkCache.Get(key); ok {
		return link, nil
	}

	link, err := resolveBaiduShareLink(ctx, d, file, args)
	if err == nil && link != nil {
		baiduShareLinkCache.Set(key, link)
	}
	return link, err
}

func (d *BaiduShare2) link(ctx context.Context, file model.Obj, args model.LinkArgs, bd *baidu_netdisk.BaiduNetdisk) (*model.Link, error) {
	log.Infof("[%v] 获取百度文件直链 %v %v %v", bd.ID, file.GetName(), file.GetID(), file.GetSize())

	f, err := d.saveFile(file.GetID(), bd)
	if err != nil {
		return nil, err
	}

	go d.delete(f, bd)

	link, err := bd.Link(ctx, f, args)
	log.Debugf("Baidu link: %v %v %v", f.GetID(), f.GetPath(), link)
	return link, err
}

// unlimitedLink 用给定百度账号解析一条“原画(无限)”免转存 dlna 直链。
// sekey 用分享校验得到的 d.Token(即 BDCLND / randsk 的原始 URL-encoded 形态)。
func (d *BaiduShare2) unlimitedLink(ctx context.Context, file model.Obj, bd *baidu_netdisk.BaiduNetdisk) (*model.Link, error) {
	if d.AccountStorageID > 0 {
		if err := baiduPlaybackParamsError(d.ShareId, d.ShareUk, file.GetID()); err != nil {
			return nil, err
		}
	}
	if d.ShareId == "" || d.ShareUk == "" {
		if err := validateBaiduShare(d); err != nil {
			return nil, err
		}
	}
	resolve := func() (*model.Link, error) {
		params := baiduUnlimitedParams{
			shareID: d.ShareId,
			shareUK: d.ShareUk,
			sekey:   d.Token,
			fileID:  file.GetID(),
			cookie:  bd.Cookie,
		}
		return resolveBaiduUnlimitedLink(ctx, baiduUnlimitedHTTPClient, params)
	}

	link, err := resolve()
	if !isBaiduDlnaErrno(err, -9) {
		return link, err
	}

	// -9 表示 fid 不在当前分享校验上下文。除了文件确实已失效外，常见原因是
	// 挂载保存的 BDCLND/randsk 已过期；重新 Validate 一次可刷新分享元数据
	// 和 sekey。只重试一次，避免对已删除文件或受限分享形成请求循环。
	if validateErr := validateBaiduShare(d); validateErr != nil {
		return nil, fmt.Errorf("%w；刷新分享校验态失败: %v", err, validateErr)
	}
	return resolve()
}

func (d *BaiduShare2) saveFile(fid string, bd *baidu_netdisk.BaiduNetdisk) (model.Obj, error) {
	if err := baiduPlaybackParamsError(d.ShareId, d.ShareUk, fid); err != nil {
		return nil, err
	}
	Cookie := cookie.SetStr(bd.Cookie, "BDCLND", d.Token)
	decoded, err := decodeBaiduSekey(d.Token)
	if err != nil {
		return nil, err
	}
	data := map[string]string{
		"fsidlist": fmt.Sprintf("[%v]", fid),
		"path":     "/" + conf.TempDirName,
	}
	query := map[string]string{
		"app_id":     "250528",
		"channel":    "chunlei",
		"clienttype": "0",
		"web":        "1",
		"async":      "1",
		"ondup":      "newcopy",
		"shareid":    d.ShareId,
		"from":       d.ShareUk,
		"sekey":      decoded,
	}

	res, err := d.client.R().
		SetFormData(data).
		SetHeader("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8").
		SetHeader("Cookie", Cookie).
		SetHeader("Referer", "https://pan.baidu.com").
		SetHeader("User-Agent", "netdisk").
		SetQueryParams(query).
		Post("/share/transfer")

	if err != nil {
		return nil, err
	}

	if res.IsSuccess() {
		log.Debugf("response: %v", res.String())
	}

	if utils.Json.Get(res.Body(), "errno").ToInt() == -6 {
		return nil, &baiduDlnaAPIError{errno: -6}
	}
	if utils.Json.Get(res.Body(), "errno").ToInt() != 0 {
		return nil, errors.New(utils.Json.Get(res.Body(), "show_msg").ToString())
	}

	file := File{
		FileId: utils.Json.Get(res.Body(), "extra", "list", 0, "to_fs_id").ToInt64(),
		Path:   utils.Json.Get(res.Body(), "extra", "list", 0, "to").ToString(),
	}
	return file, nil
}

func (d *BaiduShare2) delete(file model.Obj, bd *baidu_netdisk.BaiduNetdisk) {
	delayTime := setting.GetInt(conf.DeleteDelayTime, 900)
	if delayTime == 0 {
		return
	}

	if delayTime < 5 {
		delayTime = 5
	}

	log.Infof("[%v] Delete Baidu temp file %v after %v seconds.", bd.ID, file.GetID(), delayTime)
	time.Sleep(time.Duration(delayTime) * time.Second)
	bd.Delete(file)
}

func (d *BaiduShare2) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	return errs.NotSupport
}

func (d *BaiduShare2) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	return errs.NotSupport
}

func (d *BaiduShare2) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	return errs.NotSupport
}

func (d *BaiduShare2) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	return errs.NotSupport
}

func (d *BaiduShare2) Remove(ctx context.Context, obj model.Obj) error {
	return errs.NotSupport
}

func (d *BaiduShare2) Put(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, up driver.UpdateProgress) error {
	return errs.NotSupport
}

var _ driver.Driver = (*BaiduShare2)(nil)
