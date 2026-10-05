package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestCenterCreditAndTokenAreOrganizationScopedAndRetrySafe(t *testing.T) {
	oldScale := common.QuotaPerUnit
	common.QuotaPerUnit = 500000
	t.Cleanup(func() { common.QuotaPerUnit = oldScale })
	t.Cleanup(func() {
		DB.Unscoped().Where("organization_id IN ?", []string{"org_center_a", "org_center_b", "org_center_c"}).Delete(&CenterOperation{})
		accounts := []CenterAccount{}
		DB.Where("organization_id IN ?", []string{"org_center_a", "org_center_b", "org_center_c"}).Find(&accounts)
		for _, account := range accounts {
			DB.Unscoped().Where("user_id = ?", account.UserID).Delete(&Token{})
			DB.Unscoped().Delete(&User{}, account.UserID)
		}
		DB.Where("organization_id IN ?", []string{"org_center_a", "org_center_b", "org_center_c"}).Delete(&CenterAccount{})
	})

	first, err := GrantCenterCredit("org_center_a", "credit_center_request_001", "admin@example.test", 123)
	require.NoError(t, err)
	require.Equal(t, 615000, first.Quota)
	retried, err := GrantCenterCredit("org_center_a", "credit_center_request_001", "admin@example.test", 123)
	require.NoError(t, err)
	require.Equal(t, first.Quota, retried.Quota)
	_, err = GrantCenterCredit("org_center_a", "credit_center_request_001", "admin@example.test", 124)
	require.ErrorIs(t, err, ErrCenterOperationConflict)

	other, err := GrantCenterCredit("org_center_b", "credit_center_request_002", "admin@example.test", 50)
	require.NoError(t, err)
	require.NotEqual(t, first.Id, other.Id)
	require.Equal(t, 250000, other.Quota)
	grantsA, err := ListCenterCreditGrants("org_center_a")
	require.NoError(t, err)
	require.Len(t, grantsA, 1, "retry must not create another ledger row")
	require.Equal(t, "credit_center_request_001", grantsA[0].RequestID)
	require.Equal(t, 123, grantsA[0].AmountCents)
	grantsB, err := ListCenterCreditGrants("org_center_b")
	require.NoError(t, err)
	require.Len(t, grantsB, 1)
	require.Equal(t, "credit_center_request_002", grantsB[0].RequestID)

	token, err := CreateCenterToken("org_center_a", "token_center_request_001", "Production", []string{"deepseek-flash"}, 30)
	require.NoError(t, err)
	require.True(t, token.UnlimitedQuota)
	require.True(t, token.ModelLimitsEnabled)
	require.Equal(t, first.Id, token.UserId)
	retriedToken, err := CreateCenterToken("org_center_a", "token_center_request_001", "Production", []string{"deepseek-flash"}, 30)
	require.NoError(t, err)
	require.Equal(t, token.Id, retriedToken.Id)
	_, err = CreateCenterToken("org_center_a", "token_center_request_001", "Changed", []string{"deepseek-flash"}, 30)
	require.ErrorIs(t, err, ErrCenterOperationConflict)
	_, err = GetCenterToken("org_center_b", token.Id)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)

	require.NoError(t, SetCenterAccountEnabled("org_center_a", false))
	disabled, err := GetCenterUser("org_center_a")
	require.NoError(t, err)
	require.Equal(t, common.UserStatusDisabled, disabled.Status)
	_, err = GrantCenterCredit("org_center_a", "credit_center_request_003", "admin@example.test", 1)
	require.ErrorIs(t, err, ErrCenterAccountDisabled)
	require.NoError(t, SetCenterAccountEnabled("org_center_a", true))
	require.NoError(t, SetCenterAccountEnabled("org_center_c", false))
	preFrozen, err := GetCenterUser("org_center_c")
	require.NoError(t, err)
	require.Equal(t, common.UserStatusDisabled, preFrozen.Status)
}

func TestCenterExperienceChargeIsSettledAndOrganizationScoped(t *testing.T) {
	const requestID = "202610051016420000000000000001"
	t.Cleanup(func() {
		DB.Where("request_id = ?", requestID).Delete(&Log{})
		for _, organizationID := range []string{"org_charge_a", "org_charge_b"} {
			account, err := GetCenterAccount(organizationID)
			if err == nil {
				DB.Unscoped().Delete(&User{}, account.UserID)
			}
			DB.Where("organization_id = ?", organizationID).Delete(&CenterAccount{})
			DB.Where("organization_id = ?", organizationID).Delete(&CenterOperation{})
		}
	})
	userA, err := GrantCenterCredit("org_charge_a", "credit_charge_a_request_001", "test", 100)
	require.NoError(t, err)
	_, err = GrantCenterCredit("org_charge_b", "credit_charge_b_request_001", "test", 100)
	require.NoError(t, err)
	require.NoError(t, LOG_DB.Create(&Log{UserId: userA.Id, Type: LogTypeConsume, RequestId: requestID, Quota: 362}).Error)

	quota, err := GetCenterExperienceCharge("org_charge_a", requestID)
	require.NoError(t, err)
	require.Equal(t, 362, quota)
	_, err = GetCenterExperienceCharge("org_charge_b", requestID)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}
