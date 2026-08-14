package guangyapan_share

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/drivers/guangyapan"
	"github.com/OpenListTeam/OpenList/v4/internal/cache"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/OpenListTeam/OpenList/v4/pkg/singleflight"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
)

const guangYaPanAPIBaseURL = "https://api.guangyapan.com"

var (
	guangYaPanShareLinkCache = cache.NewKeyedCache[*model.Link](time.Hour)
	guangYaPanPlaybackFiles  = cache.NewKeyedCache[preparedPlaybackFile](24 * time.Hour)
	guangYaPanPlaybackGroup  singleflight.Group[preparedPlaybackFile]
	guangYaPanShareIDRegexp  = regexp.MustCompile(`^[A-Za-z0-9]+_[A-Za-z0-9_-]+$`)
	guangYaPanShareURLRegexp = regexp.MustCompile(`https?://(?:www\.)?guangyapan\.com/s/([A-Za-z0-9_-]+)`)
	guangYaPanAccountIdx     = 0
)

var resolveGuangYaPanShareLink = func(ctx context.Context, d *GuangYaPanShare, file model.Obj, args model.LinkArgs) (*model.Link, error) {
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

type GuangYaPanShare struct {
	model.Storage
	Addition

	client *resty.Client
}

type preparedPlaybackFile struct {
	FileID string
	Name   string
	Size   int64
}

func (d *GuangYaPanShare) accountCount() int {
	if d.AccountStorageID > 0 {
		return 1
	}
	return op.GetDriverCount("GuangYaPan")
}

func (d *GuangYaPanShare) account(ctx context.Context, index int) (*guangyapan.GuangYaPan, error) {
	var storage driver.Driver
	if d.AccountStorageID > 0 {
		storage = op.GetDriverByStorageID("GuangYaPan", d.AccountStorageID)
	} else {
		storage = op.GetFirstDriver("GuangYaPan", index)
	}
	if storage == nil {
		return nil, errors.New("找不到光鸭网盘帐号")
	}
	account, ok := storage.(*guangyapan.GuangYaPan)
	if !ok {
		return nil, errors.New("光鸭网盘帐号类型错误")
	}
	if err := account.Init(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(account.AccessToken) == "" {
		return nil, errors.New("光鸭网盘帐号未登录")
	}
	return account, nil
}

func (d *GuangYaPanShare) Config() driver.Config {
	return config
}

func (d *GuangYaPanShare) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *GuangYaPanShare) Init(ctx context.Context) error {
	d.ShareID = normalizeShareID(d.ShareID)
	if d.ShareID == "" {
		return errors.New("invalid guangyapan share_id")
	}
	d.DeviceID = normalizeDeviceID(d.DeviceID)
	if d.DeviceID == "" {
		d.DeviceID = randomDeviceID()
	}
	if d.PageSize <= 0 {
		d.PageSize = 200
	}
	if d.OrderBy < 0 {
		d.OrderBy = 0
	}
	if d.SortType != 0 && d.SortType != 1 {
		d.SortType = 0
	}
	d.client = newBizClient(d.DeviceID, "")

	if conf.LazyLoad && !conf.StoragesLoaded {
		return nil
	}

	return d.Validate()
}

func (d *GuangYaPanShare) Drop(ctx context.Context) error {
	return nil
}

func (d *GuangYaPanShare) Validate() error {
	return d.getShareAccessToken(context.Background())
}

func (d *GuangYaPanShare) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	if d.ShareAccessToken == "" {
		if err := d.getShareAccessToken(ctx); err != nil {
			return nil, err
		}
	}

	parentID := strings.TrimSpace(dir.GetID())
	cursor := 0
	items := make([]model.Obj, 0, d.PageSize)

	for {
		var out shareListResp
		body := map[string]any{
			"pageSize":    d.PageSize,
			"accessToken": d.ShareAccessToken,
			"parentId":    parentID,
			"orderBy":     d.OrderBy,
			"sortType":    d.SortType,
		}
		if cursor > 0 {
			body["cursor"] = cursor
		}
		if err := d.postShareAPI(ctx, "/userres/v1/get_share_page_files_list", body, &out); err != nil {
			if isShareTokenError(err) {
				if refreshErr := d.getShareAccessToken(ctx); refreshErr != nil {
					return nil, refreshErr
				}
				body["accessToken"] = d.ShareAccessToken
				if retryErr := d.postShareAPI(ctx, "/userres/v1/get_share_page_files_list", body, &out); retryErr != nil {
					return nil, retryErr
				}
			} else {
				return nil, err
			}
		}
		for _, item := range out.Data.List {
			items = append(items, fileToObj(item))
		}
		if len(out.Data.List) < d.PageSize {
			break
		}
		if out.Data.Total > 0 && len(items) >= out.Data.Total {
			break
		}
		if out.Data.Cursor <= cursor {
			break
		}
		cursor = out.Data.Cursor
	}

	return items, nil
}

