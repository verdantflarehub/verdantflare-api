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
