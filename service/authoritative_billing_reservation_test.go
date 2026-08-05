package service

import (
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func ensureAuthoritativeSubscriptionSchema(t *testing.T) {
	t.Helper()
	require.NoError(t, model.DB.Exec(`CREATE TABLE IF NOT EXISTS subscription_plans (
		id INTEGER PRIMARY KEY,
		title TEXT NOT NULL,
		quota_reset_period TEXT NOT NULL DEFAULT 'never',
		created_at INTEGER,
		updated_at INTEGER
	)`).Error)
	require.NoError(t, model.DB.Exec(`CREATE TABLE IF NOT EXISTS subscription_pre_consume_records (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		request_id TEXT NOT NULL UNIQUE,
		user_id INTEGER NOT NULL,
		user_subscription_id INTEGER NOT NULL,
		pre_consumed INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL,
		created_at INTEGER,
		updated_at INTEGER
	)`).Error)
	require.NoError(t, model.DB.Exec("DELETE FROM subscription_pre_consume_records").Error)
	require.NoError(t, model.DB.Exec("DELETE FROM subscription_plans").Error)
	t.Cleanup(func() {
		_ = model.DB.Exec("DELETE FROM subscription_pre_consume_records").Error
		_ = model.DB.Exec("DELETE FROM subscription_plans").Error
	})
}

func seedAuthoritativeSubscription(t *testing.T, subscriptionID, userID int, total, used int64) {
	t.Helper()
	planID := 9000 + subscriptionID
	require.NoError(t, model.DB.Exec(
		"INSERT INTO subscription_plans (id, title, quota_reset_period, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		planID, "authoritative-test", model.SubscriptionResetNever, time.Now().Unix(), time.Now().Unix(),
	).Error)
	require.NoError(t, model.DB.Create(&model.UserSubscription{
		Id: subscriptionID, UserId: userID, PlanId: planID,
		AmountTotal: total, AmountUsed: used, Status: "active",
		StartTime: time.Now().Add(-time.Hour).Unix(), EndTime: time.Now().Add(time.Hour).Unix(),
	}).Error)
}

func TestAuthoritativeBillingReservationRejectsStalePrecheckAfterT1(t *testing.T) {
	truncate(t)
	previousBatch := common.BatchUpdateEnabled
	common.BatchUpdateEnabled = false
	t.Cleanup(func() { common.BatchUpdateEnabled = previousBatch })

	const userID, tokenID, initialQuota = 871, 871, 1000
	const tokenKey = "sk-authoritative-race"
	seedUser(t, userID, initialQuota)
	seedToken(t, tokenID, userID, tokenKey, initialQuota)

	// This is the legacy request's precheck snapshot. T1 then consumes the
	// complete authoritative balance before that request resumes.
	require.Equal(t, initialQuota, getUserQuota(t, userID))
	require.Equal(t, initialQuota, getTokenRemainQuota(t, tokenID))
	_, created, err := PrepareTaskSubmission(taskSubmissionTestInput(
		userID, tokenID, "b627ec73-18f6-47f9-94d0-0686d82ea560", initialQuota,
	))
	require.NoError(t, err)
	require.True(t, created)

	_, err = reserveAuthoritativeBillingQuota(&relaycommon.RelayInfo{
		UserId: userID, TokenId: tokenID, TokenKey: tokenKey,
	}, &WalletFunding{userId: userID}, 1)
	require.True(t, errors.Is(err, errAuthoritativeBillingQuotaInsufficient))
	require.Zero(t, getUserQuota(t, userID))
	require.Zero(t, getTokenRemainQuota(t, tokenID))
	require.Equal(t, initialQuota, getTokenUsedQuota(t, tokenID))
}

func TestAuthoritativeBillingReservationRollsBackFundingWhenTokenIsInsufficient(t *testing.T) {
	truncate(t)
	previousBatch := common.BatchUpdateEnabled
	common.BatchUpdateEnabled = false
	t.Cleanup(func() { common.BatchUpdateEnabled = previousBatch })

	const userID, tokenID, initialQuota = 872, 872, 1000
	const tokenKey = "sk-authoritative-atomic"
	seedUser(t, userID, initialQuota)
	seedToken(t, tokenID, userID, tokenKey, 0)

	_, err := reserveAuthoritativeBillingQuota(&relaycommon.RelayInfo{
		UserId: userID, TokenId: tokenID, TokenKey: tokenKey,
	}, &WalletFunding{userId: userID}, 500)
	require.True(t, errors.Is(err, errAuthoritativeBillingQuotaInsufficient))
	require.Equal(t, initialQuota, getUserQuota(t, userID))
	require.Zero(t, getTokenRemainQuota(t, tokenID))
	require.Zero(t, getTokenUsedQuota(t, tokenID))
}

func TestAuthoritativeBillingReservationRejectsBatchAccounting(t *testing.T) {
	truncate(t)
	previousBatch := common.BatchUpdateEnabled
	common.BatchUpdateEnabled = true
	t.Cleanup(func() { common.BatchUpdateEnabled = previousBatch })

	const userID, tokenID = 882, 882
	const tokenKey = "sk-authoritative-batch-rejected"
	seedUser(t, userID, 1000)
	seedToken(t, tokenID, userID, tokenKey, 1000)
	_, err := reserveAuthoritativeBillingQuota(&relaycommon.RelayInfo{
		UserId: userID, TokenId: tokenID, TokenKey: tokenKey,
	}, &WalletFunding{userId: userID}, 200)
	require.ErrorIs(t, err, ErrTaskSubmissionBatchQuotaUnsafe)
	require.Equal(t, 1000, getUserQuota(t, userID))
	require.Equal(t, 1000, getTokenRemainQuota(t, tokenID))
	require.Zero(t, getTokenUsedQuota(t, tokenID))
}

func TestAuthoritativeBillingReservationSubscriptionReplayChargesTokenOnce(t *testing.T) {
	truncate(t)
	ensureAuthoritativeSubscriptionSchema(t)

	const userID, tokenID, subscriptionID, quota = 873, 873, 873, 400
	const tokenKey = "sk-authoritative-subscription-replay"
	const requestID = "authoritative-subscription-replay"
	seedUser(t, userID, 0)
	seedToken(t, tokenID, userID, tokenKey, 1000)
	seedAuthoritativeSubscription(t, subscriptionID, userID, 1000, 0)
	relayInfo := &relaycommon.RelayInfo{UserId: userID, TokenId: tokenID, TokenKey: tokenKey}

	firstFunding := &SubscriptionFunding{requestId: requestID, userId: userID, modelName: SD2OriginModel, amount: quota}
	consumed, err := reserveAuthoritativeBillingQuota(relayInfo, firstFunding, quota)
	require.NoError(t, err)
	require.Equal(t, quota, consumed)
	require.Equal(t, int64(quota), getSubscriptionUsed(t, subscriptionID))
	require.Equal(t, 600, getTokenRemainQuota(t, tokenID))
	require.Equal(t, quota, getTokenUsedQuota(t, tokenID))

	replayedFunding := &SubscriptionFunding{requestId: requestID, userId: userID, modelName: SD2OriginModel, amount: quota}
	consumed, err = reserveAuthoritativeBillingQuota(relayInfo, replayedFunding, quota)
	require.NoError(t, err)
	require.Zero(t, consumed)
	require.Equal(t, subscriptionID, replayedFunding.subscriptionId)
	require.Equal(t, int64(quota), replayedFunding.preConsumed)
	require.Equal(t, int64(quota), getSubscriptionUsed(t, subscriptionID))
	require.Equal(t, 600, getTokenRemainQuota(t, tokenID))
	require.Equal(t, quota, getTokenUsedQuota(t, tokenID))

	conflictingFunding := &SubscriptionFunding{requestId: requestID, userId: userID, modelName: SD2OriginModel, amount: quota + 1}
	_, err = reserveAuthoritativeBillingQuota(relayInfo, conflictingFunding, quota+1)
	require.ErrorIs(t, err, model.ErrSubscriptionPreConsumeConflict)
	require.Equal(t, int64(quota), getSubscriptionUsed(t, subscriptionID))
	require.Equal(t, 600, getTokenRemainQuota(t, tokenID))

	foreignFunding := &SubscriptionFunding{requestId: requestID, userId: userID + 1, modelName: SD2OriginModel, amount: quota}
	_, err = reserveAuthoritativeBillingQuota(&relaycommon.RelayInfo{UserId: userID + 1, TokenId: tokenID}, foreignFunding, quota)
	require.ErrorIs(t, err, model.ErrSubscriptionPreConsumeConflict)
	require.Equal(t, int64(quota), getSubscriptionUsed(t, subscriptionID))

	// The refund must use the existing transaction connection. The service test
	// database intentionally has MaxOpenConns=1, so a nested global transaction
	// would deadlock here instead of completing atomically.
	require.NoError(t, model.RefundSubscriptionPreConsume(requestID))
	require.NoError(t, model.RefundSubscriptionPreConsume(requestID))
	require.Zero(t, getSubscriptionUsed(t, subscriptionID))
	var record model.SubscriptionPreConsumeRecord
	require.NoError(t, model.DB.Where("request_id = ?", requestID).First(&record).Error)
	require.Equal(t, "refunded", record.Status)
}

func TestAuthoritativeBillingReservationUsesDatabaseUnlimitedFlag(t *testing.T) {
	t.Run("stale cached unlimited cannot bypass finite token", func(t *testing.T) {
		truncate(t)
		const userID, tokenID, quota = 874, 874, 200
		const tokenKey = "sk-authoritative-stale-unlimited"
		seedUser(t, userID, 1000)
		seedToken(t, tokenID, userID, tokenKey, 1000)

		consumed, err := reserveAuthoritativeBillingQuota(&relaycommon.RelayInfo{
			UserId: userID, TokenId: tokenID, TokenKey: tokenKey, TokenUnlimited: true,
		}, &WalletFunding{userId: userID}, quota)
		require.NoError(t, err)
		require.Equal(t, quota, consumed)
		require.Equal(t, 800, getUserQuota(t, userID))
		require.Equal(t, 800, getTokenRemainQuota(t, tokenID))
		require.Equal(t, quota, getTokenUsedQuota(t, tokenID))
	})

	t.Run("stale cached finite does not charge unlimited token", func(t *testing.T) {
		truncate(t)
		const userID, tokenID, quota = 875, 875, 200
		const tokenKey = "sk-authoritative-stale-finite"
		seedUser(t, userID, 1000)
		seedToken(t, tokenID, userID, tokenKey, 17)
		require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", tokenID).Updates(map[string]any{
			"unlimited_quota": true,
			"used_quota":      9,
		}).Error)

		consumed, err := reserveAuthoritativeBillingQuota(&relaycommon.RelayInfo{
			UserId: userID, TokenId: tokenID, TokenKey: tokenKey, TokenUnlimited: false,
		}, &WalletFunding{userId: userID}, quota)
		require.NoError(t, err)
		require.Zero(t, consumed)
		require.Equal(t, 800, getUserQuota(t, userID))
		require.Equal(t, 17, getTokenRemainQuota(t, tokenID))
		require.Equal(t, 9, getTokenUsedQuota(t, tokenID))
	})
}

