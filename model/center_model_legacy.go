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

// Legacy reconstruction is deliberately limited to the two existing
// DeepSeek IDs. The supplier credential never crosses the model-ops API.
func centerLegacyModel(modelID string) bool {
	return modelID == "deepseek-flash" || modelID == "deepseek-v4-pro"
}

func CenterLegacyPreparePayloadHash(modelID string, legacyID int) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", modelID, legacyID)))
	return hex.EncodeToString(digest[:])
}

func CenterLegacyRecoveryPayloadHash(modelID string, legacyID, sourceID, publicID int, action string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d\x00%d\x00%s", modelID, legacyID, sourceID, publicID, action)))
	return hex.EncodeToString(digest[:])
}

func centerLegacyChannel(tx *gorm.DB, modelID string, legacyID int) (Channel, string, error) {
	if !centerLegacyModel(modelID) || legacyID <= 0 {
		return Channel{}, "", ErrCenterModelChannelInvalid
	}
	var legacy Channel
	if err := lockForUpdate(tx).Where("id = ?", legacyID).Take(&legacy).Error; err != nil {
		return Channel{}, "", err
	}
	if legacy.Status != common.ChannelStatusEnabled || legacy.Type != constant.ChannelTypeDeepSeek ||
		len(legacy.GetGroups()) != 1 || legacy.GetGroups()[0] != "default" || strings.TrimSpace(legacy.Key) == "" {
		return Channel{}, "", ErrCenterModelChannelInvalid
	}
	models := legacy.GetModels()
	if len(models) < 1 || len(models) > 2 {
		return Channel{}, "", ErrCenterModelChannelInvalid
	}
	found := false
	for _, candidate := range models {
		if !centerLegacyModel(candidate) {
			return Channel{}, "", ErrCenterModelChannelInvalid
		}
		if candidate == modelID {
			found = true
		}
	}
	if !found || (len(models) == 2 && models[0] == models[1]) {
		return Channel{}, "", ErrCenterModelChannelInvalid
	}
	mapping := map[string]string{}
	if raw := legacy.GetModelMapping(); raw != "" {
		if err := common.Unmarshal([]byte(raw), &mapping); err != nil || mapping == nil {
			return Channel{}, "", ErrCenterModelChannelInvalid
		}
	}
	upstream := mapping[modelID]
	if upstream == "" {
		upstream = modelID
	}
	var ability Ability
	if err := tx.Where("channel_id = ? AND model = ? AND "+commonGroupCol+" = ? AND enabled = ?", legacyID, modelID, "default", true).
		Take(&ability).Error; err != nil {
		return Channel{}, "", ErrCenterModelChannelInvalid
	}
	return legacy, upstream, nil
}

