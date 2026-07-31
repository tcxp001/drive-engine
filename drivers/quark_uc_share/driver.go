package quark_uc_share

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	quark "github.com/OpenListTeam/OpenList/v4/drivers/quark_uc"
	"github.com/OpenListTeam/OpenList/v4/drivers/quark_uc_tv"
	"github.com/OpenListTeam/OpenList/v4/internal/cache"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/OpenListTeam/OpenList/v4/internal/token"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	log "github.com/sirupsen/logrus"
)

type QuarkUCShare struct {
	model.Storage
	Addition
	config driver.Config
	conf   Conf
}

// Unlimited playback uses an account-side temporary copy that is deleted after
// the configured delay (900 seconds by default). Refresh before that lifecycle
// ends instead of retaining a dead temporary-file link for a full hour.
var quarkUCShareLinkCache = cache.NewKeyedCache[*model.Link](10 * time.Minute)

var resolveQuarkUCShareLink = func(ctx context.Context, d *QuarkUCShare, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	name := d.getDriverName()
	count := op.GetDriverCount(name)
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

func (d *QuarkUCShare) Config() driver.Config {
	config := d.config
	if config.Name == "UCShare" || config.Name == "QuarkShare" {
		// Normal callers stay behind PowerList's stable proxy URL. The signed
		// anonymous CDN URL is available only through the explicit admin RAW
		// link request used by Media Vault's optional direct-play line.
		config.NoLinkURL = true
	}
	return config
}

func (d *QuarkUCShare) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *QuarkUCShare) Init(ctx context.Context) error {
	if d.config.Name == "UCShare" || d.config.Name == "QuarkShare" {
		return quarkUCUnlimitedKeeper.ValidateShare(ctx, d)
	}
	key := conf.QUARK
	if Cookie == "" {
		Cookie = token.GetAccountToken(key)
	}

	if conf.LazyLoad && !conf.StoragesLoaded {
		return nil
	}

	return d.Validate()
}

func (d *QuarkUCShare) Drop(ctx context.Context) error {
	if d.config.Name == "UCShare" || d.config.Name == "QuarkShare" {
		quarkUCUnlimitedKeeper.DropStorage(d.ID)
	}
	return nil
}

func (d *QuarkUCShare) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	if d.config.Name == "UCShare" || d.config.Name == "QuarkShare" {
		files, err := quarkUCUnlimitedKeeper.ListFiles(ctx, d, dir.GetID())
		if err != nil {
			log.Warnf("list %s files error: %v", d.config.Name, err)
			return nil, err
		}
		return utils.SliceConvert(files, func(src File) (model.Obj, error) {
			return fileToObj(src), nil
		})
	}
	if d.ShareToken == "" {
		if err := d.Validate(); err != nil {
			return nil, err
		}
	}

	files, err := d.getShareFiles(dir.GetID())
	if err != nil {
		log.Warnf("list files error: %v", err)
		return nil, err
	}
	return utils.SliceConvert(files, func(src File) (model.Obj, error) {
		return fileToObj(src), nil
	})
}

