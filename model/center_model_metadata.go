package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

func CenterMetadataPayloadHash(modelID, description, endpoints string, status int) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d", modelID, description, endpoints, status)))
	return hex.EncodeToString(digest[:])
}

// ConfigureCenterModelMetadata modifies only the named model's metadata. It
// leaves vendor ownership, icon, tags and unrelated model rows untouched.
func ConfigureCenterModelMetadata(modelID, description, endpoints string, status int, requestID, payloadHash string, check func(uint64) bool) (bool, error) {
	replayed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		var operation CenterOperation
		err := tx.Where("request_id = ?", requestID).Take(&operation).Error
		if err == nil {
			if operation.OrganizationID != "model-ops" || operation.Kind != "metadata" || operation.Actor != modelID || operation.PayloadHash != payloadHash {
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
		var metadata Model
		err = lockForUpdate(tx).Where("model_name = ?", modelID).Take(&metadata).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			metadata = Model{ModelName: modelID, Description: description, Endpoints: endpoints,
				Status: status, SyncOfficial: 0, CreatedTime: time.Now().Unix(), UpdatedTime: time.Now().Unix()}
			if err := tx.Create(&metadata).Error; err != nil {
				return err
			}
			// GORM defaults normalize zero to one; enforce the requested draft state.
			if err := tx.Model(&Model{}).Where("id = ?", metadata.Id).Updates(map[string]any{"status": status, "sync_official": 0}).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			if err := tx.Model(&Model{}).Where("id = ?", metadata.Id).Updates(map[string]any{
				"description": description, "endpoints": endpoints, "status": status, "updated_time": time.Now().Unix(),
			}).Error; err != nil {
				return err
			}
		}
		changed := tx.Model(&CenterModelRevision{}).Where("model_id = ? AND sequence = ?", modelID, revision.Sequence).
			Update("sequence", revision.Sequence+1)
		if changed.Error != nil {
			return changed.Error
		}
		if changed.RowsAffected != 1 {
			return ErrCenterModelRevision
		}
		return tx.Create(&CenterOperation{RequestID: requestID, OrganizationID: "model-ops", Kind: "metadata",
			PayloadHash: payloadHash, Actor: modelID, CreatedAt: time.Now().UTC()}).Error
	})
	return replayed, err
}
