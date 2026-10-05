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
	"gorm.io/gorm/clause"
)

// CenterAccount maps a Center organization to one non-login gateway user.
type CenterAccount struct {
	OrganizationID string `gorm:"primaryKey;size:80"`
	UserID         int    `gorm:"uniqueIndex;not null"`
	CreatedAt      time.Time
}

// CenterOperation makes a credit grant or token creation retry-safe.
type CenterOperation struct {
	RequestID      string `gorm:"primaryKey;size:80"`
	OrganizationID string `gorm:"index;size:80;not null"`
	Kind           string `gorm:"size:24;not null"`
	PayloadHash    string `gorm:"size:64;not null"`
	Actor          string `gorm:"size:160"`
	AmountCents    int
	TokenID        int
	CreatedAt      time.Time
}

var ErrCenterOperationConflict = errors.New("center operation request ID conflicts with an existing operation")
var ErrCenterQuotaOverflow = errors.New("center organization quota would exceed the supported maximum")
var ErrCenterAccountDisabled = errors.New("center organization gateway account is disabled")

func GetCenterAccount(organizationID string) (CenterAccount, error) {
	var account CenterAccount
	err := DB.Where("organization_id = ?", organizationID).Take(&account).Error
	return account, err
}

func GetCenterUser(organizationID string) (User, error) {
	account, err := GetCenterAccount(organizationID)
	if err != nil {
		return User{}, err
	}
	var user User
	err = DB.First(&user, account.UserID).Error
	return user, err
}

// GetCenterExperienceCharge returns the settled consume-log quota for one
// gateway request, scoped to the Center organization's own gateway user.
func GetCenterExperienceCharge(organizationID, requestID string) (int, error) {
	user, err := GetCenterUser(organizationID)
	if err != nil {
		return 0, err
	}
	var log Log
	if err := LOG_DB.Select("quota").Where("user_id = ? AND request_id = ? AND type = ?", user.Id, requestID, LogTypeConsume).Take(&log).Error; err != nil {
		return 0, err
	}
	return log.Quota, nil
}

func ensureCenterAccount(tx *gorm.DB, organizationID string) (CenterAccount, error) {
	var account CenterAccount
	err := tx.Where("organization_id = ?", organizationID).Take(&account).Error
	if err == nil {
		return account, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return CenterAccount{}, err
	}
	digest := sha256.Sum256([]byte(organizationID))
	password, err := common.GenerateKey()
	if err != nil {
		return CenterAccount{}, err
	}
	hashedPassword, err := common.Password2Hash(password)
	if err != nil {
		return CenterAccount{}, err
	}
	user := User{
		Username: "vf" + hex.EncodeToString(digest[:8]), Password: hashedPassword,
		DisplayName: "Center org", Role: common.RoleCommonUser,
		Status: common.UserStatusEnabled, Group: "default", Quota: 0,
		AffCode: "c" + hex.EncodeToString(digest[:8]),
		Remark:  "Center-managed; interactive login disabled",
	}
	if err := tx.Create(&user).Error; err != nil {
		return CenterAccount{}, err
	}
	account = CenterAccount{OrganizationID: organizationID, UserID: user.Id}
	return account, tx.Create(&account).Error
}

func centerOperation(tx *gorm.DB, requestID, organizationID, kind, payloadHash string) (CenterOperation, bool, error) {
	var operation CenterOperation
	err := tx.Where("request_id = ?", requestID).Take(&operation).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return CenterOperation{}, false, nil
	}
	if err != nil {
		return CenterOperation{}, false, err
	}
	if operation.OrganizationID != organizationID || operation.Kind != kind || operation.PayloadHash != payloadHash {
		return CenterOperation{}, false, ErrCenterOperationConflict
	}
	return operation, true, nil
}

