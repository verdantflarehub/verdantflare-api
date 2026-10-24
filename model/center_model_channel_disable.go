package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

func CenterChannelDisablePayloadHash(modelID string, channelID int) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", modelID, channelID)))
	return hex.EncodeToString(digest[:])
}

func DisableCenterModelChannel(modelID string, channelID int, requestID, payloadHash string, check func(uint64) bool) (bool, error) {
	replayed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		var operation CenterOperation
		err := tx.Where("request_id = ?", requestID).Take(&operation).Error
		if err == nil {
			if operation.OrganizationID != "model-ops" || operation.Kind != "channel-disable" || operation.Actor != modelID || operation.PayloadHash != payloadHash {
				return ErrCenterOperationConflict
			}
			replayed = true
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var revision CenterModelRevision
		err = lockForUpdate(tx).Where("model_id = ?", modelID).Take(&revision).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			revision = CenterModelRevision{ModelID: modelID}
			if err = tx.Create(&revision).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if !check(revision.Sequence) {
			return ErrCenterModelRevision
		}
		var channel Channel
		if err := lockForUpdate(tx).Where("id = ?", channelID).Take(&channel).Error; err != nil {
			return err
		}
		// Never disable a shared source channel or another model's route.
		if channel.Models != modelID || channel.Group != centerInternalModelGroup || channel.Status != common.ChannelStatusEnabled {
			return ErrCenterModelChannelInvalid
		}
		if err := tx.Model(&Channel{}).Where("id = ?", channelID).
			Update("status", common.ChannelStatusManuallyDisabled).Error; err != nil {
			return err
		}
		if err := tx.Model(&Ability{}).Where("channel_id = ? AND model = ?", channelID, modelID).
			Update("enabled", false).Error; err != nil {
			return err
		}
		changed := tx.Model(&CenterModelRevision{}).Where("model_id = ? AND sequence = ?", modelID, revision.Sequence).
			Update("sequence", revision.Sequence+1)
		if changed.Error != nil {
			return changed.Error
		}
		if changed.RowsAffected != 1 {
			return ErrCenterModelRevision
		}
		return tx.Create(&CenterOperation{RequestID: requestID, OrganizationID: "model-ops", Kind: "channel-disable",
			PayloadHash: payloadHash, Actor: modelID, CreatedAt: time.Now().UTC()}).Error
	})
	if err != nil || replayed {
		return replayed, err
	}
	InitChannelCache()
	return false, nil
}
