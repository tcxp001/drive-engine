package quark_uc_share

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	cookiepkg "github.com/OpenListTeam/OpenList/v4/pkg/cookie"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/singleflight"
	log "github.com/sirupsen/logrus"
)

const (
	ucUnlimitedDefaultAPIBase = "https://pc-api.uc.cn/1/clouddrive"
	ucUnlimitedUserAgent      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " +
		"AppleWebKit/537.36 (KHTML, like Gecko) uc-cloud-drive/2.5.20 " +
		"Chrome/100.0.4896.160 Electron/18.3.5.4-b478491100 Safari/537.36 " +
		"Channel/pckk_other_ch"
	quarkUnlimitedDefaultAPIBase = "https://drive-pc.quark.cn/1/clouddrive"
	quarkUnlimitedUserAgent      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " +
		"AppleWebKit/537.36 (KHTML, like Gecko) quark-cloud-drive/2.5.20 " +
		"Chrome/100.0.4896.160 Electron/18.3.5.4-b478491100 Safari/537.36 " +
		"Channel/pckk_other_ch"

	ucUnlimitedLinkTTL      = 10 * time.Minute
	ucUnlimitedLeaseIdleTTL = 30 * time.Minute
	ucUnlimitedRefreshAfter = 15 * time.Minute
	ucUnlimitedStokenTTL    = 30 * time.Minute
	ucUnlimitedMaxDirs      = 2000
	ucUnlimitedMaxPages     = 100
)

// quarkUCUnlimitedKeeper owns the shared unlimited lease lifecycle for both
// QuarkShare and UCShare. UC is anonymous. Quark uses an account Cookie only
// at its API boundary and reduces it to a CDN-scoped __puus lease.
var quarkUCUnlimitedKeeper = newGlobalUCUnlimitedLeaseKeeper()

type ucTraditionalResolver func(context.Context) (*model.Link, error)

// unlimitedProvider keeps the lease/range machinery shared while isolating
// the small protocol differences between UCShare and QuarkShare.
type unlimitedProvider struct {
	key       string
	apiBase   string
	userAgent string
	referer   string
	origin    string
	pr        string
}

func unlimitedProviderFor(d *QuarkUCShare) (unlimitedProvider, error) {
	if d == nil {
		return unlimitedProvider{}, errors.New("share unlimited driver is unavailable")
	}
	switch d.config.Name {
	case "UCShare":
		apiBase := d.conf.api
		if apiBase == "" {
			apiBase = ucUnlimitedDefaultAPIBase
		}
		userAgent := d.conf.ua
		if userAgent == "" {
			userAgent = ucUnlimitedUserAgent
		}
		referer := d.conf.referer
		if referer == "" {
			referer = "https://drive.uc.cn"
		}
		pr := d.conf.pr
		if pr == "" {
			pr = "UCBrowser"
		}
		return unlimitedProvider{
			key:       "uc",
			apiBase:   apiBase,
			userAgent: userAgent,
			referer:   referer,
			pr:        pr,
		}, nil
	case "QuarkShare":
		apiBase := d.conf.api
		if apiBase == "" {
			apiBase = quarkUnlimitedDefaultAPIBase
		}
		userAgent := d.conf.ua
		if userAgent == "" {
			userAgent = quarkUnlimitedUserAgent
		}
		referer := d.conf.referer
		if referer == "" {
			referer = "https://pan.quark.cn"
		}
		pr := d.conf.pr
		if pr == "" {
			pr = "ucpro"
		}
		return unlimitedProvider{
			key:       "quark",
			apiBase:   apiBase,
			userAgent: userAgent,
			referer:   referer,
			origin:    "https://pan.quark.cn",
			pr:        pr,
		}, nil
	default:
		return unlimitedProvider{}, errors.New("share unlimited resolver is not applicable")
	}
}

type ucUnlimitedLeaseKey struct {
	storageID uint
	shareID   string
	fileID    string
}

func (k ucUnlimitedLeaseKey) string() string {
	return strconv.FormatUint(uint64(k.storageID), 10) + ":" + k.shareID + ":" + k.fileID
}

type ucUnlimitedObjectID struct {
	fileID    string
	fidToken  string
	parentFID string
}

type ucUnlimitedResolveParams struct {
	key       ucUnlimitedLeaseKey
	driver    *QuarkUCShare
	provider  unlimitedProvider
	sharePwd  string
	parentFID string
	fidToken  string
	size      int64
	fallback  ucTraditionalResolver
}

type ucUnlimitedSnapshot struct {
	url        string
	fidToken   string
	parentFID  string
	size       int64
	generation uint64
	obtainedAt time.Time
	refreshAt  time.Time
}

type ucUnlimitedLease struct {
	mu sync.RWMutex

	keeper   *ucUnlimitedLeaseKeeper
	params   ucUnlimitedResolveParams
	snapshot ucUnlimitedSnapshot

	lastAccess         time.Time
	consecutiveFailure int
	dropped            bool
	activeReaders      atomic.Int64
	fallbackReader     model.RangeReaderIF
	fallbackLink       *model.Link
	fallbackUntil      time.Time
}

type ucUnlimitedStoken struct {
	value     string
	expiresAt time.Time
}

type ucUnlimitedStorageState struct {
	leasePUUS string
	stokens   map[string]ucUnlimitedStoken
	leases    map[ucUnlimitedLeaseKey]*ucUnlimitedLease
}

type ucUnlimitedHTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type ucUnlimitedLeaseKeeper struct {
	mu sync.Mutex

	storages map[uint]*ucUnlimitedStorageState
	group    singleflight.Group[*ucUnlimitedLease]
	client   ucUnlimitedHTTPDoer
	apiBase  string
	now      func() time.Time

	linkTTL      time.Duration
	leaseIdleTTL time.Duration
	refreshAfter time.Duration
	stokenTTL    time.Duration
}

