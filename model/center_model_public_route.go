package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"gorm.io/gorm"
)

func CenterPublicRoutePayloadHash(modelID string, sourceID, publicID int) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d", modelID, sourceID, publicID)))
	return hex.EncodeToString(digest[:])
}

func CenterPublicRouteStagePayloadHash(modelID string, sourceID int, catalogToken string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s", modelID, sourceID, catalogToken)))
	return hex.EncodeToString(digest[:])
}

func CenterLegacyPublicRouteStagePayloadHash(modelID string, sourceID, legacyID int, catalogToken string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d\x00%s", modelID, sourceID, legacyID, catalogToken)))
	return hex.EncodeToString(digest[:])
}

func CenterLegacyPublicRoutePayloadHash(modelID string, sourceID, publicID, legacyID int) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d\x00%d", modelID, sourceID, publicID, legacyID)))
	return hex.EncodeToString(digest[:])
}

// StageCenterPublicRoute clones a dedicated internal route into a disabled
// default-group route. The clone remains uncallable until activation.
func StageCenterPublicRoute(modelID string, sourceID int, catalogToken, requestID, payloadHash string, check func(uint64) bool) (int, bool, error) {
	return stageCenterPublicRoute(modelID, sourceID, 0, catalogToken, requestID, payloadHash, check)
}

func StageCenterLegacyPublicRoute(modelID string, sourceID, legacyID int, catalogToken, requestID, payloadHash string, check func(uint64) bool) (int, bool, error) {
	return stageCenterPublicRoute(modelID, sourceID, legacyID, catalogToken, requestID, payloadHash, check)
}

