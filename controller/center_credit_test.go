package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCenterCreditAcceptsExistingMixedCaseOrganizationID(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.CenterAccount{}, &model.CenterOperation{}))

	const organizationID = "org_DV97FRRNE4Q"
	const requestID = "credit_mixed_case_org_001"
	body := `{"requestId":"` + requestID + `","amountCents":100,"actor":"test-admin"}`
	for _, wantQuota := range []int{500000, 500000} {
		writer := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(writer)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/internal/center/organizations/"+organizationID+"/credit-grants", strings.NewReader(body))
		ctx.Params = gin.Params{{Key: "organizationID", Value: organizationID}}
		CenterGrantCredit(ctx)
		require.Equal(t, http.StatusOK, writer.Code, writer.Body.String())
		user, err := model.GetCenterUser(organizationID)
		require.NoError(t, err)
		assert.Equal(t, wantQuota, user.Quota)
	}
	balanceWriter := httptest.NewRecorder()
	balanceContext, _ := gin.CreateTestContext(balanceWriter)
	balanceContext.Request = httptest.NewRequest(http.MethodGet, "/api/internal/center/organizations/"+organizationID+"/balance", nil)
	balanceContext.Params = gin.Params{{Key: "organizationID", Value: organizationID}}
	CenterOrganizationBalance(balanceContext)
	require.Equal(t, http.StatusOK, balanceWriter.Code, balanceWriter.Body.String())
	assert.Contains(t, balanceWriter.Body.String(), `"remainingQuota":500000`)
}