func TestAuthoritativeBillingReservationRejectsDisabledTokenAndRollsBackWallet(t *testing.T) {
	truncate(t)
	const userID, tokenID = 876, 876
	const tokenKey = "sk-authoritative-disabled"
	seedUser(t, userID, 1000)
	seedToken(t, tokenID, userID, tokenKey, 1000)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", tokenID).Update("status", common.TokenStatusDisabled).Error)

	_, err := reserveAuthoritativeBillingQuota(&relaycommon.RelayInfo{
		UserId: userID, TokenId: tokenID, TokenKey: tokenKey,
	}, &WalletFunding{userId: userID}, 200)
	require.ErrorIs(t, err, errAuthoritativeBillingQuotaInsufficient)
	require.Equal(t, 1000, getUserQuota(t, userID))
	require.Equal(t, 1000, getTokenRemainQuota(t, tokenID))
	require.Zero(t, getTokenUsedQuota(t, tokenID))
}

func TestLedgerBillingSessionReserveIsAtomic(t *testing.T) {
	t.Run("wallet", func(t *testing.T) {
		truncate(t)
		t.Setenv(SD2SubmissionLedgerEnv, "true")
		const userID, tokenID = 878, 878
		const tokenKey = "sk-ledger-wallet-reserve"
		seedUser(t, userID, 1000)
		seedToken(t, tokenID, userID, tokenKey, 500)

		relayInfo := &relaycommon.RelayInfo{UserId: userID, TokenId: tokenID, TokenKey: tokenKey}
		session := &BillingSession{relayInfo: relayInfo, funding: &WalletFunding{userId: userID}}
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		require.Nil(t, session.preConsume(ctx, 200))
		require.Equal(t, 800, getUserQuota(t, userID))
		require.Equal(t, 300, getTokenRemainQuota(t, tokenID))
		require.Error(t, session.Reserve(common.MaxQuota+1))
		require.Error(t, session.Settle(-1))
		require.Equal(t, 800, getUserQuota(t, userID))
		require.Equal(t, 300, getTokenRemainQuota(t, tokenID))

		// The wallet decrement must roll back when the paired finite-token
		// reservation cannot be made.
		require.ErrorIs(t, session.Reserve(600), errAuthoritativeBillingQuotaInsufficient)
		require.Equal(t, 800, getUserQuota(t, userID))
		require.Equal(t, 300, getTokenRemainQuota(t, tokenID))
		require.Equal(t, 200, session.GetPreConsumedQuota())

		require.NoError(t, session.Reserve(500))
		require.Equal(t, 500, getUserQuota(t, userID))
		require.Zero(t, getTokenRemainQuota(t, tokenID))
		require.Equal(t, 500, getTokenUsedQuota(t, tokenID))
		require.Equal(t, 500, session.GetPreConsumedQuota())
	})

	t.Run("subscription", func(t *testing.T) {
		truncate(t)
		ensureAuthoritativeSubscriptionSchema(t)
		t.Setenv(SD2SubmissionLedgerEnv, "true")
		const userID, tokenID, subscriptionID = 879, 879, 879
		const tokenKey = "sk-ledger-subscription-reserve"
		seedUser(t, userID, 0)
		seedToken(t, tokenID, userID, tokenKey, 500)
		seedAuthoritativeSubscription(t, subscriptionID, userID, 1000, 0)

		relayInfo := &relaycommon.RelayInfo{UserId: userID, TokenId: tokenID, TokenKey: tokenKey}
		funding := &SubscriptionFunding{
			requestId: "ledger-subscription-reserve", userId: userID,
			modelName: SD2OriginModel, amount: 200,
		}
		session := &BillingSession{relayInfo: relayInfo, funding: funding}
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		require.Nil(t, session.preConsume(ctx, 200))
		require.Equal(t, int64(200), getSubscriptionUsed(t, subscriptionID))
		require.Equal(t, 300, getTokenRemainQuota(t, tokenID))

		require.ErrorIs(t, session.Reserve(600), errAuthoritativeBillingQuotaInsufficient)
		require.Equal(t, int64(200), getSubscriptionUsed(t, subscriptionID))
		require.Equal(t, 300, getTokenRemainQuota(t, tokenID))
		require.Equal(t, 200, session.GetPreConsumedQuota())

		require.NoError(t, session.Reserve(500))
		require.Equal(t, int64(500), getSubscriptionUsed(t, subscriptionID))
		require.Zero(t, getTokenRemainQuota(t, tokenID))
		require.Equal(t, 500, getTokenUsedQuota(t, tokenID))
		require.Equal(t, 500, session.GetPreConsumedQuota())
	})
}

