package baidu_share

import (
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	log "github.com/sirupsen/logrus"
)

type Addition struct {
	driver.RootPath
	Surl string `json:"surl"`
	Pwd  string `json:"pwd"`
	// AccountStorageID pins all resolution attempts for an MV playback mount
	// to the account selected by the MV planner.
	AccountStorageID uint `json:"account_storage_id"`
	// UnlimitedOnly is set by Media Vault playback mounts.  In this mode a
	// failed DLNA/unlimited resolution must not silently consume account
	// storage through the traditional save-and-delete fallback.
	UnlimitedOnly bool `json:"unlimited_only" default:"false"`
}

var config = driver.Config{
	Name:        "BaiduShare2",
	LocalSort:   true,
	NoUpload:    true,
	DefaultRoot: "/",
	Alert:       "",
}

const baseId = 20000

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &BaiduShare2{}
	})
	op.RegisterValidateFunc(func() error {
		return validateBaiduShares()
	})
}

func validateBaiduShares() error {
	storages := op.GetStorages("BaiduShare2")
	log.Infof("validate %v Baidu shares", len(storages))
	for _, storage := range storages {
		driver := storage.(*BaiduShare2)
		if driver.ID < baseId {
			continue
		}
		err := driver.Validate()
		if err != nil {
			log.Warnf("[%v] 百度分享错误: %v", driver.ID, err)
			driver.GetStorage().SetStatus(err.Error())
			op.MustSaveDriverStorage(driver)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil
}
