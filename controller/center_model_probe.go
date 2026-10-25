package controller

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const centerModelProbeOrganizationEnv = "CENTER_MODEL_OPS_PROBE_ORGANIZATION_ID"

// The tiered rule is intentionally pinned to the reviewed DeepSeek peak/off-
// peak expression. An administrator changing the rule must re-review the
// bound before another paid legacy probe is allowed.
const centerV4ProProbeExpr = `(weekday("UTC") >= 1 && weekday("UTC") <= 5 && ((hour("UTC") >= 1 && hour("UTC") < 4) || (hour("UTC") >= 6 && hour("UTC") < 10))) ? tier("peak", p * 1.32 + cr * 0.044 + c * 3.96) : tier("off_peak", p * 0.66 + cr * 0.022 + c * 1.98)`

func centerModelProbePriceBound(modelID, mode string, fixedPrice *float64, ratio, completion float64, expression string) bool {
	const maxPromptTokens, maxOutputTokens, maxUSD = 512.0, 16.0, 0.01
	if modelID != "deepseek-flash" && modelID != "deepseek-v4-pro" &&
		mode == billing_setting.BillingModeRatio && fixedPrice != nil {
		return *fixedPrice > 0 && *fixedPrice <= maxUSD
	}
	if modelID == "deepseek-flash" && mode == billing_setting.BillingModeRatio && fixedPrice == nil &&
		ratio == 0.75 && completion == 1 {
		return (maxPromptTokens*ratio+maxOutputTokens*ratio*completion)/common.QuotaPerUnit <= maxUSD
	}
	if modelID == "deepseek-v4-pro" && mode == billing_setting.BillingModeTieredExpr && fixedPrice == nil &&
		strings.TrimSpace(expression) == centerV4ProProbeExpr {
		// The pinned expression's peak rates dominate its off-peak and cache
		// rates for this text-only request. No supplier credential is exposed.
		return (maxPromptTokens*1.32+maxOutputTokens*3.96)/1_000_000 <= maxUSD
	}
	return false
}

// CenterModelOpsProbe performs one bounded, paid text call on the dedicated
// center-internal route. A durable request ID prevents blind paid retries.
func CenterModelOpsProbe(c *gin.Context) {
	organizationID := strings.TrimSpace(os.Getenv(centerModelProbeOrganizationEnv))
	if organizationID == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "internal probe organization is not configured"})
		return
	}
	var raw map[string]any
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 4<<10), &raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid probe request"})
		return
	}
	for key := range raw {
		if key != "modelId" && key != "requestId" && key != "expectedGatewayRevision" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "unsupported probe field"})
			return
		}
	}
	modelID, _ := raw["modelId"].(string)
	requestID, _ := raw["requestId"].(string)
	expected, _ := raw["expectedGatewayRevision"].(string)
	if !centerModelIDPattern.MatchString(modelID) || strings.Contains(modelID, "..") ||
		!centerRequestIDPattern.MatchString(requestID) || len(expected) != 64 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid probe request"})
		return
	}
	snapshot, status := centerModelOpsSnapshotData(modelID)
	if status != http.StatusOK {
		c.JSON(status, gin.H{"success": false, "message": "model configuration unavailable"})
		return
	}
	if snapshot["revision"] != expected {
		c.JSON(http.StatusConflict, gin.H{"success": false, "message": "model configuration changed"})
		return
	}
	pricing, _ := snapshot["pricing"].(gin.H)
	price, priced := ratio_setting.GetModelPriceCopy()[modelID]
	var fixedPrice *float64
	if priced {
		fixedPrice = &price
	}
	ratio, explicitRatio, _ := ratio_setting.GetModelRatio(modelID)
	if !explicitRatio {
		ratio = 0
	}
	expr, _ := billing_setting.GetBillingExpr(modelID)
	mode, _ := pricing["billingMode"].(string)
	if !centerModelProbePriceBound(modelID, mode, fixedPrice, ratio, ratio_setting.GetCompletionRatio(modelID), expr) ||
		!ratio_setting.ContainsGroupRatio("center-internal") || ratio_setting.GetGroupRatio("center-internal") != 1 {
		c.JSON(http.StatusConflict, gin.H{"success": false, "message": "bounded internal probe pricing is not ready"})
		return
	}
	available := false
	for _, candidate := range model.GetGroupEnabledModels("center-internal") {
		if candidate == modelID {
			available = true
			break
		}
	}
	if !available {
		c.JSON(http.StatusConflict, gin.H{"success": false, "message": "internal model route is not enabled"})
		return
	}
	user, err := model.GetCenterUser(organizationID)
	if err != nil || user.Status != common.UserStatusEnabled {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "internal probe account is unavailable"})
		return
	}
	quotaLimit := common.QuotaFromFloat(0.01 * common.QuotaPerUnit)
	if quotaLimit <= 0 || user.Quota < quotaLimit {
		c.JSON(http.StatusPaymentRequired, gin.H{"success": false, "message": "internal probe budget is unavailable"})
		return
	}
	token, err := model.BeginCenterModelProbe(organizationID, modelID, requestID, expected, quotaLimit)
	if err != nil {
		if errors.Is(err, model.ErrCenterModelProbeAlreadyStarted) {
			c.JSON(http.StatusConflict, gin.H{"success": false, "message": "probe already submitted; inspect charge by request ID"})
		} else {
			c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "probe token unavailable"})
		}
		return
	}
	defer func() { _ = model.EndCenterModelProbe(token.Id) }()
	userCache, err := model.GetUserCache(user.Id)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "probe account cache unavailable"})
		return
	}
	userCache.WriteContext(c)
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "center-internal")
	if err := middleware.SetupContextForToken(c, &token); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "probe token setup failed"})
		return
	}
	relayBody, err := common.Marshal(gin.H{"model": modelID, "stream": false, "max_tokens": 16,
		"messages": []gin.H{{"role": "user", "content": "Reply OK."}}})
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "probe request unavailable"})
		return
	}
	c.Set(common.RequestIdKey, requestID)
	c.Set("center_model_probe_quota_limit", quotaLimit)
	c.Header(common.RequestIdKey, requestID)
	c.Request.Body = io.NopCloser(bytes.NewReader(relayBody))
	c.Request.ContentLength = int64(len(relayBody))
	c.Request.Header.Del("Authorization")
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.URL.Path = "/v1/chat/completions"
	c.Request.URL.RawPath = ""
	middleware.Distribute()(c)
	if !c.IsAborted() {
		Relay(c, types.RelayFormatOpenAI)
	}
}

func CenterModelOpsProbeCharge(c *gin.Context) {
	organizationID := strings.TrimSpace(os.Getenv(centerModelProbeOrganizationEnv))
	requestID := c.Param("requestID")
	if organizationID == "" || !centerRequestIDPattern.MatchString(requestID) {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid probe lookup"})
		return
	}
	operation, err := model.CenterModelProbeOperation(organizationID, requestID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "probe request not found"})
		} else {
			c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "probe lookup unavailable"})
		}
		return
	}
	quota, err := model.GetCenterExperienceCharge(organizationID, requestID)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"success": false, "message": "probe charge not settled; do not retry blindly"})
		return
	}
	limit := common.QuotaFromFloat(0.01 * common.QuotaPerUnit)
	verified := quota > 0 && quota <= limit
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"requestId": requestID, "modelId": operation.Actor, "gatewayRevision": operation.PayloadHash, "billedQuota": quota,
		"billedUsd": float64(quota) / common.QuotaPerUnit, "maxUsd": 0.01, "verified": verified,
	}})
}
