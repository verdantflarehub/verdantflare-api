package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCenterExperienceChatRejectsUnboundedOrInvalidInputBeforeRelay(t *testing.T) {
	for _, test := range []struct {
		name, organization, body string
		want                     int
	}{
		{name: "invalid organization", organization: "..", body: `{"modelId":"deepseek-flash","prompt":"hello"}`, want: http.StatusBadRequest},
		{name: "invalid json", organization: "org_test", body: `{`, want: http.StatusBadRequest},
		{name: "invalid model", organization: "org_test", body: `{"modelId":"https://upstream.test","prompt":"hello"}`, want: http.StatusBadRequest},
		{name: "empty prompt", organization: "org_test", body: `{"modelId":"deepseek-flash","prompt":"  "}`, want: http.StatusBadRequest},
		{name: "missing user key", organization: "org_test", body: `{"modelId":"deepseek-flash","prompt":"hello"}`, want: http.StatusBadRequest},
		{name: "over character limit", organization: "org_test", body: `{"modelId":"deepseek-flash","prompt":"` + strings.Repeat("中", 2001) + `"}`, want: http.StatusBadRequest},
		{name: "over body limit", organization: "org_test", body: `{"modelId":"deepseek-flash","prompt":"` + strings.Repeat("a", 17000) + `"}`, want: http.StatusRequestEntityTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			writer := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(writer)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/api/internal/center/organizations/"+test.organization+"/experience/chat-completions", strings.NewReader(test.body))
			ctx.Params = gin.Params{{Key: "organizationID", Value: test.organization}}
			CenterExperienceChat(ctx)
			require.Equal(t, test.want, writer.Code)
		})
	}
}

func TestCenterExperienceRequiresActiveUserKeyForExactModel(t *testing.T) {
	key := model.Token{Name: "user-created", Status: common.TokenStatusEnabled, ModelLimitsEnabled: true,
		ModelLimits: "deepseek-flash", Group: "default", ExpiredTime: time.Now().Add(time.Hour).Unix()}
	require.True(t, centerExperienceKeyAllowsModel(key, "deepseek-flash"))
	require.False(t, centerExperienceKeyAllowsModel(key, "deepseek-v4-pro"))
	key.Status = common.TokenStatusDisabled
	require.False(t, centerExperienceKeyAllowsModel(key, "deepseek-flash"))
	key.Status = common.TokenStatusEnabled
	key.ExpiredTime = time.Now().Add(-time.Hour).Unix()
	require.False(t, centerExperienceKeyAllowsModel(key, "deepseek-flash"))
	key.ExpiredTime = time.Now().Add(time.Hour).Unix()
	key.Name = centerExperienceTokenPrefix + "2026-10"
	require.False(t, centerExperienceKeyAllowsModel(key, "deepseek-flash"))
	key.Name = "user-created"
	key.ModelLimitsEnabled = false
	require.False(t, centerExperienceKeyAllowsModel(key, "deepseek-flash"))
}

func TestCenterExperienceChargeRejectsInvalidRequestID(t *testing.T) {
	writer := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/internal/center/organizations/org_test/experience/charges/bad", nil)
	ctx.Params = gin.Params{{Key: "organizationID", Value: "org_test"}, {Key: "requestID", Value: "bad"}}
	CenterExperienceCharge(ctx)
	require.Equal(t, http.StatusBadRequest, writer.Code)
}