func stageCenterPublicRoute(modelID string, sourceID, legacyID int, catalogToken, requestID, payloadHash string, check func(uint64) bool) (int, bool, error) {
	publicID := 0
	replayed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		var operation CenterOperation
		err := tx.Where("request_id = ?", requestID).Take(&operation).Error
		if err == nil {
			if operation.OrganizationID != "model-ops" || operation.Kind != "public-stage" || operation.Actor != modelID || operation.PayloadHash != payloadHash {
				return ErrCenterOperationConflict
			}
			publicID, replayed = operation.TokenID, true
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
		var catalog Token
		if err := lockForUpdate(tx).Where(commonKeyCol+" = ?", strings.TrimPrefix(catalogToken, "sk-")).Take(&catalog).Error; err != nil {
			return ErrCenterModelChannelInvalid
		}
		if catalog.Name != "control-catalog-read" || catalog.Status != common.TokenStatusEnabled ||
			catalog.Group != "default" || !catalog.ModelLimitsEnabled || catalog.UnlimitedQuota ||
			(catalog.ExpiredTime != -1 && catalog.ExpiredTime <= time.Now().Unix()) {
			return ErrCenterModelChannelInvalid
		}
		var source Channel
		if err := lockForUpdate(tx).Where("id = ?", sourceID).Take(&source).Error; err != nil {
			return err
		}
		if source.Models != modelID || source.Group != centerInternalModelGroup || source.Status != common.ChannelStatusEnabled ||
			source.Type != constant.ChannelTypeDeepSeek || strings.TrimSpace(source.Key) == "" {
			return ErrCenterModelChannelInvalid
		}
		if legacyID > 0 {
			legacy, upstream, err := centerLegacyChannel(tx, modelID, legacyID)
			if err != nil || source.Key != legacy.Key {
				return ErrCenterModelChannelInvalid
			}
			mapping := map[string]string{}
			if err := common.Unmarshal([]byte(source.GetModelMapping()), &mapping); err != nil || mapping[modelID] != upstream || len(mapping) != 1 {
				return ErrCenterModelChannelInvalid
			}
		}
		var existing []Channel
		if err := tx.Select("id", "models", commonGroupCol).Find(&existing).Error; err != nil {
			return err
		}
		for _, channel := range existing {
			for _, candidate := range channel.GetModels() {
				if candidate == modelID {
					for _, group := range channel.GetGroups() {
						if group == "default" {
							if legacyID == 0 || channel.Id != legacyID {
								return ErrCenterModelChannelInvalid
							}
						}
					}
				}
			}
		}
		clone := source
		clone.Id = 0
		clone.Name = source.Name + "-public"
		clone.Group = "default"
		clone.Status = common.ChannelStatusManuallyDisabled
		clone.CreatedTime = time.Now().Unix()
		clone.UsedQuota = 0
		if err := tx.Create(&clone).Error; err != nil {
			return err
		}
		publicID = clone.Id
		if err := tx.Create(&Ability{Group: "default", Model: modelID, ChannelId: publicID,
			Enabled: false, Priority: source.Priority, Weight: uint(source.GetWeight())}).Error; err != nil {
			return err
		}
		modelSet := make(map[string]bool)
		for _, allowed := range strings.Split(catalog.ModelLimits, ",") {
			if allowed = strings.TrimSpace(allowed); allowed != "" {
				modelSet[allowed] = true
			}
		}
		if legacyID > 0 {
			// Existing public models must already be visible through the catalog
			// token; migration must not widen its scope as a side effect.
			if !modelSet[modelID] {
				return ErrCenterModelChannelInvalid
			}
		} else {
			modelSet[modelID] = true
			allowedModels := make([]string, 0, len(modelSet))
			for allowed := range modelSet {
				allowedModels = append(allowedModels, allowed)
			}
			sort.Strings(allowedModels)
			if err := tx.Model(&Token{}).Where("id = ?", catalog.Id).
				Update("model_limits", strings.Join(allowedModels, ",")).Error; err != nil {
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
		return tx.Create(&CenterOperation{RequestID: requestID, OrganizationID: "model-ops", Kind: "public-stage",
			PayloadHash: payloadHash, Actor: modelID, TokenID: publicID, CreatedAt: time.Now().UTC()}).Error
	})
	if err != nil {
		return publicID, replayed, err
	}
	if common.RedisEnabled && legacyID == 0 {
		if err := cacheDeleteToken(strings.TrimPrefix(catalogToken, "sk-")); err != nil {
			return publicID, replayed, err
		}
	}
	if replayed {
		return publicID, true, nil
	}
	InitChannelCache()
	return publicID, false, nil
}

func ActivateCenterPublicRoute(modelID string, sourceID, publicID int, requestID, payloadHash string, check func(uint64) bool) (bool, error) {
	return activateCenterPublicRoute(modelID, sourceID, publicID, 0, requestID, payloadHash, check)
}

func ActivateCenterLegacyPublicRoute(modelID string, sourceID, publicID, legacyID int, requestID, payloadHash string, check func(uint64) bool) (bool, error) {
	return activateCenterPublicRoute(modelID, sourceID, publicID, legacyID, requestID, payloadHash, check)
}

func activateCenterPublicRoute(modelID string, sourceID, publicID, legacyID int, requestID, payloadHash string, check func(uint64) bool) (bool, error) {
	replayed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		var operation CenterOperation
		err := tx.Where("request_id = ?", requestID).Take(&operation).Error
		if err == nil {
			if operation.OrganizationID != "model-ops" || operation.Kind != "public-activate" || operation.Actor != modelID || operation.PayloadHash != payloadHash {
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
		var source, public Channel
		if err := lockForUpdate(tx).Where("id = ?", sourceID).Take(&source).Error; err != nil {
			return err
		}
		if err := lockForUpdate(tx).Where("id = ?", publicID).Take(&public).Error; err != nil {
			return err
		}
		if source.Models != modelID || source.Group != centerInternalModelGroup || source.Status != common.ChannelStatusEnabled ||
			public.Models != modelID || public.Group != "default" || public.Status != common.ChannelStatusManuallyDisabled ||
			source.Key != public.Key || source.GetModelMapping() != public.GetModelMapping() || source.Type != public.Type {
			return ErrCenterModelChannelInvalid
		}
		if legacyID > 0 {
			legacy, upstream, err := centerLegacyChannel(tx, modelID, legacyID)
			if err != nil || legacy.Key != source.Key {
				return ErrCenterModelChannelInvalid
			}
			oldMapping := map[string]string{}
			if raw := legacy.GetModelMapping(); raw != "" {
				if err := common.Unmarshal([]byte(raw), &oldMapping); err != nil || oldMapping == nil {
					return ErrCenterModelChannelInvalid
				}
			}
			newMapping := map[string]string{}
			if err := common.Unmarshal([]byte(source.GetModelMapping()), &newMapping); err != nil || newMapping[modelID] != upstream {
				return ErrCenterModelChannelInvalid
			}
			delete(oldMapping, modelID)
			mapped, err := common.Marshal(oldMapping)
			if err != nil {
				return err
			}
			remaining := make([]string, 0, len(legacy.GetModels())-1)
			for _, candidate := range legacy.GetModels() {
				if candidate != modelID {
					remaining = append(remaining, candidate)
				}
			}
			if len(remaining) > 1 {
				return ErrCenterModelChannelInvalid
			}
			updates := map[string]any{"models": strings.Join(remaining, ","), "model_mapping": string(mapped)}
			if len(remaining) == 0 {
				updates["status"] = common.ChannelStatusManuallyDisabled
			}
			if err := tx.Model(&Channel{}).Where("id = ?", legacyID).Updates(updates).Error; err != nil {
				return err
			}
			oldAbility := tx.Model(&Ability{}).Where("channel_id = ? AND model = ? AND "+commonGroupCol+" = ? AND enabled = ?",
				legacyID, modelID, "default", true).Update("enabled", false)
			if oldAbility.Error != nil {
				return oldAbility.Error
			}
			if oldAbility.RowsAffected != 1 {
				return ErrCenterModelChannelInvalid
			}
		}
		if err := tx.Model(&Channel{}).Where("id = ?", publicID).Update("status", common.ChannelStatusEnabled).Error; err != nil {
			return err
		}
		ability := tx.Model(&Ability{}).Where("channel_id = ? AND model = ? AND "+commonGroupCol+" = ?", publicID, modelID, "default").Update("enabled", true)
		if ability.Error != nil {
			return ability.Error
		}
		if ability.RowsAffected != 1 {
			return ErrCenterModelChannelInvalid
		}
		changed := tx.Model(&CenterModelRevision{}).Where("model_id = ? AND sequence = ?", modelID, revision.Sequence).
			Update("sequence", revision.Sequence+1)
		if changed.Error != nil {
			return changed.Error
		}
		if changed.RowsAffected != 1 {
			return ErrCenterModelRevision
		}
		return tx.Create(&CenterOperation{RequestID: requestID, OrganizationID: "model-ops", Kind: "public-activate",
			PayloadHash: payloadHash, Actor: modelID, TokenID: publicID, CreatedAt: time.Now().UTC()}).Error
	})
	if err != nil || replayed {
		return replayed, err
	}
	InitChannelCache()
	return false, nil
}