func newUCUnlimitedLeaseKeeper(client ucUnlimitedHTTPDoer, apiBase string) *ucUnlimitedLeaseKeeper {
	if client == nil {
		client = &http.Client{
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				if len(via) > 0 && !strings.EqualFold(request.URL.Host, via[0].URL.Host) {
					request.Header.Del("Cookie")
				}
				return nil
			},
		}
	}
	return &ucUnlimitedLeaseKeeper{
		storages:     make(map[uint]*ucUnlimitedStorageState),
		client:       client,
		apiBase:      strings.TrimRight(apiBase, "/"),
		now:          time.Now,
		linkTTL:      ucUnlimitedLinkTTL,
		leaseIdleTTL: ucUnlimitedLeaseIdleTTL,
		refreshAfter: ucUnlimitedRefreshAfter,
		stokenTTL:    ucUnlimitedStokenTTL,
	}
}

func newGlobalUCUnlimitedLeaseKeeper() *ucUnlimitedLeaseKeeper {
	keeper := newUCUnlimitedLeaseKeeper(nil, "")
	go keeper.maintenanceLoop()
	return keeper
}

func (k *ucUnlimitedLeaseKeeper) maintenanceLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		k.maintain(context.Background())
	}
}

func (k *ucUnlimitedLeaseKeeper) maintain(ctx context.Context) {
	k.cleanupIdle()
	now := k.now()
	k.mu.Lock()
	leases := make([]*ucUnlimitedLease, 0)
	for _, state := range k.storages {
		for _, lease := range state.leases {
			lease.mu.RLock()
			active := lease.activeReaders.Load() > 0 || now.Sub(lease.lastAccess) < k.leaseIdleTTL
			due := !lease.dropped && !lease.snapshot.refreshAt.IsZero() &&
				!now.Before(lease.snapshot.refreshAt)
			lease.mu.RUnlock()
			if active && due {
				leases = append(leases, lease)
			}
		}
	}
	k.mu.Unlock()
	for _, lease := range leases {
		refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, _ = k.refreshLease(refreshCtx, lease, false)
		cancel()
	}
}

func (k *ucUnlimitedLeaseKeeper) Resolve(
	ctx context.Context,
	d *QuarkUCShare,
	file model.Obj,
	fallback ucTraditionalResolver,
	exposeRaw bool,
) (*model.Link, error) {
	provider, err := unlimitedProviderFor(d)
	if err != nil {
		return nil, err
	}
	objectID, err := parseUCUnlimitedObjectID(file.GetID())
	if err != nil {
		return nil, err
	}
	if file.GetSize() <= 0 {
		return nil, fmt.Errorf("%s unlimited file size is unavailable", provider.key)
	}

	params := ucUnlimitedResolveParams{
		key: ucUnlimitedLeaseKey{
			storageID: d.ID,
			shareID:   d.ShareId,
			fileID:    objectID.fileID,
		},
		driver:    d,
		provider:  provider,
		sharePwd:  d.SharePwd,
		parentFID: objectID.parentFID,
		fidToken:  objectID.fidToken,
		size:      file.GetSize(),
		fallback:  fallback,
	}

	k.cleanupIdle()
	if lease := k.cachedLease(params.key); lease != nil {
		lease.updateRequest(params)
		if lease.needsRefresh(k.now()) {
			// A refresh due before expiry is proactive.  A still-valid current
			// generation remains usable if UC temporarily rejects the refresh.
			_, _ = k.refreshLease(ctx, lease, false)
		}
		return k.linkForLease(lease, exposeRaw), nil
	}

	lease, err, _ := k.group.Do("resolve:"+params.key.string(), func() (*ucUnlimitedLease, error) {
		if cached := k.cachedLease(params.key); cached != nil {
			cached.updateRequest(params)
			return cached, nil
		}
		snapshot, resolveErr := k.resolveSnapshot(ctx, params, false)
		if resolveErr != nil {
			snapshot, resolveErr = k.resolveSnapshot(ctx, params, true)
		}
		if resolveErr != nil {
			return nil, resolveErr
		}
		now := k.now()
		lease := &ucUnlimitedLease{
			keeper:     k,
			params:     params,
			snapshot:   snapshot,
			lastAccess: now,
		}
		k.storeLease(lease)
		k.logLease("resolved", lease, 0, false)
		return lease, nil
	})
	if err != nil {
		return nil, err
	}
	return k.linkForLease(lease, exposeRaw), nil
}

func (k *ucUnlimitedLeaseKeeper) ValidateShare(ctx context.Context, d *QuarkUCShare) error {
	provider, err := unlimitedProviderFor(d)
	if err != nil {
		return err
	}
	if d.ShareId == "" {
		return fmt.Errorf("%s share metadata is incomplete", provider.key)
	}
	params := ucUnlimitedResolveParams{
		key: ucUnlimitedLeaseKey{
			storageID: d.ID,
			shareID:   d.ShareId,
		},
		driver:   d,
		provider: provider,
		sharePwd: d.SharePwd,
	}
	stoken, err := k.getStoken(ctx, params, false)
	if err != nil {
		return err
	}
	d.ShareToken = stoken
	return nil
}

func (k *ucUnlimitedLeaseKeeper) ListFiles(
	ctx context.Context,
	d *QuarkUCShare,
	parentObjectID string,
) ([]File, error) {
	if err := k.ValidateShare(ctx, d); err != nil {
		return nil, err
	}
	parentFID := parentObjectID
	if parentFID == "" {
		parentFID = "0"
	} else if strings.Contains(parentFID, "-") {
		parentFID = strings.SplitN(parentFID, "-", 2)[0]
	}
	params := ucUnlimitedResolveParams{
		key: ucUnlimitedLeaseKey{
			storageID: d.ID,
			shareID:   d.ShareId,
		},
		driver:   d,
		provider: mustUnlimitedProvider(d),
		sharePwd: d.SharePwd,
	}
	files, err := k.listFilesWithStoken(ctx, d, parentFID, d.ShareToken)
	if err == nil {
		return files, nil
	}
	stoken, refreshErr := k.getStoken(ctx, params, true)
	if refreshErr != nil {
		return nil, refreshErr
	}
	d.ShareToken = stoken
	return k.listFilesWithStoken(ctx, d, parentFID, stoken)
}