func (d *GuangYaPanShare) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	if file.IsDir() {
		return nil, errs.NotFile
	}
	key := shareLinkCacheKey(d.ShareID, file.GetID(), d.AccountStorageID)
	log.Debugf("GuangYaPanShare share: %v %v", d.ShareID, file.GetID())
	if link, ok := guangYaPanShareLinkCache.Get(key); ok {
		return link, nil
	}

	link, err := resolveGuangYaPanShareLink(ctx, d, file, args)
	if err != nil {
		log.Infof("GuangYaPanShare link error: %v", err)
	}
	if err == nil && link != nil {
		guangYaPanShareLinkCache.Set(key, link)
	}
	return link, err
}

// Other exposes only provider metadata needed by MV's rapid-transfer planner.
// The share object must first be restored into the selected Guangya account;
// get_file_detail on that restored object is the authoritative MD5 source.
func (d *GuangYaPanShare) Other(ctx context.Context, args model.OtherArgs) (interface{}, error) {
	switch args.Method {
	case "rapid_transfer_metadata":
		return d.rapidTransferMetadata(ctx, args)
	case "prepare_playback":
		return d.preparePlayback(ctx, args)
	case "refresh_playback":
		return d.refreshPlayback(ctx, args)
	case "cleanup_playback":
		return d.cleanupPlayback(ctx, args)
	default:
		return nil, errs.NotSupport
	}
}

func (d *GuangYaPanShare) rapidTransferMetadata(ctx context.Context, args model.OtherArgs) (interface{}, error) {
	if args.Obj == nil || args.Obj.IsDir() {
		return nil, errs.NotFile
	}
	account, err := d.account(ctx, guangYaPanAccountIdx)
	guangYaPanAccountIdx++
	if err != nil {
		return nil, err
	}
	if d.ShareAccessToken == "" {
		if err := d.getShareAccessToken(ctx); err != nil {
			return nil, err
		}
	}
	taskID, err := d.restoreShare(ctx, account, args.Obj.GetID())
	if err != nil {
		return nil, err
	}
	if err := d.waitTaskDone(ctx, account, taskID); err != nil {
		return nil, err
	}
	restored, err := d.resolveRestoredFile(ctx, account, taskID, args.Obj)
	if err != nil {
		return nil, err
	}
	go d.delete(context.Background(), restored, account)
	md5, err := account.GetFileDetail(ctx, restored.GetID())
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"name": restored.GetName(),
		"size": restored.GetSize(),
		"md5":  md5,
	}, nil
}

func (d *GuangYaPanShare) preparePlayback(ctx context.Context, args model.OtherArgs) (interface{}, error) {
	if args.Obj == nil || args.Obj.IsDir() {
		return nil, errs.NotFile
	}
	data := otherData(args.Data)
	sessionKey := strings.TrimSpace(stringValue(data["session_key"]))
	cacheKey := playbackFileCacheKey(sessionKey, d.AccountStorageID)
	account, err := d.account(ctx, 0)
	if err != nil {
		return nil, err
	}
	var saved preparedPlaybackFile
	if cacheKey != "" {
		saved, err, _ = guangYaPanPlaybackGroup.Do(
			cacheKey,
			func() (preparedPlaybackFile, error) {
				if cached, ok := guangYaPanPlaybackFiles.Get(cacheKey); ok && cached.FileID != "" {
					return cached, nil
				}
				prepared, prepareErr := d.preparePlaybackFile(ctx, account, args.Obj)
				if prepareErr != nil {
					return preparedPlaybackFile{}, prepareErr
				}
				guangYaPanPlaybackFiles.Set(cacheKey, prepared)
				return prepared, nil
			},
		)
	} else {
		saved, err = d.preparePlaybackFile(ctx, account, args.Obj)
	}
	if err != nil {
		return nil, err
	}
	file := &model.Object{
		ID:   saved.FileID,
		Name: saved.Name,
		Size: saved.Size,
	}
	link, err := account.Link(ctx, file, model.LinkArgs{})
	if err != nil {
		if cacheKey != "" {
			guangYaPanPlaybackFiles.Delete(cacheKey)
		}
		return nil, err
	}
	return playbackLeaseResult(file, link), nil
}