// PrepareCenterLegacyModelChannel clones one model from the old shared route
// into a dedicated internal route while the original remains callable.
func PrepareCenterLegacyModelChannel(modelID string, legacyID int, requestID, payloadHash string, check func(uint64) bool) (int, bool, error) {
	internalID, replayed := 0, false
	err := DB.Transaction(func(tx *gorm.DB) error {
		var operation CenterOperation
		err := tx.Where("request_id = ?", requestID).Take(&operation).Error
		if err == nil {
			if operation.OrganizationID != "model-ops" || operation.Kind != "legacy-prepare" || operation.Actor != modelID || operation.PayloadHash != payloadHash {
				return ErrCenterOperationConflict
			}
			internalID, replayed = operation.TokenID, true
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
		legacy, upstream, err := centerLegacyChannel(tx, modelID, legacyID)
		if err != nil {
			return err
		}
		var channels []Channel
		if err := tx.Select("id", "models").Find(&channels).Error; err != nil {
			return err
		}
		for _, channel := range channels {
			if channel.Id == legacyID {
				continue
			}
			for _, candidate := range channel.GetModels() {
				if candidate == modelID {
					return ErrCenterModelChannelInvalid
				}
			}
		}
		mapping, err := common.Marshal(map[string]string{modelID: upstream})
		if err != nil {
			return err
		}
		clone := legacy
		clone.Id = 0
		clone.Name = legacy.Name + "-" + modelID + "-internal"
		clone.Group = centerInternalModelGroup
		clone.Models = modelID
		mapped := string(mapping)
		clone.ModelMapping = &mapped
		clone.UsedQuota = 0
		clone.CreatedTime = time.Now().Unix()
		if err := tx.Create(&clone).Error; err != nil {
			return err
		}
		internalID = clone.Id
		if err := tx.Create(&Ability{Group: centerInternalModelGroup, Model: modelID, ChannelId: internalID,
			Enabled: true, Priority: legacy.Priority, Weight: uint(legacy.GetWeight()), Tag: legacy.Tag}).Error; err != nil {
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
		return tx.Create(&CenterOperation{RequestID: requestID, OrganizationID: "model-ops", Kind: "legacy-prepare",
			PayloadHash: payloadHash, Actor: modelID, TokenID: internalID, CreatedAt: time.Now().UTC()}).Error
	})
	if err != nil || replayed {
		return internalID, replayed, err
	}
	InitChannelCache()
	return internalID, false, nil
}

// RecoverCenterLegacyPublicRoute removes only a route created for this model.
// Before activation it aborts the disabled route; after activation it restores
// the old shared route atomically and removes the new route. No history, token,
// supplier credential or model metadata is deleted.
func RecoverCenterLegacyPublicRoute(modelID string, legacyID, sourceID, publicID int, rollback bool,
	requestID, payloadHash string, check func(uint64) bool) (bool, error) {
	if !centerLegacyModel(modelID) || legacyID <= 0 || sourceID <= 0 || publicID <= 0 ||
		legacyID == sourceID || legacyID == publicID || sourceID == publicID {
		return false, ErrCenterModelChannelInvalid
	}
	kind := "legacy-abort"
	if rollback {
		kind = "legacy-rollback"
	}
	replayed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		var operation CenterOperation
		err := tx.Where("request_id = ?", requestID).Take(&operation).Error
		if err == nil {
			if operation.OrganizationID != "model-ops" || operation.Kind != kind || operation.Actor != modelID || operation.PayloadHash != payloadHash {
				return ErrCenterOperationConflict
			}
			replayed = true
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var revision CenterModelRevision
		if err := lockForUpdate(tx).Where("model_id = ?", modelID).Take(&revision).Error; err != nil {
			return err
		}
		if !check(revision.Sequence) {
			return ErrCenterModelRevision
		}
		var legacy, source, public Channel
		for _, item := range []struct {
			id      int
			channel *Channel
		}{{legacyID, &legacy}, {sourceID, &source}, {publicID, &public}} {
			if err := lockForUpdate(tx).Where("id = ?", item.id).Take(item.channel).Error; err != nil {
				return err
			}
		}
		if source.Models != modelID || source.Group != centerInternalModelGroup || source.Status != common.ChannelStatusEnabled ||
			public.Models != modelID || public.Group != "default" || source.Type != constant.ChannelTypeDeepSeek ||
			public.Type != source.Type || legacy.Type != source.Type || source.Key == "" ||
			public.Key != source.Key || legacy.Key != source.Key || public.GetModelMapping() != source.GetModelMapping() ||
			len(legacy.GetGroups()) != 1 || legacy.GetGroups()[0] != "default" {
			return ErrCenterModelChannelInvalid
		}
		var oldAbility, publicAbility Ability
		if err := lockForUpdate(tx).Where("channel_id = ? AND model = ? AND "+commonGroupCol+" = ?",
			legacyID, modelID, "default").Take(&oldAbility).Error; err != nil {
			return ErrCenterModelChannelInvalid
		}
		if err := lockForUpdate(tx).Where("channel_id = ? AND model = ? AND "+commonGroupCol+" = ?",
			publicID, modelID, "default").Take(&publicAbility).Error; err != nil {
			return ErrCenterModelChannelInvalid
		}
		if rollback {
			if public.Status != common.ChannelStatusEnabled || !publicAbility.Enabled || oldAbility.Enabled ||
				(legacy.Status != common.ChannelStatusEnabled && legacy.Status != common.ChannelStatusManuallyDisabled) {
				return ErrCenterModelChannelInvalid
			}
			for _, candidate := range legacy.GetModels() {
				if candidate == modelID || !centerLegacyModel(candidate) {
					return ErrCenterModelChannelInvalid
				}
			}
			mapping := map[string]string{}
			if raw := legacy.GetModelMapping(); raw != "" {
				if err := common.Unmarshal([]byte(raw), &mapping); err != nil || mapping == nil {
					return ErrCenterModelChannelInvalid
				}
			}
			sourceMapping := map[string]string{}
			if err := common.Unmarshal([]byte(source.GetModelMapping()), &sourceMapping); err != nil || sourceMapping[modelID] == "" {
				return ErrCenterModelChannelInvalid
			}
			mapping[modelID] = sourceMapping[modelID]
			mapped, err := common.Marshal(mapping)
			if err != nil {
				return err
			}
			models := append(legacy.GetModels(), modelID)
			if err := tx.Model(&Channel{}).Where("id = ?", legacyID).Updates(map[string]any{
				"models": strings.Join(models, ","), "model_mapping": string(mapped), "status": common.ChannelStatusEnabled,
			}).Error; err != nil {
				return err
			}
			if err := tx.Model(&Ability{}).Where("channel_id = ? AND model = ? AND "+commonGroupCol+" = ?", legacyID, modelID, "default").
				Update("enabled", true).Error; err != nil {
				return err
			}
		} else {
			if public.Status != common.ChannelStatusManuallyDisabled || publicAbility.Enabled || !oldAbility.Enabled ||
				legacy.Status != common.ChannelStatusEnabled {
				return ErrCenterModelChannelInvalid
			}
			found := false
			for _, candidate := range legacy.GetModels() {
				if candidate == modelID {
					found = true
				}
			}
			if !found {
				return ErrCenterModelChannelInvalid
			}
		}
		if err := tx.Where("channel_id = ? AND model = ? AND "+commonGroupCol+" = ?", publicID, modelID, "default").
			Delete(&Ability{}).Error; err != nil {
			return err
		}
		if err := tx.Where("id = ?", publicID).Delete(&Channel{}).Error; err != nil {
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
		return tx.Create(&CenterOperation{RequestID: requestID, OrganizationID: "model-ops", Kind: kind,
			PayloadHash: payloadHash, Actor: modelID, TokenID: publicID, CreatedAt: time.Now().UTC()}).Error
	})
	if err != nil || replayed {
		return replayed, err
	}
	InitChannelCache()
	return false, nil
}