func mustUnlimitedProvider(d *QuarkUCShare) unlimitedProvider {
	provider, _ := unlimitedProviderFor(d)
	return provider
}

func (k *ucUnlimitedLeaseKeeper) listFilesWithStoken(
	ctx context.Context,
	d *QuarkUCShare,
	parentFID string,
	stoken string,
) ([]File, error) {
	files := make([]File, 0)
	for page := 1; page <= ucUnlimitedMaxPages; page++ {
		var response ListResp
		query := url.Values{
			"pwd_id":        {d.ShareId},
			"stoken":        {stoken},
			"pdir_fid":      {parentFID},
			"force":         {"0"},
			"_page":         {strconv.Itoa(page)},
			"_size":         {"100"},
			"_fetch_banner": {"0"},
			"_fetch_share":  {"0"},
			"_fetch_total":  {"1"},
		}
		if d.OrderBy != "" && d.OrderBy != "none" {
			query.Set("_sort", "file_type:asc,"+d.OrderBy+":"+d.OrderDirection)
		}
		params := ucUnlimitedResolveParams{
			key:      ucUnlimitedLeaseKey{storageID: d.ID, shareID: d.ShareId},
			driver:   d,
			provider: mustUnlimitedProvider(d),
			sharePwd: d.SharePwd,
		}
		if err := k.apiJSON(ctx, params, http.MethodGet, "/share/sharepage/detail",
			query, nil, &response); err != nil {
			return nil, err
		}
		if err := validateUnlimitedAPIResponse(params.provider, response.Status, response.Code,
			response.Message); err != nil {
			return nil, err
		}
		files = append(files, response.Data.Files...)
		if len(response.Data.Files) == 0 || len(files) >= response.Metadata.Total {
			break
		}
	}
	return files, nil
}

func (k *ucUnlimitedLeaseKeeper) linkForLease(
	lease *ucUnlimitedLease,
	exposeRaw bool,
) *model.Link {
	lease.mu.RLock()
	size := lease.snapshot.size
	rawURL := lease.snapshot.url
	params := lease.params
	lease.mu.RUnlock()
	expiration := k.linkTTL
	link := &model.Link{
		RangeReader:   &ucUnlimitedRangeReader{lease: lease},
		ContentLength: size,
		Expiration:    &expiration,
	}
	if exposeRaw {
		link.URL = rawURL
		link.Header = unlimitedRawHeaders(
			params.provider,
			rawURL,
			k.cdnLeaseCookie(params),
		)
	}
	return link
}

func unlimitedRawHeaders(
	provider unlimitedProvider,
	rawURL string,
	leaseCookie string,
) http.Header {
	headers := make(http.Header)
	headers.Set("User-Agent", provider.userAgent)
	if provider.key == "uc" {
		headers.Set("Referer", rawURL+"\\ ")
	} else {
		headers.Set("Referer", strings.TrimRight(provider.referer, "/")+"/")
		headers.Set("Origin", provider.origin)
		headers.Set("Accept", "*/*")
		if leaseCookie != "" {
			headers.Set("Cookie", leaseCookie)
		}
	}
	headers.Set("Accept-Encoding", "identity")
	return headers
}

func (k *ucUnlimitedLeaseKeeper) cachedLease(key ucUnlimitedLeaseKey) *ucUnlimitedLease {
	k.mu.Lock()
	defer k.mu.Unlock()
	state := k.storages[key.storageID]
	if state == nil {
		return nil
	}
	lease := state.leases[key]
	if lease == nil {
		return nil
	}
	lease.mu.RLock()
	dropped := lease.dropped
	lease.mu.RUnlock()
	if dropped {
		delete(state.leases, key)
		return nil
	}
	return lease
}

func (k *ucUnlimitedLeaseKeeper) storeLease(lease *ucUnlimitedLease) {
	k.mu.Lock()
	defer k.mu.Unlock()
	state := k.storageStateLocked(lease.params.key.storageID)
	state.leases[lease.params.key] = lease
}

func (k *ucUnlimitedLeaseKeeper) storageStateLocked(storageID uint) *ucUnlimitedStorageState {
	state := k.storages[storageID]
	if state == nil {
		state = &ucUnlimitedStorageState{
			stokens: make(map[string]ucUnlimitedStoken),
			leases:  make(map[ucUnlimitedLeaseKey]*ucUnlimitedLease),
		}
		k.storages[storageID] = state
	}
	return state
}

func (k *ucUnlimitedLeaseKeeper) DropStorage(storageID uint) {
	k.mu.Lock()
	state := k.storages[storageID]
	delete(k.storages, storageID)
	k.mu.Unlock()
	if state == nil {
		return
	}
	for _, lease := range state.leases {
		lease.mu.Lock()
		lease.dropped = true
		lease.snapshot.url = ""
		lease.params.fallback = nil
		if lease.activeReaders.Load() == 0 && lease.fallbackLink != nil {
			lease.fallbackLink.Close()
			lease.fallbackLink = nil
			lease.fallbackReader = nil
		}
		lease.mu.Unlock()
	}
}

func (k *ucUnlimitedLeaseKeeper) cleanupIdle() {
	now := k.now()
	k.mu.Lock()
	defer k.mu.Unlock()
	for storageID, state := range k.storages {
		for key, lease := range state.leases {
			lease.mu.RLock()
			idle := now.Sub(lease.lastAccess) >= k.leaseIdleTTL
			dropped := lease.dropped
			lease.mu.RUnlock()
			if (idle || dropped) && lease.activeReaders.Load() == 0 {
				lease.mu.Lock()
				if lease.fallbackLink != nil {
					lease.fallbackLink.Close()
					lease.fallbackLink = nil
					lease.fallbackReader = nil
				}
				lease.mu.Unlock()
				delete(state.leases, key)
			}
		}
		if len(state.leases) == 0 && len(state.stokens) == 0 && state.leasePUUS == "" {
			delete(k.storages, storageID)
		}
	}
}

