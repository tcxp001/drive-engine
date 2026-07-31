package pikpak_share

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/pikpak"
	"github.com/OpenListTeam/OpenList/v4/internal/cache"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/go-resty/resty/v2"
)

type PikPakShare struct {
	model.Storage
	Addition
	*Common
	PassCodeToken string
}

type accountPlaybackRoute struct {
	Name     string    `json:"name"`
	URL      string    `json:"url"`
	Expire   time.Time `json:"expire,omitempty"`
	Original bool      `json:"original,omitempty"`
}

type accountPlaybackResult struct {
	Routes []accountPlaybackRoute `json:"routes"`
	Header map[string]string      `json:"header"`
}

type restoredPikPakFile struct {
	FileID string
}

var restoredPikPakFiles = cache.NewKeyedCache[restoredPikPakFile](24 * time.Hour)
var restoredPikPakFilesMu sync.Mutex

var restorePikPakShareFile = func(
	ctx context.Context,
	account *pikpak.PikPak,
	shareID string,
	passCodeToken string,
	sourceFileID string,
	name string,
	size int64,
) (string, error) {
	return account.RestoreShareFile(
		ctx,
		shareID,
		passCodeToken,
		sourceFileID,
		name,
		size,
	)
}

var schedulePikPakAccountFileCleanup = func(
	d *PikPakShare,
	account *pikpak.PikPak,
	fileID string,
	delay time.Duration,
) {
	go d.deleteAccountFileAfter(account, fileID, delay)
}

var pikPakTemporaryFileTTL = func() time.Duration {
	seconds := setting.GetInt(conf.DeleteDelayTime, 900)
	if seconds <= 0 {
		return 24 * time.Hour
	}
	return time.Duration(seconds) * time.Second
}

func (d *PikPakShare) account() (*pikpak.PikPak, error) {
	if d.AccountStorageID == 0 {
		return nil, errors.New("找不到PikPak网盘帐号")
	}
	storage := op.GetDriverByStorageID("PikPak", d.AccountStorageID)
	if storage == nil {
		return nil, errors.New("找不到PikPak网盘帐号")
	}
	account, ok := storage.(*pikpak.PikPak)
	if !ok {
		return nil, errors.New("PikPak网盘帐号类型错误")
	}
	return account, nil
}

func (d *PikPakShare) restoredFile(
	ctx context.Context,
	account *pikpak.PikPak,
	file model.Obj,
) (string, error) {
	key := fmt.Sprintf("%s\x00%s\x00%d", d.ShareId, file.GetID(), account.ID)
	restoredPikPakFilesMu.Lock()
	defer restoredPikPakFilesMu.Unlock()
	if saved, ok := restoredPikPakFiles.Get(key); ok && saved.FileID != "" {
		return saved.FileID, nil
	}
	fileID, err := restorePikPakShareFile(
		ctx,
		account,
		d.ShareId,
		d.PassCodeToken,
		file.GetID(),
		file.GetName(),
		file.GetSize(),
	)
	if err != nil {
		return "", err
	}
	ttl := pikPakTemporaryFileTTL()
	restoredPikPakFiles.SetWithTTL(key, restoredPikPakFile{FileID: fileID}, ttl)
	schedulePikPakAccountFileCleanup(d, account, fileID, ttl)
	return fileID, nil
}

func (d *PikPakShare) deleteAccountFileDelay(account *pikpak.PikPak, fileID string) {
	d.deleteAccountFileAfter(account, fileID, pikPakTemporaryFileTTL())
}

func (d *PikPakShare) deleteAccountFileAfter(
	account *pikpak.PikPak,
	fileID string,
	delay time.Duration,
) {
	if delay <= 0 {
		return
	}
	time.Sleep(delay)
	if err := account.Remove(
		context.Background(),
		&model.Object{ID: fileID, Name: fileID},
	); err != nil {
		// Cleanup is best-effort and must not invalidate an already-issued
		// playback result.
		return
	}
}

func (d *PikPakShare) accountPlayback(
	ctx context.Context,
	file model.Obj,
) (accountPlaybackResult, error) {
	account, err := d.account()
	if err != nil {
		return accountPlaybackResult{}, err
	}
	fileID, err := d.restoredFile(ctx, account, file)
	if err != nil {
		return accountPlaybackResult{}, err
	}
	info, err := account.GetFileInfo(ctx, fileID)
	if err != nil {
		return accountPlaybackResult{}, err
	}

	return buildAccountPlaybackResult(
		info,
		account.GetUserAgent(),
		account.GetDeviceID(),
	)
}