func TestLedgerBillingSessionSettleUsesAuthoritativeTokenMode(t *testing.T) {
	t.Run("cached unlimited but database finite", func(t *testing.T) {
		truncate(t)
		t.Setenv(SD2SubmissionLedgerEnv, "true")
		const userID, tokenID = 880, 880
		const tokenKey = "sk-ledger-settle-finite"
		seedUser(t, userID, 1000)
		seedToken(t, tokenID, userID, tokenKey, 1000)
		relayInfo := &relaycommon.RelayInfo{
			UserId: userID, TokenId: tokenID, TokenKey: tokenKey, TokenUnlimited: true,
		}
		session := &BillingSession{relayInfo: relayInfo, funding: &WalletFunding{userId: userID}}
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		require.Nil(t, session.preConsume(ctx, 200))
		require.NoError(t, session.Settle(300))
		require.Equal(t, 700, getUserQuota(t, userID))
		require.Equal(t, 700, getTokenRemainQuota(t, tokenID))
		require.Equal(t, 300, getTokenUsedQuota(t, tokenID))
	})

	t.Run("cached finite but database unlimited", func(t *testing.T) {
		truncate(t)
		t.Setenv(SD2SubmissionLedgerEnv, "true")
		const userID, tokenID = 881, 881
		const tokenKey = "sk-ledger-settle-unlimited"
		seedUser(t, userID, 1000)
		seedToken(t, tokenID, userID, tokenKey, 17)
		require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", tokenID).Updates(map[string]any{
			"unlimited_quota": true,
			"used_quota":      9,
		}).Error)
		relayInfo := &relaycommon.RelayInfo{
			UserId: userID, TokenId: tokenID, TokenKey: tokenKey, TokenUnlimited: false,
		}
		session := &BillingSession{relayInfo: relayInfo, funding: &WalletFunding{userId: userID}}
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		require.Nil(t, session.preConsume(ctx, 200))
		require.NoError(t, session.Settle(300))
		require.Equal(t, 700, getUserQuota(t, userID))
		require.Equal(t, 17, getTokenRemainQuota(t, tokenID))
		require.Equal(t, 9, getTokenUsedQuota(t, tokenID))
	})
}