func (l *ucUnlimitedLease) updateRequest(params ucUnlimitedResolveParams) {
	l.mu.Lock()
	l.params.driver = params.driver
	l.params.provider = params.provider
	l.params.sharePwd = params.sharePwd
	l.params.parentFID = params.parentFID
	l.params.fidToken = params.fidToken
	l.params.size = params.size
	l.params.fallback = params.fallback
	l.lastAccess = l.keeper.now()
	l.mu.Unlock()
}

func (l *ucUnlimitedLease) needsRefresh(now time.Time) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return !l.dropped && !l.snapshot.refreshAt.IsZero() && !now.Before(l.snapshot.refreshAt)
}

func (k *ucUnlimitedLeaseKeeper) refreshLease(
	ctx context.Context,
	lease *ucUnlimitedLease,
	forceMeta bool,
) (*ucUnlimitedLease, error) {
	key := lease.params.key
	returnValue, err, _ := k.group.Do("refresh:"+key.string(), func() (*ucUnlimitedLease, error) {
		lease.mu.RLock()
		if lease.dropped {
			lease.mu.RUnlock()
			return nil, errors.New("share unlimited lease was dropped")
		}
		params := lease.params
		lease.mu.RUnlock()

		snapshot, resolveErr := k.resolveSnapshot(ctx, params, forceMeta)
		if resolveErr != nil && !forceMeta {
			snapshot, resolveErr = k.resolveSnapshot(ctx, params, true)
		}
		if resolveErr != nil {
			lease.mu.Lock()
			lease.consecutiveFailure++
			lease.lastAccess = k.now()
			lease.mu.Unlock()
			k.logLease("refresh_failed", lease, 0, false)
			return nil, resolveErr
		}
		lease.mu.Lock()
		snapshot.generation = lease.snapshot.generation + 1
		lease.snapshot = snapshot
		lease.params.fidToken = snapshot.fidToken
		lease.params.parentFID = snapshot.parentFID
		lease.params.size = snapshot.size
		lease.lastAccess = k.now()
		lease.consecutiveFailure = 0
		lease.mu.Unlock()
		k.logLease("refreshed", lease, 0, false)
		return lease, nil
	})
	return returnValue, err
}

func (k *ucUnlimitedLeaseKeeper) resolveSnapshot(
	ctx context.Context,
	params ucUnlimitedResolveParams,
	forceMeta bool,
) (ucUnlimitedSnapshot, error) {
	stoken, err := k.getStoken(ctx, params, false)
	if err != nil {
		return ucUnlimitedSnapshot{}, err
	}
	fidToken := params.fidToken
	parentFID := params.parentFID
	size := params.size

	downloadURL := ""
	downloadSize := int64(0)
	if !forceMeta && fidToken != "" {
		downloadURL, downloadSize, _ = k.requestDownload(ctx, params, stoken, params.key.fileID,
			fidToken)
		if downloadSize > 0 {
			size = downloadSize
		}
	}
	if downloadURL == "" {
		stoken, err = k.getStoken(ctx, params, true)
		if err != nil {
			return ucUnlimitedSnapshot{}, err
		}
		meta, metaErr := k.findFileMeta(ctx, params, stoken, params.key.fileID, parentFID)
		if metaErr != nil {
			return ucUnlimitedSnapshot{}, metaErr
		}
		fidToken = meta.FIDToken()
		parentFID = meta.ParentFID()
		if meta.Size > 0 {
			size = meta.Size
		}
		downloadURL, downloadSize, err = k.requestDownload(ctx, params, stoken, params.key.fileID,
			fidToken)
		if err != nil {
			return ucUnlimitedSnapshot{}, err
		}
		if downloadSize > 0 {
			size = downloadSize
		}
	}
	if downloadURL == "" {
		return ucUnlimitedSnapshot{}, fmt.Errorf("%s unlimited download URL is unavailable",
			params.provider.key)
	}
	probedSize, err := k.probe(ctx, params, downloadURL, size)
	if err != nil {
		return ucUnlimitedSnapshot{}, err
	}
	if size <= 0 {
		size = probedSize
	}
	now := k.now()
	return ucUnlimitedSnapshot{
		url:        downloadURL,
		fidToken:   fidToken,
		parentFID:  parentFID,
		size:       size,
		generation: 1,
		obtainedAt: now,
		refreshAt:  now.Add(k.refreshAfter),
	}, nil
}

func parseUCUnlimitedObjectID(raw string) (ucUnlimitedObjectID, error) {
	parts := strings.SplitN(raw, "-", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return ucUnlimitedObjectID{}, errors.New("UC unlimited object metadata is incomplete")
	}
	return ucUnlimitedObjectID{fileID: parts[0], fidToken: parts[1], parentFID: parts[2]}, nil
}

type ucUnlimitedTokenResponse struct {
	Status  int    `json:"status"`
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Stoken string `json:"stoken"`
	} `json:"data"`
}

type ucUnlimitedDownloadResponse struct {
	Status  int             `json:"status"`
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type ucUnlimitedFileMeta struct {
	FID             string `json:"fid"`
	FileID          string `json:"file_id"`
	ShareFIDToken   string `json:"share_fid_token"`
	FIDTokenValue   string `json:"fid_token"`
	ParentFIDValue  string `json:"pdir_fid"`
	ParentFIDLegacy string `json:"parent_fid"`
	Size            int64  `json:"size"`
	FileType        int    `json:"file_type"`
	File            bool   `json:"file"`
	Dir             bool   `json:"dir"`
}

func (m ucUnlimitedFileMeta) ID() string {
	if m.FID != "" {
		return m.FID
	}
	return m.FileID
}

func (m ucUnlimitedFileMeta) FIDToken() string {
	if m.ShareFIDToken != "" {
		return m.ShareFIDToken
	}
	return m.FIDTokenValue
}

func (m ucUnlimitedFileMeta) ParentFID() string {
	if m.ParentFIDValue != "" {
		return m.ParentFIDValue
	}
	if m.ParentFIDLegacy != "" {
		return m.ParentFIDLegacy
	}
	return "0"
}

func (m ucUnlimitedFileMeta) IsDirectory() bool {
	return m.Dir || (!m.File && m.FileType == 0)
}

type ucUnlimitedDetailResponse struct {
	Status  int    `json:"status"`
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		List []ucUnlimitedFileMeta `json:"list"`
	} `json:"data"`
	Metadata struct {
		Total int `json:"_total"`
	} `json:"metadata"`
}

