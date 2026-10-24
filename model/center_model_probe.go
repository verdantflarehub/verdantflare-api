package model

import (
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

var ErrCenterModelProbeAlreadyStarted = errors.New("model probe request was already started")

// BeginCenterModelProbe creates a short-lived, non-public token that can spend
// at most the caller-supplied USD 0.01 equivalent. Neither its
// key nor its token ID is returned to Control or an MCP caller.
func BeginCenterModelProbe(organizationID, modelID, requestID, gatewayRevision string, quotaLimit int) (Token, error) {
	if quotaLimit <= 0 || len(gatewayRevision) != 64 {
		return Token{}, errors.New("invalid probe quota limit")
	}
	var token Token
	err := DB.Transaction(func(tx *gorm.DB) error {
		var operation CenterOperation
		err := tx.Where("request_id = ?", requestID).Take(&operation).Error
		if err == nil {
			return ErrCenterModelProbeAlreadyStarted
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var account CenterAccount
		if err := tx.Where("organization_id = ?", organizationID).Take(&account).Error; err != nil {
			return err
		}
		key, err := common.GenerateKey()
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		token = Token{UserId: account.UserID, Key: key, Status: common.TokenStatusEnabled,
			Name: "__vf_center_experience__model-probe", CreatedTime: now, AccessedTime: now,
			ExpiredTime: now + 300, RemainQuota: quotaLimit, UnlimitedQuota: false,
			ModelLimitsEnabled: true, ModelLimits: modelID, Group: centerInternalModelGroup}
		if err := tx.Create(&token).Error; err != nil {
			return err
		}
		return tx.Create(&CenterOperation{RequestID: requestID, OrganizationID: organizationID,
			Kind: "model-probe", PayloadHash: gatewayRevision, Actor: modelID,
			TokenID: token.Id, CreatedAt: time.Now().UTC()}).Error
	})
	return token, err
}

func EndCenterModelProbe(tokenID int) error {
	return DB.Model(&Token{}).Where("id = ?", tokenID).Update("status", common.TokenStatusDisabled).Error
}

func CenterModelProbeOperation(organizationID, requestID string) (CenterOperation, error) {
	var operation CenterOperation
	err := DB.Where("request_id = ? AND organization_id = ? AND kind = ?", requestID, organizationID, "model-probe").Take(&operation).Error
	return operation, err
}
