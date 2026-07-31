package _115

import (
	"errors"
	"fmt"
	"net/url"
)

const apiVideoPreview = "https://v.anxia.com/webapi/files/video"

type VideoPreviewInfo struct {
	State          bool              `json:"state"`
	Error          string            `json:"error"`
	Message        string            `json:"message"`
	VideoURL       string            `json:"video_url"`
	DefinitionList map[string]string `json:"definition_list"`
}

func (d *Pan115) GetVideoPreviewInfo(pickCode string) (*VideoPreviewInfo, error) {
	if pickCode == "" {
		return nil, errors.New("empty 115 pickcode")
	}
	result := VideoPreviewInfo{}
	resp, err := d.client.NewRequest().
		SetQueryParams(map[string]string{
			"pickcode": pickCode,
			"share_id": "0",
			"local":    "1",
		}).
		SetResult(&result).
		Get(apiVideoPreview)
	if err != nil {
		return nil, err
	}
	if resp.IsError() {
		return nil, fmt.Errorf("115 video preview returned HTTP %d", resp.StatusCode())
	}
	if _, err := url.ParseRequestURI(result.VideoURL); err != nil {
		return nil, errors.New("115 video preview URL is unavailable")
	}
	if result.DefinitionList == nil {
		result.DefinitionList = map[string]string{}
	}
	return &result, nil
}
