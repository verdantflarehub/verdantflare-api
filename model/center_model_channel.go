package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

var ErrCenterModelChannelInvalid = errors.New("source channel cannot safely serve this model")

const centerInternalModelGroup = "center-internal"

// AttachCenterModelAlias maps a new model ID to an already configured upstream
// on one existing channel. It does not accept a credential, change the source
// model, or expose the alias publicly. New provider channels use a separate
// credential-upload flow and are deliberately not supported here.
func AttachCenterModelAlias(modelID, sourceModelID string, channelID int, requestID, payloadHash string, check func(uint64) bool) (bool, error) {
	replayed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		var operation CenterOperation
		err := tx.Where("request_id = ?", requestID).Take(&operation).Error
		if err == nil {
			if operation.OrganizationID != "model-ops" || operation.Kind != "alias" || operation.PayloadHash != payloadHash || operation.Actor != modelID {
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
		var source Channel
		if err := lockForUpdate(tx).Where("id = ?", channelID).Take(&source).Error; err != nil {
			return err
		}
		if strings.TrimSpace(source.Key) == "" || source.Status != common.ChannelStatusEnabled {
			return ErrCenterModelChannelInvalid
		}
		groups := source.GetGroups()
		// Adding a model to a shared default-group channel would make an
		// unpublished test alias callable by ordinary customers who know its ID.
		if len(groups) != 1 || groups[0] != centerInternalModelGroup {
			return ErrCenterModelChannelInvalid
		}
		found := false
		for _, candidate := range source.GetModels() {
			if candidate == sourceModelID {
				found = true
			}
		}
		if !found {
			return ErrCenterModelChannelInvalid
		}
		var channels []Channel
		if err := tx.Select("id", "models").Find(&channels).Error; err != nil {
			return err
		}
		for _, channel := range channels {
			for _, candidate := range channel.GetModels() {
				if candidate == modelID {
					return ErrCenterModelChannelInvalid
				}
			}
		}
		var metadata Model
		if err := tx.Where("model_name = ?", modelID).Take(&metadata).Error; err == nil {
			return ErrCenterModelChannelInvalid
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		mapping := map[string]string{}
		if raw := source.GetModelMapping(); raw != "" {
			if err := common.Unmarshal([]byte(raw), &mapping); err != nil {
				return ErrCenterModelChannelInvalid
			}
		}
		if mapping == nil {
			return ErrCenterModelChannelInvalid
		}
		upstreamModel := sourceModelID
		if configured := mapping[sourceModelID]; configured != "" {
			upstreamModel = configured
		}
		mapping[modelID] = upstreamModel
		mappingJSON, err := common.Marshal(mapping)
		if err != nil {
			return err
		}
		models := strings.Trim(source.Models, ",") + "," + modelID
		if err := tx.Model(&Channel{}).Where("id = ?", channelID).Updates(map[string]any{
			"models": models, "model_mapping": string(mappingJSON),
		}).Error; err != nil {
			return err
		}
		for _, group := range groups {
			ability := Ability{Group: group, Model: modelID, ChannelId: channelID, Enabled: true,
				Priority: source.Priority, Weight: uint(source.GetWeight()), Tag: source.Tag}
			if err := tx.Create(&ability).Error; err != nil {
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
		return tx.Create(&CenterOperation{RequestID: requestID, OrganizationID: "model-ops", Kind: "alias",
			PayloadHash: payloadHash, Actor: modelID, CreatedAt: time.Now().UTC()}).Error
	})
	if err != nil || replayed {
		return replayed, err
	}
	InitChannelCache()
	RefreshPricing()
	return false, nil
}

func CenterAliasPayloadHash(modelID, sourceModelID string, channelID int) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", modelID, sourceModelID, channelID)))
	return hex.EncodeToString(digest[:])
}
