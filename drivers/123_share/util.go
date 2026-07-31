package _123Share

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	_123 "github.com/OpenListTeam/OpenList/v4/drivers/123"
	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/go-resty/resty/v2"
	jsoniter "github.com/json-iterator/go"
	log "github.com/sirupsen/logrus"
)

const (
	Api          = "https://yun.123pan.com/api"
	AApi         = "https://yun.123pan.com/a/api"
	BApi         = "https://yun.123pan.com/b/api"
	MainApi      = BApi
	FileList     = MainApi + "/share/get"
	DownloadInfo = MainApi + "/share/download/info"
	//AuthKeySalt      = "8-8D$sL8gPjom7bk#cY"

	// Anonymous share endpoints are the complete 123 unlimited-playback
	// credential. They require neither a 123Pan cookie nor 123 Open OAuth.
	AnonOrigin       = "https://yun.123pan.com"
	AnonDownloadInfo = AnonOrigin + "/b/api/share/download/info"
	AnonUA           = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"
)

var idx = 0

var err123TrafficLimit = errors.New("123 分享流量包不足(5112)")

func signPath(path string, os string, version string) (k string, v string) {
	table := []byte{'a', 'd', 'e', 'f', 'g', 'h', 'l', 'm', 'y', 'i', 'j', 'n', 'o', 'p', 'k', 'q', 'r', 's', 't', 'u', 'b', 'c', 'v', 'w', 's', 'z'}
	random := fmt.Sprintf("%.f", math.Round(1e7*rand.Float64()))
	now := time.Now().In(time.FixedZone("CST", 8*3600))
	timestamp := fmt.Sprint(now.Unix())
	nowStr := []byte(now.Format("200601021504"))
	for i := 0; i < len(nowStr); i++ {
		nowStr[i] = table[nowStr[i]-48]
	}
	timeSign := fmt.Sprint(crc32.ChecksumIEEE(nowStr))
	data := strings.Join([]string{timestamp, random, path, os, version, timeSign}, "|")
	dataSign := fmt.Sprint(crc32.ChecksumIEEE([]byte(data)))
	return timeSign, strings.Join([]string{timestamp, random, dataSign}, "-")
}

func GetApi(rawUrl string) string {
	u, _ := url.Parse(rawUrl)
	query := u.Query()
	query.Add(signPath(u.Path, "web", "3"))
	u.RawQuery = query.Encode()
	return u.String()
}

func (d *Pan123Share) request(url string, method string, callback base.ReqCallback, resp interface{}) ([]byte, error) {
	if d.UnlimitedOnly {
		body, err := d.requestAnon(url, method, callback, resp)
		if err != nil {
			return nil, err
		}
		if err := check123AnonResponse(body); err != nil {
			return nil, err
		}
		return body, nil
	}

	storage := d.accountDriver(idx)
	if storage != nil {
		pan123, ok := storage.(*_123.Pan123)
		if !ok {
			return nil, errors.New("123云盘帐号类型无效")
		}
		return pan123.Request(url, method, callback, resp)
	}
	if d.ref != nil {
		return d.ref.Request(url, method, callback, resp)
	}
	return nil, errors.New("找不到123云盘帐号")
}

// requestAnon calls the public share API without Authorization or auth-key
// signing. It intentionally never consults the global 123Pan account registry.
func (d *Pan123Share) requestAnon(
	targetURL string,
	method string,
	callback base.ReqCallback,
	resp interface{},
) ([]byte, error) {
	req := base.RestyClient.R()
	req.SetHeaders(map[string]string{
		"user-agent":   AnonUA,
		"referer":      AnonOrigin + "/",
		"origin":       AnonOrigin,
		"accept":       "application/json,text/plain,*/*",
		"platform":     "android",
		"app-version":  "43",
		"content-type": "application/json;charset=UTF-8",
	})
	if callback != nil {
		callback(req)
	}
	if resp != nil {
		req.SetResult(resp)
	}
	res, err := req.Execute(method, targetURL)
	if err != nil {
		return nil, err
	}
	return res.Body(), nil
}

func check123AnonResponse(body []byte) error {
	code := utils.Json.Get(body, "code").ToInt()
	if code == 0 {
		return nil
	}
	message := strings.TrimSpace(jsoniter.Get(body, "message").ToString())
	if code == 5112 || strings.Contains(message, "流量包不足") || strings.Contains(message, "提取流量不足") {
		return err123TrafficLimit
	}
	if message == "" {
		message = fmt.Sprintf("123 anonymous API returned code %d", code)
	}
	return errors.New(message)
}