func (k *ucUnlimitedLeaseKeeper) getStoken(
	ctx context.Context,
	params ucUnlimitedResolveParams,
	force bool,
) (string, error) {
	cacheKey := params.key.shareID + ":" + shortSensitiveDigest(params.sharePwd)
	now := k.now()
	if !force {
		k.mu.Lock()
		state := k.storageStateLocked(params.key.storageID)
		entry, ok := state.stokens[cacheKey]
		k.mu.Unlock()
		if ok && entry.value != "" && now.Before(entry.expiresAt) {
			return entry.value, nil
		}
	}

	var response ucUnlimitedTokenResponse
	body := map[string]any{"pwd_id": params.key.shareID, "passcode": params.sharePwd}
	if params.provider.key == "quark" {
		body["share_for_transfer"] = true
	}
	err := k.apiJSON(ctx, params, http.MethodPost, "/share/sharepage/token", nil, body, &response)
	if err != nil {
		return "", err
	}
	if err := validateUnlimitedAPIResponse(params.provider, response.Status, response.Code,
		response.Message); err != nil {
		return "", err
	}
	if response.Data.Stoken == "" {
		return "", fmt.Errorf("%s unlimited stoken is unavailable", params.provider.key)
	}
	k.mu.Lock()
	state := k.storageStateLocked(params.key.storageID)
	state.stokens[cacheKey] = ucUnlimitedStoken{
		value:     response.Data.Stoken,
		expiresAt: now.Add(k.stokenTTL),
	}
	k.mu.Unlock()
	return response.Data.Stoken, nil
}

func (k *ucUnlimitedLeaseKeeper) requestDownload(
	ctx context.Context,
	params ucUnlimitedResolveParams,
	stoken string,
	fileID string,
	fidToken string,
) (string, int64, error) {
	shareID := params.key.shareID
	if shareID == "" || stoken == "" || fileID == "" || fidToken == "" {
		return "", 0, fmt.Errorf("%s unlimited download metadata is incomplete",
			params.provider.key)
	}
	var response ucUnlimitedDownloadResponse
	body := map[string]any{
		"fids":       []string{fileID},
		"fids_token": []string{fidToken},
		"pwd_id":     shareID,
		"stoken":     stoken,
	}
	if params.provider.key == "uc" {
		body["speedup_session"] = ""
	}
	err := k.apiJSON(ctx, params, http.MethodPost, "/file/download", nil, body, &response)
	if err != nil {
		return "", 0, err
	}
	if err := validateUnlimitedAPIResponse(params.provider, response.Status, response.Code,
		response.Message); err != nil {
		return "", 0, err
	}
	var payload any
	if err := json.Unmarshal(response.Data, &payload); err != nil {
		return "", 0, fmt.Errorf("%s unlimited download response is invalid",
			params.provider.key)
	}
	downloadURL, downloadSize := findUnlimitedDownload(payload)
	if downloadURL == "" {
		return "", 0, fmt.Errorf("%s unlimited download response is empty",
			params.provider.key)
	}
	return downloadURL, downloadSize, nil
}

func findUnlimitedDownload(value any) (string, int64) {
	switch item := value.(type) {
	case []any:
		for _, entry := range item {
			if rawURL, size := findUnlimitedDownload(entry); rawURL != "" {
				return rawURL, size
			}
		}
	case map[string]any:
		for _, key := range []string{"download_url", "downloadUrl", "url"} {
			if rawURL, ok := item[key].(string); ok && rawURL != "" {
				size := numberAsInt64(item["size"])
				if size == 0 {
					size = numberAsInt64(item["file_size"])
				}
				return rawURL, size
			}
		}
		for _, nested := range item {
			if rawURL, size := findUnlimitedDownload(nested); rawURL != "" {
				return rawURL, size
			}
		}
	}
	return "", 0
}

func (k *ucUnlimitedLeaseKeeper) findFileMeta(
	ctx context.Context,
	params ucUnlimitedResolveParams,
	stoken string,
	fileID string,
	parentFID string,
) (ucUnlimitedFileMeta, error) {
	shareID := params.key.shareID
	queue := make([]string, 0, 8)
	if parentFID != "" && parentFID != "0" {
		queue = append(queue, parentFID)
	}
	queue = append(queue, "0")
	visited := make(map[string]struct{})

	for len(queue) > 0 && len(visited) < ucUnlimitedMaxDirs {
		current := queue[0]
		queue = queue[1:]
		if current == "" {
			continue
		}
		if _, exists := visited[current]; exists {
			continue
		}
		visited[current] = struct{}{}
		for page := 1; page <= ucUnlimitedMaxPages; page++ {
			var response ucUnlimitedDetailResponse
			query := url.Values{
				"pwd_id":   {shareID},
				"stoken":   {stoken},
				"pdir_fid": {current},
				"force":    {"0"},
				"_page":    {strconv.Itoa(page)},
				"_size":    {"100"},
				"_sort":    {"file_type:asc,file_name:asc"},
			}
			if err := k.apiJSON(ctx, params, http.MethodGet, "/share/sharepage/detail",
				query, nil, &response); err != nil {
				return ucUnlimitedFileMeta{}, err
			}
			if err := validateUnlimitedAPIResponse(params.provider, response.Status, response.Code,
				response.Message); err != nil {
				return ucUnlimitedFileMeta{}, err
			}
			for _, item := range response.Data.List {
				if item.ID() == fileID && !item.IsDirectory() {
					if item.ParentFIDValue == "" && item.ParentFIDLegacy == "" {
						item.ParentFIDValue = current
					}
					if item.FIDToken() == "" {
						return ucUnlimitedFileMeta{}, fmt.Errorf(
							"%s unlimited file token is unavailable", params.provider.key)
					}
					return item, nil
				}
				if item.IsDirectory() && item.ID() != "" {
					queue = append(queue, item.ID())
				}
			}
			total := response.Metadata.Total
			if len(response.Data.List) == 0 || page*100 >= total {
				break
			}
		}
	}
	return ucUnlimitedFileMeta{}, fmt.Errorf("%s unlimited file metadata was not found",
		params.provider.key)
}

