package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"gorm.io/gorm"
)

func CenterCredentialRotatePayloadHash(modelID string, channelID int, credential string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s", modelID, channelID, credential)))
	return hex.EncodeToString(digest[:])
}

func RotateCenterModelCredential(modelID string, channelID int, credential, requestID, payloadHash string, check func(uint64) bool) (bool, error) {
	replayed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		var operation CenterOperation
		err := tx.Where("request_id = ?", requestID).Take(&operation).Error
		if err == nil {
			if operation.OrganizationID != "model-ops" || operation.Kind != "credential-rotate" || operation.Actor != modelID || operation.PayloadHash != payloadHash {
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
		if channel.Models != modelID || channel.Group != centerInternalModelGroup || channel.Type != constant.ChannelTypeDeepSeek {
			return ErrCenterModelChannelInvalid
		}
		if err := tx.Model(&Channel{}).Where("id = ?", channelID).Update("key", credential).Error; err != nil {
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
		return tx.Create(&CenterOperation{RequestID: requestID, OrganizationID: "model-ops", Kind: "credential-rotate",
			PayloadHash: payloadHash, Actor: modelID, CreatedAt: time.Now().UTC()}).Error
	})
	if err != nil || replayed {
		return replayed, err
	}
	InitChannelCache()
	return false, nil
}
