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

// StageCenterPublicRoute clones a dedicated internal route into a disabled
// default-group route. The clone remains uncallable until activation.
func StageCenterPublicRoute(modelID string, sourceID int, catalogToken, requestID, payloadHash string, check func(uint64) bool) (int, bool, error) {
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
		var existing []Channel
		if err := tx.Select("id", "models", commonGroupCol).Find(&existing).Error; err != nil {
			return err
		}
		for _, channel := range existing {
			for _, candidate := range channel.GetModels() {
				if candidate == modelID {
					for _, group := range channel.GetGroups() {
						if group == "default" {
							return ErrCenterModelChannelInvalid
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
	if common.RedisEnabled {
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
