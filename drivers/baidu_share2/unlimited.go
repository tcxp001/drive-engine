package baidu_share

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/cookie"
)

// 百度原画(无限) 免转存直链。
//
// 通过带签名的 share/list?origin=dlna 接口直接换取分享文件 dlink，跟一次 302 后
// 得到 d.pcs.baidu.com CDN 上的最终直链。实测：最终直链只需 dlna UA 即可播、
// 免 Cookie、Range 完美支持、无 100 秒试看限制，且不受“每账号 dlink 限速”影响。
// 因此把它作为分享播放的首选路径；失败时驱动回退到 saveFile 转存-删除方案。
const (
	baiduDlnaUA      = "netdisk;P2SP;2.2.91.136;android-android;"
	baiduDlnaDevUID  = "73CED981D0F186D12BC18CAE1684FFD5|VSRCQTF6W"
	baiduDlnaChannel = "android_12_zhao_bd-netdisk_1024266h"
	baiduDlnaVersion = "11.30.2"
	// dlna 请求签名的固定盐(参考实现里两段固定字符串)。
	baiduDlnaSaltA       = "ebrcUYiuxaZv2XGu7KIYKxUrqfnOfpDF"
	baiduDlnaSaltBSuffix = "ae5821440fab5e1a61a025f014bd8972"

	baiduWebUA       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
	baiduDlnaLinkTTL = time.Hour
)

// baiduHTTPDoer 抽象 http.Client，便于单测注入。
type baiduHTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// dlna 直链的最终地址靠一次 302 拿到，因此客户端必须“不自动跟随重定向”，
// 这样 share/list 返回的 JSON 与 dlink 的 302 Location 都能原样读取。
var baiduUnlimitedHTTPClient baiduHTTPDoer = &http.Client{
	Timeout: 20 * time.Second,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

var baiduUIDPattern = regexp.MustCompile(`"uid"\s*:\s*"?([0-9]+)"?`)

// baiduUnlimitedParams 是解析一条 dlna 直链所需的全部输入。
type baiduUnlimitedParams struct {
	shareID string // 分享 shareid
	shareUK string // 分享 share_uk
	sekey   string // 分享专属 sekey，原始(URL-encoded)形态，即 BDCLND / randsk
	fileID  string // 目标文件 fs_id
	cookie  string // 百度网盘账号 Cookie(须含 BDUSS)
}

type baiduDlnaAPIError struct {
	errno int64
}

func (e *baiduDlnaAPIError) Error() string {
	if e.errno == -6 {
		return "[baidu_login_invalid] 百度账号未登录: errno=-6"
	}
	if e.errno == -9 {
		return "百度原画(无限)请求失败: errno=-9（文件不在当前分享校验上下文）"
	}
	return fmt.Sprintf("百度原画(无限)请求失败: errno=%d", e.errno)
}

func isBaiduDlnaErrno(err error, errno int64) bool {
	var apiErr *baiduDlnaAPIError
	return errors.As(err, &apiErr) && apiErr.errno == errno
}

// decodeBaiduSekey 把 BDCLND/randsk 的 Cookie 编码值还原为 share/list
// 参数值。必须使用 PathUnescape 而不是 QueryUnescape：部分 sekey 本身包含
// “+”，QueryUnescape 会错误地把它变成空格。后续 url.Values.Encode 会负责
// 对参数做且只做一次 URL 编码。
func decodeBaiduSekey(raw string) (string, error) {
	if raw == "" || !strings.Contains(raw, "%") {
		return raw, nil
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return "", fmt.Errorf("百度分享 sekey 解码失败: %w", err)
	}
	return decoded, nil
}

func baiduSha1(input string) string {
	sum := sha1.Sum([]byte(input))
	return hex.EncodeToString(sum[:])
}

func baiduDlnaSaltB() string {
	return baiduDlnaDevUID + baiduDlnaVersion + baiduDlnaSaltBSuffix
}

// 签名: sha1( sha1(BDUSS) + uid + saltA + time + saltB )。
func baiduDlnaRand(bduss, uid, timestamp string) string {
	return baiduSha1(baiduSha1(bduss) + uid + baiduDlnaSaltA + timestamp + baiduDlnaSaltB())
}

func baiduIsHTTP(value string) bool {
	return strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://")
}

func baiduURLKeyHit(key string) bool {
	switch strings.ToLower(key) {
	case "dlink", "downloadurl", "download_url", "url":
		return true
	}
	return false
}

// baiduPickHTTPURL 深度遍历响应体找 dlink/downloadurl/download_url/url 字段中的
// http 直链；严格模式只认这几个字段，宽松模式退化为任意 http 字符串。map 键排序
// 遍历以保证结果确定(避免 Go map 随机序导致多直链时选取不稳定)。
func baiduPickHTTPURL(value any) string {
	if u := baiduWalkHTTPURL(value, true); u != "" {
		return u
	}
	return baiduWalkHTTPURL(value, false)
}

func baiduWalkHTTPURL(node any, strict bool) string {
	switch typed := node.(type) {
	case []any:
		for _, item := range typed {
			if u := baiduWalkHTTPURL(item, strict); u != "" {
				return u
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if str, ok := typed[key].(string); ok {
				if (!strict || baiduURLKeyHit(key)) && baiduIsHTTP(str) {
					return str
				}
			}
		}
		for _, key := range keys {
			if u := baiduWalkHTTPURL(typed[key], strict); u != "" {
				return u
			}
		}
	case string:
		if !strict && baiduIsHTTP(typed) {
			return typed
		}
	}
	return ""
}

func baiduErrno(data any) int64 {
	root, ok := data.(map[string]any)
	if !ok {
		return 0
	}
	for _, key := range []string{"errno", "error_code"} {
		switch value := root[key].(type) {
		case float64:
			return int64(value)
		case json.Number:
			n, _ := value.Int64()
			return n
		case string:
			n, _ := strconv.ParseInt(value, 10, 64)
			return n
		}
	}
	return 0
}

// fetchBaiduAccountUID 从 mbd.baidu.com/userx/v1/info/get 解析当前登录账号的 uid
// (签名要用)。注意 uid 与网盘 share_uk 不同，签名必须用账号 uid。
func fetchBaiduAccountUID(ctx context.Context, client baiduHTTPDoer, accountCookie string) (string, error) {
	const infoURL = "https://mbd.baidu.com/userx/v1/info/get?appname=baiduboxapp&fields=%5B%22uid%22%5D&client&clientfrom&lang=zh-cn&tpl&ttt"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, infoURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", baiduWebUA)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Cookie", accountCookie)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	match := baiduUIDPattern.FindSubmatch(body)
	if len(match) < 2 {
		return "", errors.New("百度原画(无限)未能解析账号 UID")
	}
	return string(match[1]), nil
}

// requestBaiduDlnaDlink 用 dlna 签名请求 share/list 拿到分享文件 dlink。
func requestBaiduDlnaDlink(ctx context.Context, client baiduHTTPDoer, params baiduUnlimitedParams, timestamp, rand string) (string, error) {
	decodedSekey, err := decodeBaiduSekey(params.sekey)
	if err != nil {
		return "", err
	}

	query := url.Values{}
	query.Set("shareid", params.shareID)
	query.Set("uk", params.shareUK)
	query.Set("fid", params.fileID)
	if decodedSekey != "" {
		query.Set("sekey", decodedSekey)
	}
	query.Set("origin", "dlna")
	query.Set("devuid", baiduDlnaDevUID)
	query.Set("clienttype", "1")
	query.Set("channel", baiduDlnaChannel)
	query.Set("version", baiduDlnaVersion)
	query.Set("time", timestamp)
	query.Set("rand", rand)

	reqURL := "https://pan.baidu.com/share/list?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", baiduDlnaUA)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	requestCookie := params.cookie
	if params.sekey != "" {
		// share/list 不仅校验账号 BDUSS，也会使用 BDCLND 确认当前分享的
		// 密码验证上下文。缺少它时，部分分享即使 fid 正确也返回 errno=-9。
		requestCookie = cookie.SetStr(requestCookie, "BDCLND", params.sekey)
	}
	req.Header.Set("Cookie", requestCookie)

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("百度原画(无限)请求失败: HTTP %d", resp.StatusCode)
	}
	var data any
	if err := json.Unmarshal(body, &data); err != nil {
		return "", fmt.Errorf("百度原画(无限)响应解析失败: %w", err)
	}
	if errno := baiduErrno(data); errno != 0 {
		return "", &baiduDlnaAPIError{errno: errno}
	}
	dlink := baiduPickHTTPURL(data)
	if dlink == "" {
		return "", errors.New("百度原画(无限)未返回直链")
	}
	return dlink, nil
}