func buildAccountPlaybackResult(
	info *pikpak.File,
	userAgent string,
	deviceID string,
) (accountPlaybackResult, error) {
	routes := make([]accountPlaybackRoute, 0, len(info.Medias)+1)
	raw := info.Links["application/octet-stream"]
	rawURL := strings.TrimSpace(raw.Url)
	if rawURL == "" {
		rawURL = strings.TrimSpace(info.WebContentLink)
	}
	if rawURL != "" {
		routes = append(routes, accountPlaybackRoute{
			Name:     "RAW",
			URL:      rawURL,
			Expire:   raw.Expire,
			Original: true,
		})
	}
	for _, media := range info.Medias {
		url := strings.TrimSpace(media.Link.Url)
		if url == "" {
			continue
		}
		name := strings.TrimSpace(media.MediaName)
		if name == "" {
			name = strings.TrimSpace(media.ResolutionName)
		}
		if name == "" {
			name = "other"
		}
		routes = append(routes, accountPlaybackRoute{
			Name:   name,
			URL:    url,
			Expire: media.Link.Expire,
		})
	}
	if len(routes) == 0 {
		return accountPlaybackResult{}, errors.New("PikPak播放线路为空")
	}
	return accountPlaybackResult{
		Routes: routes,
		Header: map[string]string{
			"user-agent":  userAgent,
			"x-device-id": deviceID,
		},
	}, nil
}

func (d *PikPakShare) Config() driver.Config {
	return config
}

func (d *PikPakShare) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *PikPakShare) Init(ctx context.Context) error {
	if d.Common == nil {
		d.Common = &Common{
			DeviceID:  utils.GetMD5EncodeStr(d.Addition.ShareId + d.Addition.SharePwd + time.Now().String()),
			UserAgent: "",
			RefreshCTokenCk: func(token string) {
				d.Common.CaptchaToken = token
				op.MustSaveDriverStorage(d)
			},
		}
	}

	if d.Addition.DeviceID != "" {
		d.SetDeviceID(d.Addition.DeviceID)
	} else {
		d.Addition.DeviceID = d.Common.DeviceID
		op.MustSaveDriverStorage(d)
	}

	if d.Platform == "" {
		d.Platform = "pc"
	}

	if d.Platform == "android" {
		d.ClientID = AndroidClientID
		d.ClientSecret = AndroidClientSecret
		d.ClientVersion = AndroidClientVersion
		d.PackageName = AndroidPackageName
		d.Algorithms = AndroidAlgorithms
		d.UserAgent = BuildCustomUserAgent(d.GetDeviceID(), AndroidClientID, AndroidPackageName, AndroidSdkVersion, AndroidClientVersion, AndroidPackageName, "")
	} else if d.Platform == "web" {
		d.ClientID = WebClientID
		d.ClientSecret = WebClientSecret
		d.ClientVersion = WebClientVersion
		d.PackageName = WebPackageName
		d.Algorithms = WebAlgorithms
		d.UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/117.0.0.0 Safari/537.36"
	} else if d.Platform == "pc" {
		d.ClientID = PCClientID
		d.ClientSecret = PCClientSecret
		d.ClientVersion = PCClientVersion
		d.PackageName = PCPackageName
		d.Algorithms = PCAlgorithms
		d.UserAgent = "MainWindow Mozilla/5.0 (Windows NT 10.0; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) PikPak/2.6.11.4955 Chrome/100.0.4896.160 Electron/18.3.15 Safari/537.36"
	}

	// 获取CaptchaToken
	err := d.RefreshCaptchaToken(GetAction(http.MethodGet, "https://api-drive.mypikpak.net/drive/v1/share:batch_file_info"), "")
	if err != nil {
		return err
	}

	if d.SharePwd != "" {
		return d.getSharePassToken()
	}

	return nil
}

func (d *PikPakShare) Drop(ctx context.Context) error {
	return nil
}

func (d *PikPakShare) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	files, err := d.getFiles(dir.GetID())
	if err != nil {
		return nil, err
	}
	return utils.SliceConvert(files, func(src File) (model.Obj, error) {
		return fileToObj(src), nil
	})
}

func (d *PikPakShare) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	if d.AccountPlaybackOnly {
		result, err := d.accountPlayback(ctx, file)
		if err != nil {
			return nil, err
		}
		route := result.Routes[0]
		header := http.Header{}
		for name, value := range result.Header {
			if strings.TrimSpace(value) != "" {
				header.Set(name, value)
			}
		}
		return &model.Link{URL: route.URL, Header: header}, nil
	}

	var resp ShareResp
	query := map[string]string{
		"share_id":        d.ShareId,
		"file_id":         file.GetID(),
		"pass_code_token": d.PassCodeToken,
	}
	_, err := d.request("https://api-drive.mypikpak.net/drive/v1/share/file_info", http.MethodGet, func(req *resty.Request) {
		req.SetQueryParams(query)
	}, &resp)
	if err != nil {
		return nil, err
	}

	downloadUrl := resp.FileInfo.WebContentLink
	if downloadUrl == "" && len(resp.FileInfo.Medias) > 0 {
		// 使用转码后的链接
		if d.Addition.UseTransCodingAddress && len(resp.FileInfo.Medias) > 1 {
			downloadUrl = resp.FileInfo.Medias[1].Link.Url
		} else {
			downloadUrl = resp.FileInfo.Medias[0].Link.Url
		}

	}

	return &model.Link{
		URL: downloadUrl,
	}, nil
}

func (d *PikPakShare) Other(ctx context.Context, args model.OtherArgs) (interface{}, error) {
	if args.Method != "video_preview" {
		return nil, errs.NotSupport
	}
	if args.Obj == nil || args.Obj.IsDir() {
		return nil, errs.NotFile
	}
	return d.accountPlayback(ctx, args.Obj)
}

var _ driver.Driver = (*PikPakShare)(nil)