func (d *GuangYaPanShare) preparePlaybackFile(
	ctx context.Context,
	account *guangyapan.GuangYaPan,
	source model.Obj,
) (preparedPlaybackFile, error) {
	if d.ShareAccessToken == "" {
		if err := d.getShareAccessToken(ctx); err != nil {
			return preparedPlaybackFile{}, err
		}
	}
	taskID, err := d.restoreShare(ctx, account, source.GetID())
	if err != nil {
		return preparedPlaybackFile{}, err
	}
	if err := d.waitTaskDone(ctx, account, taskID); err != nil {
		return preparedPlaybackFile{}, err
	}
	restored, err := d.resolveRestoredFile(ctx, account, taskID, source)
	if err != nil {
		return preparedPlaybackFile{}, err
	}
	go d.delete(context.Background(), restored, account)
	return preparedPlaybackFile{
		FileID: restored.GetID(),
		Name:   restored.GetName(),
		Size:   restored.GetSize(),
	}, nil
}

func (d *GuangYaPanShare) refreshPlayback(ctx context.Context, args model.OtherArgs) (interface{}, error) {
	data := otherData(args.Data)
	fileID := strings.TrimSpace(stringValue(data["file_id"]))
	if fileID == "" {
		return nil, errors.New("refresh playback requires file_id")
	}
	account, err := d.account(ctx, 0)
	if err != nil {
		return nil, err
	}
	file := &model.Object{
		ID:   fileID,
		Name: strings.TrimSpace(stringValue(data["name"])),
		Size: int64Value(data["size"]),
	}
	link, err := account.Link(ctx, file, model.LinkArgs{})
	if err != nil {
		return nil, err
	}
	return playbackLeaseResult(file, link), nil
}

func (d *GuangYaPanShare) cleanupPlayback(ctx context.Context, args model.OtherArgs) (interface{}, error) {
	data := otherData(args.Data)
	fileID := strings.TrimSpace(stringValue(data["file_id"]))
	if fileID == "" {
		return nil, errors.New("cleanup playback requires file_id")
	}
	cacheKey := playbackFileCacheKey(
		strings.TrimSpace(stringValue(data["session_key"])),
		d.AccountStorageID,
	)
	if cacheKey != "" {
		defer guangYaPanPlaybackFiles.Delete(cacheKey)
	}
	account, err := d.account(ctx, 0)
	if err != nil {
		return nil, err
	}
	file := &model.Object{
		ID:   fileID,
		Name: strings.TrimSpace(stringValue(data["name"])),
		Size: int64Value(data["size"]),
	}
	removeErr := account.Remove(ctx, file)
	if removeErr != nil {
		return nil, removeErr
	}
	return map[string]interface{}{"removed": true}, nil
}

func (d *GuangYaPanShare) link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	account, err := d.account(ctx, guangYaPanAccountIdx)
	guangYaPanAccountIdx++
	if err != nil {
		return nil, err
	}
	if d.ShareAccessToken == "" {
		if err := d.getShareAccessToken(ctx); err != nil {
			return nil, err
		}
	}
	log.Infof("[%v] 获取光鸭文件直链 %v %v %v", account.ID, file.GetName(), file.GetID(), file.GetSize())

	taskID, err := d.restoreShare(ctx, account, file.GetID())
	log.Debugf("RestoreShare task id: %v", taskID)
	if err != nil {
		log.Infof("RestoreShare error: %v", err)
		return nil, err
	}

	if err := d.waitTaskDone(ctx, account, taskID); err != nil {
		log.Infof("waitTaskDone error: %v", err)
		return nil, err
	}

	restored, err := d.resolveRestoredFile(ctx, account, taskID, file)
	log.Debugf("Restored: %+v", restored)
	if err != nil {
		log.Infof("restored error: %v", err)
		return nil, err
	}

	link, err := account.Link(ctx, restored, args)
	if err != nil {
		log.Infof("link error: %v", err)
		return nil, err
	}

	go d.delete(context.Background(), restored, account)

	return link, nil
}

