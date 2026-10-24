package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"gorm.io/gorm"
)

func CenterChannelUpdatePayloadHash(modelID string, channelID int, name, upstream string, weight uint, priority int64) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%d\x00%d", modelID, channelID, name, upstream, weight, priority)))
	return hex.EncodeToString(digest[:])
}

// UpdateCenterModelChannel edits only a dedicated, unpublished internal route.
// A staged or active default route requires a separate migration workflow.
func UpdateCenterModelChannel(modelID string, channelID int, name, upstream, requestID, payloadHash string, weight uint, priority int64, check func(uint64) bool) (bool, error) {
	replayed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		var operation CenterOperation
		err := tx.Where("request_id = ?", requestID).Take(&operation).Error
		if err == nil {
			if operation.OrganizationID != "model-ops" || operation.Kind != "channel-update" || operation.Actor != modelID || operation.PayloadHash != payloadHash {
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
		if channel.Models != modelID || channel.Group != centerInternalModelGroup || channel.Type != constant.ChannelTypeDeepSeek || channel.Status != common.ChannelStatusEnabled || channel.Key == "" {
			return ErrCenterModelChannelInvalid
		}
		var channels []Channel
		if err := tx.Select("id", "models", commonGroupCol).Find(&channels).Error; err != nil {
			return err
		}
		for _, candidate := range channels {
			if candidate.Id == channelID {
				continue
			}
			for _, candidateModel := range candidate.GetModels() {
				if candidateModel == modelID {
					return ErrCenterModelChannelInvalid
				}
			}
		}
		mapping, err := common.Marshal(map[string]string{modelID: upstream})
		if err != nil {
			return err
		}
		mappingString := string(mapping)
		if err := tx.Model(&Channel{}).Where("id = ?", channelID).Updates(map[string]any{
			"name": name, "model_mapping": mappingString, "weight": weight, "priority": priority,
		}).Error; err != nil {
			return err
		}
		ability := tx.Model(&Ability{}).Where("channel_id = ? AND model = ? AND "+commonGroupCol+" = ?", channelID, modelID, centerInternalModelGroup).
			Updates(map[string]any{"weight": weight, "priority": priority})
		if ability.Error != nil {
			return ability.Error
		}
		if ability.RowsAffected != 1 {
			return ErrCenterModelChannelInvalid
		}
		changed := tx.Model(&CenterModelRevision{}).Where("model_id = ? AND sequence = ?", modelID, revision.Sequence).Update("sequence", revision.Sequence+1)
		if changed.Error != nil {
			return changed.Error
		}
		if changed.RowsAffected != 1 {
			return ErrCenterModelRevision
		}
		return tx.Create(&CenterOperation{RequestID: requestID, OrganizationID: "model-ops", Kind: "channel-update",
			PayloadHash: payloadHash, Actor: modelID, TokenID: channelID, CreatedAt: time.Now().UTC()}).Error
	})
	if err != nil || replayed {
		return replayed, err
	}
	InitChannelCache()
	return false, nil
}