func (k *ucUnlimitedLeaseKeeper) apiJSON(
	ctx context.Context,
	params ucUnlimitedResolveParams,
	method string,
	path string,
	query url.Values,
	body any,
	result any,
) error {
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	apiBase := k.apiBase
	if apiBase == "" {
		apiBase = params.provider.apiBase
	}
	target, err := url.Parse(strings.TrimRight(apiBase, "/") + path)
	if err != nil {
		return fmt.Errorf("%s unlimited API endpoint is invalid", params.provider.key)
	}
	values := target.Query()
	values.Set("pr", params.provider.pr)
	values.Set("fr", "pc")
	if params.provider.key == "quark" {
		values.Set("entry", "ft")
		values.Set("uc_param_str", "")
	}
	for key, entries := range query {
		for _, entry := range entries {
			values.Add(key, entry)
		}
	}
	target.RawQuery = values.Encode()

	var bodyReader io.Reader
	if body != nil {
		encoded, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			return fmt.Errorf("%s unlimited request body is invalid", params.provider.key)
		}
		bodyReader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(requestCtx, method, target.String(), bodyReader)
	if err != nil {
		return fmt.Errorf("%s unlimited API request could not be created", params.provider.key)
	}
	request.Header.Set("User-Agent", params.provider.userAgent)
	request.Header.Set("Referer", params.provider.referer)
	request.Header.Set("Accept", "application/json, text/plain, */*")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	apiCookie := ""
	if params.provider.key == "quark" {
		apiCookie = strings.TrimSpace(params.driver.APICookie)
		if apiCookie == "" {
			return errors.New("quark unlimited API Cookie is unavailable")
		}
	}
	k.mu.Lock()
	state := k.storageStateLocked(params.key.storageID)
	puus := state.leasePUUS
	if puus == "" && params.provider.key == "quark" {
		puus = cookiepkg.GetStr(apiCookie, "__puus")
		if puus != "" {
			state.leasePUUS = puus
		}
	}
	k.mu.Unlock()
	requestCookie := apiCookie
	if puus != "" {
		requestCookie = cookiepkg.SetStr(requestCookie, "__puus", puus)
	}
	if requestCookie != "" {
		request.Header.Set("Cookie", requestCookie)
	}

	response, err := k.client.Do(request)
	if err != nil {
		return fmt.Errorf("%s unlimited API request failed: %w", params.provider.key, err)
	}
	defer response.Body.Close()
	k.captureLeaseCookie(params.key.storageID, response)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%s unlimited API returned HTTP %d",
			params.provider.key, response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4<<20))
	if err := decoder.Decode(result); err != nil {
		return fmt.Errorf("%s unlimited API response is invalid", params.provider.key)
	}
	return nil
}

func (k *ucUnlimitedLeaseKeeper) captureLeaseCookie(
	storageID uint,
	response *http.Response,
) {
	for _, responseCookie := range response.Cookies() {
		if responseCookie.Name != "__puus" || responseCookie.Value == "" {
			continue
		}
		k.mu.Lock()
		state := k.storageStateLocked(storageID)
		state.leasePUUS = responseCookie.Value
		k.mu.Unlock()
		return
	}
}

func (k *ucUnlimitedLeaseKeeper) cdnLeaseCookie(
	params ucUnlimitedResolveParams,
) string {
	if params.provider.key != "quark" {
		return ""
	}
	k.mu.Lock()
	state := k.storages[params.key.storageID]
	puus := ""
	if state != nil {
		puus = state.leasePUUS
	}
	k.mu.Unlock()
	if puus == "" {
		return ""
	}
	return cookiepkg.SetStr("", "__puus", puus)
}

func validateUnlimitedAPIResponse(
	provider unlimitedProvider,
	status int,
	code int,
	message string,
) error {
	if status >= 400 || code != 0 {
		if message == "" {
			message = "request rejected"
		}
		return fmt.Errorf("%s unlimited API rejected request: %s", provider.key, message)
	}
	return nil
}

func (k *ucUnlimitedLeaseKeeper) probe(
	ctx context.Context,
	params ucUnlimitedResolveParams,
	rawURL string,
	expectedSize int64,
) (int64, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	response, err := k.cdnRequest(
		probeCtx, params, rawURL, http_range.Range{Start: 0, Length: 1}, true)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	start, end, total, err := parseStrictContentRange(response.Header.Get("Content-Range"))
	if response.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("%s unlimited Range probe returned HTTP %d",
			params.provider.key, response.StatusCode)
	}
	if err != nil || start != 0 || end != 0 {
		return 0, fmt.Errorf("%s unlimited Range probe returned invalid Content-Range",
			params.provider.key)
	}
	if response.ContentLength != 1 {
		return 0, fmt.Errorf("%s unlimited Range probe returned invalid Content-Length",
			params.provider.key)
	}
	if expectedSize > 0 && total != expectedSize {
		return 0, fmt.Errorf("%s unlimited Range probe returned an unexpected file size",
			params.provider.key)
	}
	buffer := make([]byte, 2)
	count, readErr := io.ReadFull(response.Body, buffer[:1])
	if count != 1 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
		return 0, fmt.Errorf("%s unlimited Range probe body is invalid",
			params.provider.key)
	}
	return total, nil
}

