package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"gorm.io/gorm"
)

// CreateCenterModelChannel creates a dedicated internal DeepSeek channel. The
// credential is accepted only by the restricted service endpoint; it must not
// be passed through an MCP tool argument or returned in a readback.
func CreateCenterModelChannel(modelID, upstreamModelID, name, credential, requestID, payloadHash string, check func(uint64) bool) (int, bool, error) {
	var channelID int
	replayed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		var operation CenterOperation
		err := tx.Where("request_id = ?", requestID).Take(&operation).Error
		if err == nil {
			if operation.OrganizationID != "model-ops" || operation.Kind != "channel-create" || operation.Actor != modelID || operation.PayloadHash != payloadHash {
				return ErrCenterOperationConflict
			}
			var existing Channel
			if err := tx.Where("name = ? AND models = ? AND "+commonGroupCol+" = ?", name, modelID, centerInternalModelGroup).Take(&existing).Error; err != nil {
				return err
			}
			channelID = existing.Id
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
		var channels []Channel
		if err := tx.Select("id", "models").Find(&channels).Error; err != nil {
			return err
		}
		for _, existing := range channels {
			for _, candidate := range existing.GetModels() {
				if candidate == modelID {
					return ErrCenterModelChannelInvalid
				}
			}
		}
		mapping, err := common.Marshal(map[string]string{modelID: upstreamModelID})
		if err != nil {
			return err
		}
		mappingString := string(mapping)
		channel := Channel{Name: name, Type: constant.ChannelTypeDeepSeek, Key: credential,
			Status: common.ChannelStatusEnabled, Models: modelID, Group: centerInternalModelGroup,
			ModelMapping: &mappingString, CreatedTime: time.Now().Unix()}
		if err := tx.Create(&channel).Error; err != nil {
			return err
		}
		channelID = channel.Id
		if err := tx.Create(&Ability{Group: centerInternalModelGroup, Model: modelID,
			ChannelId: channelID, Enabled: true}).Error; err != nil {
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
		return tx.Create(&CenterOperation{RequestID: requestID, OrganizationID: "model-ops", Kind: "channel-create",
			PayloadHash: payloadHash, Actor: modelID, CreatedAt: time.Now().UTC()}).Error
	})
	if err != nil || replayed {
		return channelID, replayed, err
	}
	InitChannelCache()
	RefreshPricing()
	return channelID, false, nil
}

func CenterChannelCreatePayloadHash(modelID, upstreamModelID, name, credential string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s", modelID, upstreamModelID, name, strings.TrimSpace(credential))))
	return hex.EncodeToString(digest[:])
}