func (d *QuarkUCShare) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	if d.config.Name == "UCShare" || d.config.Name == "QuarkShare" {
		exposeRaw := args.Type == model.LinkTypeAnonymousRaw
		allowTraditionalFallback := !exposeRaw && !d.UnlimitedOnly
		var traditional ucTraditionalResolver
		if allowTraditionalFallback {
			traditional = func(currentCtx context.Context) (*model.Link, error) {
				// Do not retain the first playback request's IP or headers in a
				// long-lived RangeReader.  A fallback always uses the context of
				// the RangeRead that triggered it and a minimal LinkArgs value.
				return d.resolveTraditionalLink(currentCtx, file, model.LinkArgs{})
			}
		}
		link, err := quarkUCUnlimitedKeeper.Resolve(ctx, d, file, traditional, exposeRaw)
		if err == nil && link != nil {
			return link, nil
		}
		if err == nil {
			err = fmt.Errorf("%s unlimited link is unavailable", d.getDriverName())
		}
		log.WithFields(log.Fields{
			"action":     "resolve_failed",
			"provider":   d.getDriverName(),
			"storage_id": d.ID,
			"share":      shortSensitiveDigest(d.ShareId),
			"file":       shortSensitiveDigest(file.GetID()),
			"size":       file.GetSize(),
			"reason":     classifyUCUnlimitedError(err),
			"fallback":   allowTraditionalFallback,
		}).Warn("share unlimited lease event")
		if exposeRaw {
			if d.config.Name == "QuarkShare" && !d.UnlimitedOnly {
				safeLink, safeErr := d.resolveCredentialSafeRawLink(ctx, file)
				if safeErr == nil {
					return safeLink, nil
				}
				log.WithFields(log.Fields{
					"action":     "safe_raw_fallback_failed",
					"provider":   d.getDriverName(),
					"storage_id": d.ID,
					"share":      shortSensitiveDigest(d.ShareId),
					"file":       shortSensitiveDigest(file.GetID()),
					"size":       file.GetSize(),
					"reason":     classifyUCUnlimitedError(safeErr),
				}).Warn("share unlimited lease event")
			}
			return nil, err
		}
		if d.UnlimitedOnly {
			return nil, err
		}
		return d.resolveTraditionalLink(ctx, file, args)
	}
	return d.resolveTraditionalLink(ctx, file, args)
}

func (d *QuarkUCShare) resolveTraditionalLink(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	if d.ShareToken == "" {
		if err := d.Validate(); err != nil {
			return nil, err
		}
	}

	key := d.traditionalLinkCacheKey(file)
	if link, ok := quarkUCShareLinkCache.Get(key); ok {
		return link, nil
	}

	link, err := resolveQuarkUCShareLink(ctx, d, file, args)
	if err == nil && link != nil {
		quarkUCShareLinkCache.Set(key, link)
	}
	return link, err
}

func (d *QuarkUCShare) resolveCredentialSafeRawLink(
	ctx context.Context,
	file model.Obj,
) (*model.Link, error) {
	link, err := d.resolveTraditionalLink(ctx, file, model.LinkArgs{})
	if err != nil {
		return nil, err
	}
	if link == nil || !strings.HasPrefix(link.URL, "http") {
		return nil, fmt.Errorf("%s credential-safe RAW URL is unavailable", d.getDriverName())
	}
	if link.RequireReference {
		link.Close()
		return nil, fmt.Errorf("%s credential-safe RAW URL requires a live reference",
			d.getDriverName())
	}
	defer link.Close()

	provider, err := unlimitedProviderFor(d)
	if err != nil {
		return nil, err
	}
	headers := credentialSafeRawHeaders(link.Header, provider)
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, link.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("%s credential-safe RAW probe request is invalid",
			d.getDriverName())
	}
	request.Header = headers.Clone()
	request.Header.Set("Range", "bytes=0-0")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s credential-safe RAW probe failed: %w",
			d.getDriverName(), err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("%s credential-safe RAW probe returned HTTP %d",
			d.getDriverName(), response.StatusCode)
	}
	start, end, total, rangeErr := parseStrictContentRange(
		response.Header.Get("Content-Range"))
	if rangeErr != nil || start != 0 || end != 0 {
		return nil, fmt.Errorf("%s credential-safe RAW probe returned invalid Content-Range",
			d.getDriverName())
	}

	expiration := time.Hour
	if link.Expiration != nil {
		expiration = *link.Expiration
	}
	return &model.Link{
		URL:           link.URL,
		Header:        headers,
		Expiration:    &expiration,
		Concurrency:   link.Concurrency,
		PartSize:      link.PartSize,
		ContentLength: total,
	}, nil
}

