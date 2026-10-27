package baidu_share

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/go-resty/resty/v2"
)

func TestBaiduShareMetadataParsing(t *testing.T) {
	for _, tc := range []struct {
		name string
		page string
		id   string
		uk   string
	}{
		{"legacy JS strings", `shareid: "2907614368", share_uk: "123456789"`, "2907614368", "123456789"},
		{"JSON numbers", `{"shareid":2907614368,"share_uk":123456789}`, "2907614368", "123456789"},
		{"JSON strings", `{"shareid":"2907614368","share_uk":"123456789"}`, "2907614368", "123456789"},
		{"JS assignments", `shareid = 2907614368; share_uk = 123456789;`, "2907614368", "123456789"},
		{"field names only", `["share_uk","shareid"]`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet || r.URL.Path != "/s/1metadata" {
					t.Errorf("unexpected metadata request: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "text/html")
				_, _ = io.WriteString(w, "<script>"+tc.page+"</script>")
			}))
			defer server.Close()
			d := &BaiduShare2{
				Addition: Addition{Surl: "1metadata"},
				Token:    "existing-share-token",
				client:   resty.New().SetBaseURL(server.URL),
			}
			if err := d.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if d.ShareId != tc.id || d.ShareUk != tc.uk {
				t.Fatalf("metadata = (%q, %q), want (%q, %q)", d.ShareId, d.ShareUk, tc.id, tc.uk)
			}
			if calls != 1 || d.Token != "existing-share-token" {
				t.Fatalf("validation changed request count or token: calls=%d", calls)
			}
		})
	}
}

func TestBaiduShareListHarvestsMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		id     string
		uk     string
		body   string
		wantID string
		wantUK string
		failed bool
	}{
		{"both missing", "", "", `{"errno":0,"share_id":2907614368,"uk":123456789,"list":[]}`, "2907614368", "123456789", false},
		{"only ID missing", "", "123456789", `{"errno":0,"share_id":2907614368,"uk":123456789,"list":[]}`, "2907614368", "123456789", false},
		{"only UK missing", "2907614368", "", `{"errno":0,"share_id":"2907614368","uk":"123456789","list":[]}`, "2907614368", "123456789", false},
		{"existing metadata", "existing-id", "existing-uk", `{"errno":0,"share_id":2907614368,"uk":123456789,"list":[]}`, "existing-id", "existing-uk", false},
		{"no metadata", "", "", `{"errno":0,"list":[]}`, "", "", false},
		{"failed listing", "", "", `{"errno":105,"share_id":2907614368,"uk":123456789,"list":[]}`, "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet || r.URL.Path != "/share/list" {
					t.Errorf("unexpected listing request: %s %s", r.Method, r.URL.Path)
				}
				if r.URL.Query().Get("shorturl") != "metadata" {
					t.Errorf("listing requested a different share")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			d := &BaiduShare2{
				Addition: Addition{Surl: "1metadata"},
				ShareId:  tc.id,
				ShareUk:  tc.uk,
				Token:    "existing-share-token",
				client:   resty.New().SetBaseURL(server.URL),
			}
			_, err := d.List(context.Background(), &model.Object{Path: "/"}, model.ListArgs{})
			if (err != nil) != tc.failed {
				t.Fatalf("List error = %v, want failure=%v", err, tc.failed)
			}
			if d.ShareId != tc.wantID || d.ShareUk != tc.wantUK {
				t.Fatalf("metadata = (%q, %q), want (%q, %q)", d.ShareId, d.ShareUk, tc.wantID, tc.wantUK)
			}
			if calls != 1 || d.Token != "existing-share-token" {
				t.Fatalf("listing added validation/retry or changed token: calls=%d", calls)
			}
		})
	}
}