func (k *ucUnlimitedLeaseKeeper) cdnRequest(
	ctx context.Context,
	params ucUnlimitedResolveParams,
	rawURL string,
	requestRange http_range.Range,
	useRange bool,
) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%s unlimited CDN request could not be created",
			params.provider.key)
	}
	request.Header.Set("User-Agent", params.provider.userAgent)
	if params.provider.key == "uc" {
		// UC's anonymous share CDN rejects the generic drive page Referer. Its
		// PC share flow uses the signed URL plus a literal backslash.
		request.Header.Set("Referer", rawURL+"\\ ")
	} else {
		request.Header.Set("Referer", strings.TrimRight(params.provider.referer, "/")+"/")
		request.Header.Set("Origin", params.provider.origin)
		request.Header.Set("Accept", "*/*")
	}
	request.Header.Set("Accept-Encoding", "identity")
	if useRange {
		request.Header.Set("Range", formatByteRange(requestRange))
	}
	if params.provider.key == "quark" {
		leaseCookie := k.cdnLeaseCookie(params)
		if leaseCookie == "" {
			return nil, errors.New("quark unlimited CDN lease Cookie is unavailable")
		}
		// The complete MV account Cookie is API-only. Quark's CDN receives
		// exactly one short-lived lease field and no other account material.
		request.Header.Set("Cookie", leaseCookie)
	}
	return k.client.Do(request)
}

type ucUnlimitedRangeReader struct {
	lease *ucUnlimitedLease
}

func (r *ucUnlimitedRangeReader) RangeRead(
	ctx context.Context,
	requestRange http_range.Range,
) (io.ReadCloser, error) {
	if r == nil || r.lease == nil {
		return nil, errors.New("share unlimited RangeReader is unavailable")
	}
	lease := r.lease
	lease.mu.Lock()
	if lease.dropped {
		lease.mu.Unlock()
		return nil, errors.New("share unlimited lease was dropped")
	}
	size := lease.snapshot.size
	lease.lastAccess = lease.keeper.now()
	lease.mu.Unlock()

	normalized, full, err := normalizeUCUnlimitedRange(requestRange, size)
	if err != nil {
		return nil, err
	}
	if lease.needsRefresh(lease.keeper.now()) {
		_, _ = lease.keeper.refreshLease(ctx, lease, false)
	}
	body, status, err := lease.readCurrent(ctx, normalized, full)
	if err == nil {
		return body, nil
	}

	if status == http.StatusUnauthorized || status == http.StatusForbidden ||
		status == http.StatusNotFound || status == http.StatusGone {
		if _, refreshErr := lease.keeper.refreshLease(ctx, lease, true); refreshErr == nil {
			body, _, retryErr := lease.readCurrent(ctx, normalized, full)
			if retryErr == nil {
				return body, nil
			}
			err = retryErr
		}
	}
	lease.keeper.logLease("fallback", lease, status, true)
	if fallbackBody, fallbackErr := lease.readFallback(ctx, normalized); fallbackErr == nil {
		return fallbackBody, nil
	}
	return nil, err
}

func (l *ucUnlimitedLease) readCurrent(
	ctx context.Context,
	requestRange http_range.Range,
	full bool,
) (io.ReadCloser, int, error) {
	l.mu.RLock()
	rawURL := l.snapshot.url
	size := l.snapshot.size
	params := l.params
	dropped := l.dropped
	l.mu.RUnlock()
	if dropped || rawURL == "" {
		return nil, 0, errors.New("share unlimited lease has no active URL")
	}
	response, err := l.keeper.cdnRequest(ctx, params, rawURL, requestRange, !full)
	if err != nil {
		return nil, 0, err
	}
	if validationErr := validateRangeResponse(response, requestRange, size, full); validationErr != nil {
		status := response.StatusCode
		response.Body.Close()
		return nil, status, validationErr
	}
	l.activeReaders.Add(1)
	l.mu.Lock()
	l.lastAccess = l.keeper.now()
	l.consecutiveFailure = 0
	l.mu.Unlock()
	return &ucUnlimitedTrackedBody{
		ReadCloser: response.Body,
		onClose: func() {
			l.activeReaders.Add(-1)
		},
	}, response.StatusCode, nil
}

func (l *ucUnlimitedLease) readFallback(
	ctx context.Context,
	requestRange http_range.Range,
) (io.ReadCloser, error) {
	if err := l.ensureFallback(ctx); err != nil {
		return nil, err
	}
	l.mu.Lock()
	rangeReader := l.fallbackReader
	if rangeReader == nil {
		l.mu.Unlock()
		return nil, errors.New("UC traditional fallback reader is unavailable")
	}
	l.activeReaders.Add(1)
	l.mu.Unlock()
	body, err := rangeReader.RangeRead(ctx, requestRange)
	if err != nil {
		l.activeReaders.Add(-1)
		return nil, err
	}
	return &ucUnlimitedTrackedBody{
		ReadCloser: body,
		onClose: func() {
			l.activeReaders.Add(-1)
			l.mu.Lock()
			if l.dropped && l.activeReaders.Load() == 0 && l.fallbackLink != nil {
				l.fallbackLink.Close()
				l.fallbackLink = nil
				l.fallbackReader = nil
			}
			l.mu.Unlock()
		},
	}, nil
}

func (l *ucUnlimitedLease) ensureFallback(ctx context.Context) error {
	now := l.keeper.now()
	l.mu.RLock()
	if l.fallbackReader != nil &&
		(now.Before(l.fallbackUntil) || l.activeReaders.Load() > 0) {
		l.mu.RUnlock()
		return nil
	}
	key := l.params.key
	l.mu.RUnlock()
	_, err, _ := l.keeper.group.Do("fallback:"+key.string(), func() (*ucUnlimitedLease, error) {
		now := l.keeper.now()
		l.mu.RLock()
		if l.fallbackReader != nil &&
			(now.Before(l.fallbackUntil) || l.activeReaders.Load() > 0) {
			l.mu.RUnlock()
			return l, nil
		}
		fallback := l.params.fallback
		size := l.snapshot.size
		dropped := l.dropped
		l.mu.RUnlock()
		if dropped || fallback == nil {
			return nil, errors.New("UC traditional fallback is unavailable")
		}
		link, resolveErr := fallback(ctx)
		if resolveErr != nil || link == nil {
			return nil, errors.New("UC traditional fallback failed")
		}
		rangeReader, readerErr := stream.GetRangeReaderFromLink(size, link)
		if readerErr != nil {
			link.Close()
			return nil, errors.New("UC traditional fallback link is invalid")
		}
		l.mu.Lock()
		if l.fallbackLink != nil && l.activeReaders.Load() == 0 {
			l.fallbackLink.Close()
		}
		l.fallbackLink = link
		l.fallbackReader = rangeReader
		l.fallbackUntil = now.Add(5 * time.Minute)
		l.mu.Unlock()
		return l, nil
	})
	return err
}