func (d *GuangYaPanShare) delete(ctx context.Context, file model.Obj, bd *guangyapan.GuangYaPan) {
	delayTime := setting.GetInt(conf.DeleteDelayTime, 900)
	if delayTime == 0 {
		return
	}

	delayTime += 5

	log.Infof("[%v] Delete GuangYa temp file %v after %v seconds.", bd.ID, file.GetID(), delayTime)
	time.Sleep(time.Duration(delayTime) * time.Second)
	bd.Remove(ctx, file)
}

func (d *GuangYaPanShare) getShareAccessToken(ctx context.Context) error {
	d.DeviceID = normalizeDeviceID(d.DeviceID)
	if d.DeviceID == "" {
		d.DeviceID = randomDeviceID()
	}
	if d.client == nil {
		d.client = newBizClient(d.DeviceID, "")
	}
	var out shareAccessTokenResp
	if err := d.postShareAPI(ctx, "/userres/v1/get_share_access_token", map[string]any{
		"shareId": d.ShareID,
		"code":    d.SharePwd,
	}, &out); err != nil {
		return err
	}
	token := strings.TrimSpace(out.Data.AccessToken)
	if token == "" {
		return errors.New("empty share access token")
	}
	d.ShareAccessToken = token
	return nil
}

func (d *GuangYaPanShare) postShareAPI(ctx context.Context, apiPath string, body any, out any) error {
	resp, err := d.client.R().
		SetContext(ctx).
		SetBody(body).
		SetResult(out).
		Post(apiPath)
	if err != nil {
		return err
	}
	if resp.IsError() {
		return fmt.Errorf("request failed: status=%d body=%s", resp.StatusCode(), resp.String())
	}

	switch typed := out.(type) {
	case *shareAccessTokenResp:
		if !strings.EqualFold(strings.TrimSpace(typed.Msg), "success") {
			return fmt.Errorf("get share access token failed: %s", strings.TrimSpace(typed.Msg))
		}
	case *shareListResp:
		if !strings.EqualFold(strings.TrimSpace(typed.Msg), "success") {
			if strings.Contains(strings.ToLower(typed.Msg), "token") {
				d.ShareAccessToken = ""
			}
			return fmt.Errorf("list share files failed: %s", strings.TrimSpace(typed.Msg))
		}
	}
	return nil
}

func (d *GuangYaPanShare) restoreShare(ctx context.Context, account *guangyapan.GuangYaPan, fileID string) (string, error) {
	body := restoreShareBody(
		d.ShareAccessToken,
		fileID,
		account.TempDirId,
		account.ClientID,
	)
	var out restoreShareResp
	if err := d.postAccountAPI(ctx, account, "/userres/v1/restore_share", body, &out); err != nil {
		return "", err
	}
	if out.Code == 219 {
		log.Debugf("restore share success: %v", out.Msg)
		return "", nil
	}
	if !strings.EqualFold(strings.TrimSpace(out.Msg), "success") && isShareTokenMessage(out.Msg) {
		if err := d.getShareAccessToken(ctx); err != nil {
			return "", err
		}
		body["accessToken"] = d.ShareAccessToken
		if err := d.postAccountAPI(ctx, account, "/userres/v1/restore_share", body, &out); err != nil {
			return "", err
		}
	}
	if !strings.EqualFold(strings.TrimSpace(out.Msg), "success") {
		return "", fmt.Errorf("restore share failed: %s", strings.TrimSpace(out.Msg))
	}
	if strings.TrimSpace(out.Data.TaskID) == "" {
		return "", errors.New("restore share failed: empty task id")
	}
	return strings.TrimSpace(out.Data.TaskID), nil
}