func TestLegacyUnlimitedBillingSessionDoesNotMintTokenQuotaOnRefund(t *testing.T) {
	truncate(t)
	t.Setenv(SD2SubmissionLedgerEnv, "false")
	const userID, tokenID, quota = 877, 877, 200
	const tokenKey = "sk-unlimited-refund"
	seedUser(t, userID, 1000)
	seedToken(t, tokenID, userID, tokenKey, 17)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", tokenID).Updates(map[string]any{
		"unlimited_quota": true,
		"used_quota":      9,
	}).Error)

	relayInfo := &relaycommon.RelayInfo{
		UserId: userID, TokenId: tokenID, TokenKey: tokenKey, TokenUnlimited: true,
	}
	session := &BillingSession{relayInfo: relayInfo, funding: &WalletFunding{userId: userID}}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.Nil(t, session.preConsume(ctx, quota))
	require.Zero(t, session.tokenConsumed)
	require.True(t, session.NeedsRefund())
	require.Equal(t, 800, getUserQuota(t, userID))

	session.Refund(ctx)
	require.Eventually(t, func() bool { return getUserQuota(t, userID) == 1000 }, time.Second, 10*time.Millisecond)
	require.Equal(t, 17, getTokenRemainQuota(t, tokenID))
	require.Equal(t, 9, getTokenUsedQuota(t, tokenID))
}