// followBaiduDlnaRedirect 跟随一层 302，把 dlink 换成 d.pcs.baidu.com CDN 上的
// 最终直链；拿不到 Location 时返回空串，由调用方用原始 dlink 兜底。
func followBaiduDlnaRedirect(ctx context.Context, client baiduHTTPDoer, rawURL string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", baiduDlnaUA)
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	return resp.Header.Get("Location")
}

// resolveBaiduUnlimitedLink 执行完整的百度原画(无限)免转存解析，返回可直接播放的
// model.Link(仅带 dlna UA，免 Cookie、Range 支持)。任一环节失败即返回 error，
// 由驱动层回退到 saveFile 转存-删除方案。
func resolveBaiduUnlimitedLink(ctx context.Context, client baiduHTTPDoer, params baiduUnlimitedParams) (*model.Link, error) {
	if err := baiduPlaybackParamsError(params.shareID, params.shareUK, params.fileID); err != nil {
		return nil, err
	}
	bduss := cookie.GetStr(params.cookie, "BDUSS")
	if bduss == "" {
		return nil, errors.New("百度账号 Cookie 缺少 BDUSS")
	}
	uid, err := fetchBaiduAccountUID(ctx, client, params.cookie)
	if err != nil {
		return nil, err
	}

	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	rand := baiduDlnaRand(bduss, uid, timestamp)

	dlink, err := requestBaiduDlnaDlink(ctx, client, params, timestamp, rand)
	if err != nil {
		return nil, err
	}

	// 302 拿不到就用原始 dlink 兜底(它自身也可播，只是每次会先经过一层跳转)。
	finalURL := followBaiduDlnaRedirect(ctx, client, dlink)
	if finalURL == "" {
		finalURL = dlink
	}

	expiration := baiduDlnaLinkTTL
	return &model.Link{
		URL: finalURL,
		Header: http.Header{
			"User-Agent": []string{baiduDlnaUA},
		},
		Expiration: &expiration,
	}, nil
}