func credentialSafeRawHeaders(source http.Header, provider unlimitedProvider) http.Header {
	headers := make(http.Header)
	for key, values := range source {
		switch strings.ToLower(key) {
		case "authorization", "cookie", "proxy-authorization":
			continue
		}
		for _, value := range values {
			headers.Add(key, value)
		}
	}
	if provider.key == "quark" {
		if cookie := quarkCDNLeaseCookie(source); cookie != "" {
			// User-approved exception: expose only Quark's CDN-scoped __puus
			// lease. Never forward any other imported account Cookie fields.
			headers.Set("Cookie", cookie)
		}
	}
	if headers.Get("User-Agent") == "" {
		headers.Set("User-Agent", provider.userAgent)
	}
	if headers.Get("Referer") == "" {
		headers.Set("Referer", strings.TrimRight(provider.referer, "/")+"/")
	}
	if provider.origin != "" && headers.Get("Origin") == "" {
		headers.Set("Origin", provider.origin)
	}
	headers.Set("Accept-Encoding", "identity")
	return headers
}

func quarkCDNLeaseCookie(source http.Header) string {
	request := &http.Request{Header: source.Clone()}
	for _, cookie := range request.Cookies() {
		if cookie.Name == "__puus" && cookie.Value != "" {
			return cookie.String()
		}
	}
	return ""
}

func (d *QuarkUCShare) traditionalLinkCacheKey(file model.Obj) string {
	return d.config.Name + ":" + strconv.FormatUint(uint64(d.ID), 10) + ":" +
		shortSensitiveDigest(d.ShareId) + ":" + file.GetID()
}

func (d *QuarkUCShare) link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	if setting.GetBool(conf.UssQuarkTv) {
		link, err := d.getTvLink(ctx, file, args, false)
		if link != nil {
			return link, err
		}
	}

	name := d.getDriverName()
	storage := op.GetFirstDriver(name, int(traditionalAccountIndex.Add(1)-1))
	if storage == nil {
		return nil, fmt.Errorf("找不到%s网盘帐号", name)
	}
	uc := storage.(*quark.QuarkOrUC)
	if !uc.VIP {
		link, err := d.getTvLink(ctx, file, args, true)
		if link != nil {
			return link, err
		}
	}

	Cookie = uc.Cookie
	log.Infof("[%v] 获取%s文件直链 size=%v", uc.ID, name, file.GetSize())
	binding := bindRequestDriver(uc)
	newFile, err := d.saveFile(binding, uc, file.GetID())
	if err != nil {
		return nil, err
	}

	link, err := d.getDownloadUrl(ctx, uc, newFile, args)
	return link, err
}

func (d *QuarkUCShare) getTvLink(ctx context.Context, file model.Obj, args model.LinkArgs, forceStream bool) (*model.Link, error) {
	var tvName string
	if d.config.Name == "UCShare" {
		tvName = "UCTV"
	} else {
		tvName = "QuarkTV"
	}
	storage := op.GetFirstDriver(tvName, int(traditionalTVAccountIndex.Add(1)-1))
	if storage != nil {
		uc := storage.(*quark_uc_tv.QuarkUCTV)
		if uc.Cookie != "" {
			Cookie = uc.Cookie
			log.Infof("[%v] 获取%s文件直链 size=%v", uc.ID, tvName, file.GetSize())
			binding := bindTVRequestDriver(uc)
			newFile, err := d.saveTvFile(ctx, binding, uc, file.GetID())
			if err != nil {
				return nil, err
			}

			videoLinkMethod := uc.Addition.VideoLinkMethod
			if forceStream {
				uc.Addition.VideoLinkMethod = "streaming"
			}
			link, err := d.getTvDownloadUrl(ctx, uc, newFile, args)
			uc.Addition.VideoLinkMethod = videoLinkMethod
			if link != nil && uc.VideoLinkMethod == "streaming" {
				link.URL = link.URL + "#proxy=0"
			}
			return link, err
		}
	}
	return nil, nil
}

var (
	traditionalAccountIndex   atomic.Uint64
	traditionalTVAccountIndex atomic.Uint64
)

var _ driver.Driver = (*QuarkUCShare)(nil)