type ucUnlimitedTrackedBody struct {
	io.ReadCloser
	once    sync.Once
	onClose func()
}

func (b *ucUnlimitedTrackedBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.onClose)
	return err
}

func normalizeUCUnlimitedRange(requestRange http_range.Range, size int64) (http_range.Range, bool, error) {
	if size <= 0 || requestRange.Start < 0 || requestRange.Start >= size {
		return http_range.Range{}, false, http_range.ErrNoOverlap
	}
	length := requestRange.Length
	if length < 0 || requestRange.Start+length > size {
		length = size - requestRange.Start
	}
	if length <= 0 {
		return http_range.Range{}, false, http_range.ErrInvalid
	}
	normalized := http_range.Range{Start: requestRange.Start, Length: length}
	return normalized, normalized.Start == 0 && normalized.Length == size, nil
}

func validateRangeResponse(
	response *http.Response,
	requestRange http_range.Range,
	size int64,
	full bool,
) error {
	if full && response.StatusCode == http.StatusOK {
		if response.ContentLength >= 0 && response.ContentLength != requestRange.Length {
			return errors.New("UC unlimited full response length mismatch")
		}
		return nil
	}
	if response.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("UC unlimited upstream returned HTTP %d", response.StatusCode)
	}
	start, end, total, err := parseStrictContentRange(response.Header.Get("Content-Range"))
	if err != nil || start != requestRange.Start ||
		end != requestRange.Start+requestRange.Length-1 ||
		total != size {
		return errors.New("UC unlimited Content-Range mismatch")
	}
	if response.ContentLength >= 0 && response.ContentLength != requestRange.Length {
		return errors.New("UC unlimited partial response length mismatch")
	}
	return nil
}

func parseStrictContentRange(value string) (int64, int64, int64, error) {
	if !strings.HasPrefix(value, "bytes ") {
		return 0, 0, 0, http_range.ErrInvalid
	}
	rangeAndTotal := strings.TrimPrefix(value, "bytes ")
	rangePart, totalPart, ok := strings.Cut(rangeAndTotal, "/")
	if !ok || totalPart == "*" {
		return 0, 0, 0, http_range.ErrInvalid
	}
	startPart, endPart, ok := strings.Cut(rangePart, "-")
	if !ok {
		return 0, 0, 0, http_range.ErrInvalid
	}
	start, startErr := strconv.ParseInt(startPart, 10, 64)
	end, endErr := strconv.ParseInt(endPart, 10, 64)
	total, totalErr := strconv.ParseInt(totalPart, 10, 64)
	if startErr != nil || endErr != nil || totalErr != nil ||
		start < 0 || end < start || total <= end {
		return 0, 0, 0, http_range.ErrInvalid
	}
	return start, end, total, nil
}

func formatByteRange(requestRange http_range.Range) string {
	return "bytes=" + strconv.FormatInt(requestRange.Start, 10) + "-" +
		strconv.FormatInt(requestRange.Start+requestRange.Length-1, 10)
}

func (k *ucUnlimitedLeaseKeeper) logLease(
	action string,
	lease *ucUnlimitedLease,
	status int,
	fallback bool,
) {
	lease.mu.RLock()
	key := lease.params.key
	snapshot := lease.snapshot
	failures := lease.consecutiveFailure
	lease.mu.RUnlock()
	host := ""
	if parsed, err := url.Parse(snapshot.url); err == nil {
		host = parsed.Host
	}
	log.WithFields(log.Fields{
		"action":     action,
		"storage_id": key.storageID,
		"share":      shortSensitiveDigest(key.shareID),
		"file":       shortSensitiveDigest(key.fileID),
		"size":       snapshot.size,
		"generation": snapshot.generation,
		"status":     status,
		"host":       host,
		"failures":   failures,
		"fallback":   fallback,
	}).Info("UC unlimited lease event")
}

func shortSensitiveDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:4])
}

func classifyUCUnlimitedError(err error) string {
	if err == nil {
		return "unknown"
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "Range probe returned HTTP 401"):
		return "range_probe_http_401"
	case strings.Contains(message, "Range probe returned HTTP 403"):
		return "range_probe_http_403"
	case strings.Contains(message, "Range probe returned HTTP 404"):
		return "range_probe_http_404"
	case strings.Contains(message, "Range probe returned HTTP 410"):
		return "range_probe_http_410"
	case strings.Contains(message, "invalid Content-Range"):
		return "range_probe_content_range_invalid"
	case strings.Contains(message, "invalid Content-Length"):
		return "range_probe_content_length_invalid"
	case strings.Contains(message, "Range probe"):
		return "range_probe_invalid"
	case strings.Contains(message, "unexpected file size"):
		return "range_probe_size_mismatch"
	case strings.Contains(message, "download URL"):
		return "download_url_unavailable"
	case strings.Contains(message, "file metadata"):
		return "file_metadata_unavailable"
	case strings.Contains(message, "file token"):
		return "file_token_unavailable"
	case strings.Contains(message, "stoken"):
		return "stoken_unavailable"
	case strings.Contains(message, "HTTP 401"):
		return "api_http_401"
	case strings.Contains(message, "HTTP 403"):
		return "api_http_403"
	case strings.Contains(message, "HTTP 404"):
		return "api_http_404"
	case strings.Contains(message, "HTTP 429"):
		return "api_http_429"
	case strings.Contains(message, "HTTP 5"):
		return "api_http_5xx"
	case strings.Contains(message, "request failed"):
		return "request_failed"
	default:
		return "upstream_rejected"
	}
}

func numberAsInt64(value any) int64 {
	switch number := value.(type) {
	case float64:
		return int64(number)
	case json.Number:
		result, _ := number.Int64()
		return result
	case int64:
		return number
	case int:
		return int64(number)
	default:
		return 0
	}
}