func restoreShareBody(accessToken, fileID, parentID, clientID string) map[string]any {
	return map[string]any{
		"accessToken": strings.TrimSpace(accessToken),
		"fileIds":     []string{strings.TrimSpace(fileID)},
		"parentId":    strings.TrimSpace(parentID),
		"clientId":    strings.TrimSpace(clientID),
	}
}

func (d *GuangYaPanShare) waitTaskDone(ctx context.Context, account *guangyapan.GuangYaPan, taskID string) error {
	if taskID == "" {
		return nil
	}
	const (
		maxTry   = 20
		interval = 500 * time.Millisecond
	)
	for i := 0; i < maxTry; i++ {
		var out taskStatusResp
		if err := d.postAccountAPI(ctx, account, "/userres/v1/get_task_status", map[string]any{
			"taskId": taskID,
		}, &out); err != nil {
			return err
		}
		if !strings.EqualFold(strings.TrimSpace(out.Msg), "success") {
			return fmt.Errorf("get task status failed: %s", strings.TrimSpace(out.Msg))
		}
		switch out.Data.Status {
		case 2:
			return nil
		case -1, 3:
			return fmt.Errorf("task %s failed with status=%d", taskID, out.Data.Status)
		}
		if i == maxTry-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
	return fmt.Errorf("task %s timeout", taskID)
}

func (d *GuangYaPanShare) resolveRestoredFile(ctx context.Context, account *guangyapan.GuangYaPan, taskID string, src model.Obj) (model.Obj, error) {
	if taskID != "" {
		if fileID, err := d.getTaskFileID(ctx, account, taskID); err == nil && fileID != "" {
			return &model.Object{
				ID:       fileID,
				Name:     src.GetName(),
				Size:     src.GetSize(),
				IsFolder: src.IsDir(),
			}, nil
		}
	}

	parentID := strings.TrimSpace(account.TempDirId)
	files, err := account.List(ctx, &model.Object{ID: parentID, IsFolder: true}, model.ListArgs{})
	if err != nil {
		return nil, err
	}
	file, ok := findRestoredFile(files, src)
	if !ok {
		return nil, fmt.Errorf("restored file not found for %s", src.GetName())
	}
	return file, nil
}

func (d *GuangYaPanShare) getTaskFileID(ctx context.Context, account *guangyapan.GuangYaPan, taskID string) (string, error) {
	var out taskInfoResp
	if err := d.postAccountAPI(ctx, account, "/userres/v1/file/get_info_by_task_id", map[string]any{
		"taskId": taskID,
	}, &out); err != nil {
		return "", err
	}
	log.Debugf("get task info: %+v", out)
	return strings.TrimSpace(out.Data.FileID), nil
}

func (d *GuangYaPanShare) postAccountAPI(ctx context.Context, account *guangyapan.GuangYaPan, apiPath string, body any, out any) error {
	for attempt := 0; attempt < 2; attempt++ {
		client := newBizClient(accountDeviceID(account, d.DeviceID), strings.TrimSpace(account.AccessToken))
		resp, err := client.R().
			SetContext(ctx).
			SetBody(body).
			SetResult(out).
			Post(apiPath)
		if err != nil {
			return err
		}
		if resp.StatusCode() == http.StatusUnauthorized || resp.StatusCode() == http.StatusForbidden {
			if err := account.Init(ctx); err != nil {
				return err
			}
			continue
		}
		if resp.IsError() {
			return fmt.Errorf("request failed: status=%d body=%s", resp.StatusCode(), resp.String())
		}
		return nil
	}
	return errors.New("request failed after token refresh")
}

func newBizClient(deviceID, accessToken string) *resty.Client {
	client := base.NewRestyClient().
		SetBaseURL(guangYaPanAPIBaseURL).
		SetHeader("Accept", "application/json, text/plain, */*").
		SetHeader("Content-Type", "application/json").
		SetHeader("Origin", "https://www.guangyapan.com").
		SetHeader("Referer", "https://www.guangyapan.com/").
		SetHeader("Did", deviceID).
		SetHeader("Dt", "4")
	if accessToken != "" {
		client.SetHeader("Authorization", "Bearer "+accessToken)
	}
	return client
}

func accountDeviceID(account *guangyapan.GuangYaPan, fallback string) string {
	deviceID := normalizeDeviceID(strings.TrimSpace(account.DeviceID))
	if deviceID != "" {
		return deviceID
	}
	deviceID = normalizeDeviceID(fallback)
	if deviceID != "" {
		return deviceID
	}
	return randomDeviceID()
}

func shareLinkCacheKey(shareID, fileID string, accountStorageID uint) string {
	return fmt.Sprintf(
		"%s|%s|%d",
		strings.TrimSpace(shareID),
		strings.TrimSpace(fileID),
		accountStorageID,
	)
}

func playbackFileCacheKey(sessionKey string, accountStorageID uint) string {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" || accountStorageID == 0 {
		return ""
	}
	return fmt.Sprintf("%s|%d", sessionKey, accountStorageID)
}

