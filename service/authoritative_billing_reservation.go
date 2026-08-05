package service

import (
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"gorm.io/gorm"
)

var errAuthoritativeBillingQuotaInsufficient = errors.New("authoritative billing quota is insufficient")

// reserveAuthoritativeBillingQuota is enabled with the SD2 submission ledger
// so legacy requests cannot race a T1 reservation using a stale Redis balance.
// Funding and token quota are changed in one database transaction; caches are
// refreshed only after commit and are never the authorization boundary.
func reserveAuthoritativeBillingQuota(relayInfo *relaycommon.RelayInfo, funding FundingSource, quota int) (int, error) {
	if relayInfo == nil || funding == nil || quota <= 0 || quota > common.MaxQuota {
		return 0, errors.New("invalid authoritative billing reservation")
	}
	if common.BatchUpdateEnabled {
		return 0, ErrTaskSubmissionBatchQuotaUnsafe
	}

	var subscriptionResult *model.SubscriptionPreConsumeResult
	subscriptionExtra := false
	tokenConsumed := 0
	err := model.DB.Transaction(func(tx *gorm.DB) error {
		switch typed := funding.(type) {
		case *WalletFunding:
			if typed.userId != relayInfo.UserId {
				return errors.New("wallet funding owner does not match relay user")
			}
			result := tx.Model(&model.User{}).
				Where("id = ? AND status = ? AND quota >= ?", typed.userId, common.UserStatusEnabled, quota).
				Update("quota", gorm.Expr("quota - ?", quota))
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return errAuthoritativeBillingQuotaInsufficient
			}
		case *SubscriptionFunding:
			if typed.userId != relayInfo.UserId {
				return errors.New("subscription funding owner does not match relay user")
			}
			if typed.subscriptionId > 0 {
				var subscription model.UserSubscription
				if err := submissionLockForUpdate(tx).
					Select("id").
					Where("id = ? AND user_id = ?", typed.subscriptionId, typed.userId).
					First(&subscription).Error; err != nil {
					if errors.Is(err, gorm.ErrRecordNotFound) {
						return errAuthoritativeBillingQuotaInsufficient
					}
					return err
				}
				if err := model.PostConsumeUserSubscriptionDeltaTx(tx, typed.subscriptionId, int64(quota)); err != nil {
					return err
				}
				subscriptionExtra = true
			} else {
				if typed.amount != int64(quota) {
					return errors.New("subscription funding amount does not match token reservation")
				}
				var err error
				subscriptionResult, err = model.PreConsumeUserSubscriptionTx(
					tx, typed.requestId, typed.userId, typed.modelName, 0, typed.amount,
				)
				if err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unsupported authoritative funding source %T", funding)
		}

		if subscriptionResult != nil && subscriptionResult.Replayed {
			return nil
		}
		if relayInfo.IsPlayground {
			return nil
		}

		// TokenUnlimited comes from request middleware and may have been read
		// from a stale cache. Re-read and lock the token in the same transaction
		// as the funding reservation so a cached unlimited flag can never bypass
		// a finite-token reservation (or vice versa).
		var token model.Token
		if err := submissionLockForUpdate(tx).
			Where("id = ? AND user_id = ?", relayInfo.TokenId, relayInfo.UserId).
			First(&token).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errAuthoritativeBillingQuotaInsufficient
			}
			return err
		}
		if token.Status != common.TokenStatusEnabled {
			return errAuthoritativeBillingQuotaInsufficient
		}
		if token.UnlimitedQuota {
			return nil
		}
		if token.RemainQuota < quota || token.UsedQuota < 0 || token.UsedQuota > common.MaxQuota-quota {
			return errAuthoritativeBillingQuotaInsufficient
		}
		remainBefore := token.RemainQuota
		usedBefore := token.UsedQuota
		result := tx.Model(&model.Token{}).
			Where("id = ? AND user_id = ? AND status = ? AND unlimited_quota = ? AND remain_quota = ? AND used_quota = ?",
				token.Id, relayInfo.UserId, common.TokenStatusEnabled, false, remainBefore, usedBefore).
			Updates(map[string]any{
				"remain_quota":  remainBefore - quota,
				"used_quota":    usedBefore + quota,
				"accessed_time": common.GetTimestamp(),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errAuthoritativeBillingQuotaInsufficient
		}
		tokenConsumed = quota
		return nil
	})
	if err != nil {
		return 0, err
	}

	switch typed := funding.(type) {
	case *WalletFunding:
		typed.consumed += quota
		if common.RedisEnabled {
			_, _ = model.GetUserQuota(typed.userId, true)
		}
	case *SubscriptionFunding:
		if subscriptionResult != nil {
			typed.subscriptionId = subscriptionResult.UserSubscriptionId
			typed.preConsumed = subscriptionResult.PreConsumed
			typed.AmountTotal = subscriptionResult.AmountTotal
			typed.AmountUsedAfter = subscriptionResult.AmountUsedAfter
			if planInfo, lookupErr := model.GetSubscriptionPlanInfoByUserSubscriptionId(subscriptionResult.UserSubscriptionId); lookupErr == nil && planInfo != nil {
				typed.PlanId = planInfo.PlanId
				typed.PlanTitle = planInfo.PlanTitle
			}
		} else if subscriptionExtra {
			typed.AmountUsedAfter += int64(quota)
		}
	}
	if tokenConsumed > 0 && common.RedisEnabled {
		_, _ = model.GetTokenByKey(relayInfo.TokenKey, true)
	}
	return tokenConsumed, nil
}
