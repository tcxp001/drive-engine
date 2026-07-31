package _123Share

import (
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	log "github.com/sirupsen/logrus"
)

type Addition struct {
	ShareKey string `json:"share_id" required:"true"`
	SharePwd string `json:"share_pwd"`
	driver.RootID
	// AccountStorageID is injected by Media Vault for ordinary Cookie-backed
	// playback. Unlimited-only mounts leave it zero and remain anonymous.
	AccountStorageID uint `json:"account_storage_id"`
	//OrderBy        string `json:"order_by" type:"select" options:"file_name,size,update_at" default:"file_name"`
	//OrderDirection string `json:"order_direction" type:"select" options:"asc,desc" default:"asc"`
	// AccessToken is retained only for backward-compatible mount decoding.
	// Media Vault playback never consumes it: unlimited-only is anonymous,
	// while traditional playback requires a normal 123Pan cookie account.
	AccessToken string `json:"accesstoken" type:"text"`
	// UnlimitedOnly is set on Media Vault playback mounts. In this mode the
	// share uses only anonymous /share/get and /share/download/info requests
	// and must never fall back to a normal 123Pan or 123 Open account.
	UnlimitedOnly bool `json:"unlimited_only" default:"false"`
}

var config = driver.Config{
	Name:        "123PanShare",
	LocalSort:   true,
	NoUpload:    true,
	DefaultRoot: "0",
	PreferProxy: true,
}

const baseId = 20000

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &Pan123Share{}
	})
	op.RegisterValidateFunc(func() error {
		return validate123Shares()
	})
}

func validate123Shares() error {
	storages := op.GetStorages("123PanShare")
	log.Infof("validate %v 123 shares", len(storages))
	for _, storage := range storages {
		driver := storage.(*Pan123Share)
		if driver.ID < baseId {
			continue
		}
		err := driver.Validate()
		if err != nil {
			log.Warnf("[%v] 123分享错误: %v", driver.ID, err)
			driver.GetStorage().SetStatus(err.Error())
			op.MustSaveDriverStorage(driver)
		}
		time.Sleep(800 * time.Millisecond)
	}
	return nil
}