// unwrap123DownloadLink resolves the provider's short download entry without
// following redirects and preserves the entry origin as Referer.
func unwrap123DownloadLink(downloadURL string) (*model.Link, error) {
	if strings.TrimSpace(downloadURL) == "" {
		return nil, errors.New("empty 123 download url")
	}
	outer, err := url.Parse(downloadURL)
	if err != nil {
		return nil, err
	}
	entryURL := outer.String()
	if params := outer.Query().Get("params"); params != "" {
		decoded, err := base64.StdEncoding.DecodeString(params)
		if err != nil {
			return nil, err
		}
		inner, err := url.Parse(string(decoded))
		if err != nil {
			return nil, err
		}
		entryURL = inner.String()
	}

	log.Debug("123 download url: ", entryURL)
	res, err := base.NoRedirectClient.R().
		SetHeader("Referer", AnonOrigin+"/").
		Get(entryURL)
	if err != nil {
		return nil, err
	}
	expiration := 15 * time.Minute
	link := &model.Link{Expiration: &expiration, URL: entryURL}
	switch {
	case res.StatusCode() >= 300 && res.StatusCode() < 400:
		link.URL = res.Header().Get("location")
	case res.StatusCode() < 300:
		if redirectURL := utils.Json.Get(res.Body(), "data", "redirect_url").ToString(); redirectURL != "" {
			link.URL = redirectURL
		}
	default:
		return nil, fmt.Errorf("123 download entry returned status %d", res.StatusCode())
	}
	link.Header = http.Header{
		"Referer": []string{fmt.Sprintf("%s://%s/", outer.Scheme, outer.Host)},
	}
	return link, nil
}

// anonDownloadLink exchanges a public share object for an anonymous download
// entry. Any provider error is final; callers must not fall back to CK/Open.
func (d *Pan123Share) anonDownloadLink(file File, ip string) (*model.Link, error) {
	headers := map[string]string{}
	if !utils.IsLocalIPAddr(ip) {
		headers["X-Forwarded-For"] = ip
	}
	body := base.Json{
		"driveId":   "0",
		"shareKey":  d.ShareKey,
		"SharePwd":  d.SharePwd,
		"etag":      file.Etag,
		"fileId":    file.FileId,
		"s3keyFlag": file.S3KeyFlag,
		"FileName":  file.FileName,
		"size":      file.Size,
	}
	respBody, err := d.requestAnon(
		AnonDownloadInfo,
		http.MethodPost,
		func(req *resty.Request) {
			req.SetBody(body).SetHeaders(headers)
		},
		nil,
	)
	if err != nil {
		return nil, err
	}
	if err := check123AnonResponse(respBody); err != nil {
		return nil, err
	}
	return unwrap123DownloadLink(
		utils.Json.Get(respBody, "data", "DownloadURL").ToString(),
	)
}

func (d *Pan123Share) getFiles(ctx context.Context, parentId string) ([]File, error) {
	page := 1
	res := make([]File, 0)
	for {
		if err := d.APIRateLimit(ctx, FileList); err != nil {
			return nil, err
		}
		var resp Files
		query := map[string]string{
			"limit":          "100",
			"next":           "0",
			"orderBy":        "file_id",
			"orderDirection": "desc",
			"parentFileId":   parentId,
			"Page":           strconv.Itoa(page),
			"shareKey":       d.ShareKey,
			"SharePwd":       d.SharePwd,
		}
		_, err := d.request(FileList, http.MethodGet, func(req *resty.Request) {
			req.SetQueryParams(query)
		}, &resp)
		if err != nil {
			return nil, err
		}
		page++
		res = append(res, resp.Data.InfoList...)
		if len(resp.Data.InfoList) == 0 || resp.Data.Next == "-1" {
			break
		}
	}
	return res, nil
}

// do others that not defined in Driver interface

func (d *Pan123Share) Validate() error {
	query := map[string]string{
		"limit":          "1",
		"next":           "0",
		"orderBy":        "file_id",
		"orderDirection": "desc",
		"parentFileId":   "0",
		"Page":           "1",
		"shareKey":       d.ShareKey,
		"SharePwd":       d.SharePwd,
	}
	_, err := d.request(FileList, http.MethodGet, func(req *resty.Request) {
		req.SetQueryParams(query)
	}, nil)
	return err
}
