package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"gorm.io/gorm"
)

// CenterInternalGroupSnapshot returns the actual persisted group-ratio revision.
// This narrow first-stage operation may only create center-internal at ratio 1;
// changing existing groups needs a separate impact review.
func CenterInternalGroupSnapshot() (string, bool, error) {
	var option Option
	err := DB.Where("key = ?", "GroupRatio").Take(&option).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, err
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		option.Value = ratio_setting.GroupRatio2JSONString()
	}
	var groups map[string]float64
	if err := common.Unmarshal([]byte(option.Value), &groups); err != nil {
		return "", false, err
	}
	digest := sha256.Sum256([]byte(option.Value))
	_, exists := groups[centerInternalModelGroup]
	return hex.EncodeToString(digest[:]), exists, nil
}

func CreateCenterInternalGroup(expectedRevision, requestID string) (bool, error) {
	replayed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		var operation CenterOperation
		err := tx.Where("request_id = ?", requestID).Take(&operation).Error
		if err == nil {
			if operation.OrganizationID != "model-ops" || operation.Kind != "group-create" || operation.Actor != centerInternalModelGroup {
				return ErrCenterOperationConflict
			}
			replayed = true
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var option Option
		err = lockForUpdate(tx).Where("key = ?", "GroupRatio").Take(&option).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			option = Option{Key: "GroupRatio", Value: ratio_setting.GroupRatio2JSONString()}
			if err := tx.Create(&option).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		digest := sha256.Sum256([]byte(option.Value))
		if hex.EncodeToString(digest[:]) != expectedRevision {
			return ErrCenterModelRevision
		}
		var groups map[string]float64
		if err := common.Unmarshal([]byte(option.Value), &groups); err != nil {
			return err
		}
		if groups == nil {
			return ErrCenterModelChannelInvalid
		}
		if _, exists := groups[centerInternalModelGroup]; exists {
			return ErrCenterModelChannelInvalid
		}
		groups[centerInternalModelGroup] = 1
		encoded, err := common.Marshal(groups)
		if err != nil {
			return err
		}
		if err := tx.Model(&Option{}).Where("key = ?", "GroupRatio").Update("value", string(encoded)).Error; err != nil {
			return err
		}
		return tx.Create(&CenterOperation{RequestID: requestID, OrganizationID: "model-ops",
			Kind: "group-create", Actor: centerInternalModelGroup, CreatedAt: time.Now().UTC()}).Error
	})
	if err != nil || replayed {
		return replayed, err
	}
	loadOptionsFromDatabase()
	RefreshPricing()
	return false, nil
}