func playbackLeaseResult(file model.Obj, link *model.Link) map[string]interface{} {
	if link == nil {
		return map[string]interface{}{
			"file_id": file.GetID(),
			"name":    file.GetName(),
			"size":    file.GetSize(),
		}
	}
	expiresIn := int64(0)
	if link.Expiration != nil {
		expiresIn = int64(link.Expiration.Seconds())
	}
	return map[string]interface{}{
		"file_id":    file.GetID(),
		"name":       file.GetName(),
		"size":       file.GetSize(),
		"url":        link.URL,
		"header":     link.Header,
		"expires_in": expiresIn,
	}
}

func otherData(value interface{}) map[string]interface{} {
	if typed, ok := value.(map[string]interface{}); ok {
		return typed
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return map[string]interface{}{}
	}
	var result map[string]interface{}
	if err := json.Unmarshal(raw, &result); err != nil {
		return map[string]interface{}{}
	}
	return result
}

func stringValue(value interface{}) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func int64Value(value interface{}) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case float64:
		return int64(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return parsed
	default:
		var parsed int64
		_, _ = fmt.Sscan(stringValue(value), &parsed)
		return parsed
	}
}

func findRestoredFile(files []model.Obj, target model.Obj) (model.Obj, bool) {
	targetName := strings.TrimSpace(target.GetName())
	targetSize := target.GetSize()
	log.Debugf("try to find restored file: %s %v", targetName, targetSize)
	var fallback model.Obj
	for _, file := range files {
		if file == nil || file.IsDir() {
			continue
		}
		if strings.TrimSpace(file.GetName()) != targetName {
			continue
		}
		if fallback == nil || file.ModTime().After(fallback.ModTime()) {
			fallback = file
		}
		if targetSize > 0 && file.GetSize() != targetSize {
			continue
		}
		if fallback == file {
			continue
		}
	}
	if targetSize > 0 {
		var exact model.Obj
		for _, file := range files {
			if file == nil || file.IsDir() {
				continue
			}
			if strings.TrimSpace(file.GetName()) != targetName || file.GetSize() != targetSize {
				continue
			}
			if exact == nil || file.ModTime().After(exact.ModTime()) {
				exact = file
			}
		}
		if exact != nil {
			return exact, true
		}
	}
	if fallback != nil {
		return fallback, true
	}
	return nil, false
}

func normalizeShareID(input string) string {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return ""
	}
	if matches := guangYaPanShareURLRegexp.FindStringSubmatch(raw); len(matches) >= 2 {
		raw = matches[1]
	}
	raw = strings.TrimSuffix(raw, "/")
	if guangYaPanShareIDRegexp.MatchString(raw) {
		return raw
	}
	return ""
}

func isShareTokenError(err error) bool {
	if err == nil {
		return false
	}
	return isShareTokenMessage(err.Error())
}

func isShareTokenMessage(msg string) bool {
	lower := strings.ToLower(strings.TrimSpace(msg))
	return strings.Contains(lower, "access token") || strings.Contains(lower, "accesstoken") || strings.Contains(lower, "token expired")
}

func normalizeDeviceID(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	v = strings.TrimPrefix(v, "wdi10.")
	v = strings.ReplaceAll(v, "-", "")
	if len(v) != 32 {
		return ""
	}
	for _, ch := range v {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return ""
		}
	}
	return v
}

func randomDeviceID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "0123456789abcdef0123456789abcdef"
	}
	return hex.EncodeToString(b)
}

var _ driver.Driver = (*GuangYaPanShare)(nil)
