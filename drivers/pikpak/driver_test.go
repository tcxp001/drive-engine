package pikpak

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/go-resty/resty/v2"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestRestoreShareFileUsesTraceMappingAtAccountRoot(t *testing.T) {
	originalClient := base.RestyClient
	t.Cleanup(func() {
		base.RestyClient = originalClient
	})

	var requestBody map[string]interface{}
	base.RestyClient = resty.New().SetTransport(roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		if request.Method != http.MethodPost {
			t.Fatalf("request method = %q", request.Method)
		}
		if request.URL.Path != "/drive/v1/share/restore" {
			t.Fatalf("request path = %q", request.URL.Path)
		}
		if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": {"application/json"},
			},
			Body: io.NopCloser(strings.NewReader(
				`{"params":{"trace_file_ids":"{\"source-file\":\"saved-file\"}"}}`,
			)),
			Request: request,
		}, nil
	}))

	account := &PikPak{
		Common: &Common{
			UserAgent: "account-agent",
			DeviceID:  "account-device",
		},
	}
	fileID, err := account.RestoreShareFile(
		context.Background(),
		"share-id",
		"pass-code-token",
		"source-file",
		"video.mkv",
		1024,
	)
	if err != nil {
		t.Fatalf("RestoreShareFile() error = %v", err)
	}
	if fileID != "saved-file" {
		t.Fatalf("restored file id = %q", fileID)
	}
	if requestBody["parent_id"] != "" {
		t.Fatalf("parent_id = %#v", requestBody["parent_id"])
	}
	if requestBody["specify_parent_id"] != true {
		t.Fatalf("specify_parent_id = %#v", requestBody["specify_parent_id"])
	}
	if requestBody["share_id"] != "share-id" {
		t.Fatalf("share_id = %#v", requestBody["share_id"])
	}
	if requestBody["pass_code_token"] != "pass-code-token" {
		t.Fatalf("pass_code_token = %#v", requestBody["pass_code_token"])
	}
	fileIDs, ok := requestBody["file_ids"].([]interface{})
	if !ok || len(fileIDs) != 1 || fileIDs[0] != "source-file" {
		t.Fatalf("file_ids = %#v", requestBody["file_ids"])
	}
}