func GrantCenterCredit(organizationID, requestID, actor string, amountCents int) (User, error) {
	if amountCents <= 0 || amountCents > 100000 {
		return User{}, fmt.Errorf("amountCents must be between 1 and 100000")
	}
	// One cent is 1/100 USD. Reject a changed gateway quota scale instead of
	// silently crediting an incorrect monetary amount.
	if common.QuotaPerUnit != 500000 {
		return User{}, fmt.Errorf("unsupported gateway quota scale")
	}
	units := amountCents * 5000
	payload := sha256.Sum256([]byte(fmt.Sprintf("credit:%s:%d:%s", organizationID, amountCents, actor)))
	hash := hex.EncodeToString(payload[:])
	var userID int
	err := DB.Transaction(func(tx *gorm.DB) error {
		_, found, err := centerOperation(tx, requestID, organizationID, "credit", hash)
		if err != nil || found {
			return err
		}
		account, err := ensureCenterAccount(tx, organizationID)
		if err != nil {
			return err
		}
		userID = account.UserID
		var user User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, userID).Error; err != nil {
			return err
		}
		if user.Status != common.UserStatusEnabled {
			return ErrCenterAccountDisabled
		}
		if user.Quota > common.MaxQuota-units {
			return ErrCenterQuotaOverflow
		}
		if err := tx.Model(&User{}).Where("id = ?", userID).Update("quota", gorm.Expr("quota + ?", units)).Error; err != nil {
			return err
		}
		return tx.Create(&CenterOperation{RequestID: requestID, OrganizationID: organizationID, Kind: "credit", PayloadHash: hash, Actor: actor, AmountCents: amountCents}).Error
	})
	if err != nil {
		// A concurrent retry may have committed the same request after this
		// transaction first checked for it. Re-read the durable operation.
		if _, found, lookupErr := centerOperation(DB, requestID, organizationID, "credit", hash); lookupErr != nil {
			return User{}, lookupErr
		} else if found {
			return GetCenterUser(organizationID)
		}
		return User{}, err
	}
	if userID != 0 {
		_ = InvalidateUserCache(userID)
	}
	return GetCenterUser(organizationID)
}

func CreateCenterToken(organizationID, requestID, name string, models []string, expiresInDays int) (Token, error) {
	modelsText := strings.Join(models, ",")
	payload := sha256.Sum256([]byte(fmt.Sprintf("key:%s:%s:%s:%d", organizationID, name, modelsText, expiresInDays)))
	hash := hex.EncodeToString(payload[:])
	var token Token
	err := DB.Transaction(func(tx *gorm.DB) error {
		operation, found, err := centerOperation(tx, requestID, organizationID, "key", hash)
		if err != nil {
			return err
		}
		if found {
			return tx.First(&token, operation.TokenID).Error
		}
		account, err := ensureCenterAccount(tx, organizationID)
		if err != nil {
			return err
		}
		key, err := common.GenerateKey()
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		expiresAt := int64(-1)
		if expiresInDays > 0 {
			expiresAt = now + int64(expiresInDays)*86400
		}
		token = Token{UserId: account.UserID, Key: key, Status: common.TokenStatusEnabled,
			Name: name, CreatedTime: now, AccessedTime: now, ExpiredTime: expiresAt,
			UnlimitedQuota: true, ModelLimitsEnabled: true, ModelLimits: modelsText, Group: "default"}
		if err := tx.Create(&token).Error; err != nil {
			return err
		}
		return tx.Create(&CenterOperation{RequestID: requestID, OrganizationID: organizationID, Kind: "key", PayloadHash: hash, TokenID: token.Id}).Error
	})
	if err != nil {
		if operation, found, lookupErr := centerOperation(DB, requestID, organizationID, "key", hash); lookupErr != nil {
			return Token{}, lookupErr
		} else if found {
			if loadErr := DB.First(&token, operation.TokenID).Error; loadErr != nil {
				return Token{}, loadErr
			}
			return token, nil
		}
	}
	return token, err
}

func ListCenterTokens(organizationID string) ([]Token, error) {
	account, err := GetCenterAccount(organizationID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return []Token{}, nil
	}
	if err != nil {
		return nil, err
	}
	var tokens []Token
	err = DB.Where("user_id = ?", account.UserID).Order("id desc").Find(&tokens).Error
	return tokens, err
}

func GetCenterToken(organizationID string, tokenID int) (Token, error) {
	account, err := GetCenterAccount(organizationID)
	if err != nil {
		return Token{}, err
	}
	var token Token
	err = DB.Where("id = ? AND user_id = ?", tokenID, account.UserID).Take(&token).Error
	return token, err
}

func SetCenterAccountEnabled(organizationID string, enabled bool) error {
	status := common.UserStatusDisabled
	if enabled {
		status = common.UserStatusEnabled
	}
	var userID int
	err := DB.Transaction(func(tx *gorm.DB) error {
		account, err := ensureCenterAccount(tx, organizationID)
		if err != nil {
			return err
		}
		userID = account.UserID
		return tx.Model(&User{}).Where("id = ?", userID).Update("status", status).Error
	})
	if err != nil {
		return err
	}
	return InvalidateUserCache(userID)
}
