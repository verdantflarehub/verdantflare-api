package controller

import (
	"errors"
	"math"
	"net/http"
	"regexp"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/gin-gonic/gin"
)

var centerModelIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)

type centerModelBillingInput struct {
	ModelID                 string   `json:"modelId"`
	RequestID               string   `json:"requestId"`
	ExpectedGatewayRevision string   `json:"expectedGatewayRevision"`
	Mode                    string   `json:"mode"`
	BillingExpr             string   `json:"billingExpr"`
	PriceUSD                *float64 `json:"priceUsd"`
}

func (input centerModelBillingInput) validate(preview bool) string {
	if !centerModelIDPattern.MatchString(input.ModelID) || strings.Contains(input.ModelID, "..") {
		return "invalid model ID"
	}
	if !preview && (!centerRequestIDPattern.MatchString(input.RequestID) || len(input.ExpectedGatewayRevision) != 64) {
		return "request ID and gateway revision are required"
	}
	switch input.Mode {
	case billing_setting.BillingModeTieredExpr:
		if input.PriceUSD != nil || len(input.BillingExpr) == 0 || len(input.BillingExpr) > 4096 {
			return "invalid tiered billing fields"
		}
		if err := billing_setting.SmokeTestExpr(input.BillingExpr); err != nil {
			return "invalid billing expression"
		}
	case "per_request":
		if input.PriceUSD == nil || input.BillingExpr != "" || math.IsNaN(*input.PriceUSD) || math.IsInf(*input.PriceUSD, 0) || *input.PriceUSD <= 0 || *input.PriceUSD > 1000 {
			return "priceUsd must be greater than zero and no more than 1000"
		}
	default:
		return "unsupported billing mode"
	}
	return ""
}

func parseCenterModelBilling(c *gin.Context, preview bool) (centerModelBillingInput, bool) {
	var raw map[string]any
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10), &raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid billing request"})
		return centerModelBillingInput{}, false
	}
	allowed := map[string]bool{"modelId": true, "requestId": true, "expectedGatewayRevision": true, "mode": true, "billingExpr": true, "priceUsd": true}
	for key := range raw {
		if !allowed[key] {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "unsupported billing field"})
			return centerModelBillingInput{}, false
		}
	}
	encoded, err := common.Marshal(raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid billing request"})
		return centerModelBillingInput{}, false
	}
	var input centerModelBillingInput
	if err := common.Unmarshal(encoded, &input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid billing request"})
		return centerModelBillingInput{}, false
	}
	if message := input.validate(preview); message != "" {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": message})
		return centerModelBillingInput{}, false
	}
	return input, true
}

// CenterModelOpsBillingPreview validates a bounded pricing rule but never
// persists it. This is not a proof that a provider call was billed correctly.
func CenterModelOpsBillingPreview(c *gin.Context) {
	input, ok := parseCenterModelBilling(c, true)
	if !ok {
		return
	}
	data, status := centerModelOpsSnapshotData(input.ModelID)
	if status != http.StatusOK {
		c.JSON(status, gin.H{"success": false, "message": "model configuration unavailable"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"modelId": input.ModelID, "currentRevision": data["revision"], "mode": input.Mode,
		"priceUsd": input.PriceUSD, "validated": true, "actualChargeVerified": false,
	}})
}

// CenterModelOpsBillingApply performs a model-scoped, idempotent write. It
// does not make the model public or assert that a paid probe has passed.
func CenterModelOpsBillingApply(c *gin.Context) {
	input, ok := parseCenterModelBilling(c, false)
	if !ok {
		return
	}
	data, status := centerModelOpsSnapshotData(input.ModelID)
	if status != http.StatusOK {
		c.JSON(status, gin.H{"success": false, "message": "model configuration unavailable"})
		return
	}
	sequence, ok := data["sequence"].(uint64)
	if !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "model revision unavailable"})
		return
	}
	// A replay is resolved by the durable request ledger before the revision
	// check; changed payloads with the same ID remain conflicts.
	revisionMatches := data["revision"] == input.ExpectedGatewayRevision
	if !revisionMatches {
		// Preserve an idempotent retry after the first response was lost.
		sequence = ^uint64(0)
	}
	hash := model.CenterBillingPayloadHash(input.ModelID, input.Mode, input.BillingExpr, input.PriceUSD)
	replayed, err := model.ApplyCenterModelBilling(input.ModelID, input.RequestID, hash,
		func(current uint64) bool { return revisionMatches && current == sequence }, input.Mode, input.BillingExpr, input.PriceUSD)
	if err != nil {
		switch {
		case errors.Is(err, model.ErrCenterModelRevision), errors.Is(err, model.ErrCenterOperationConflict):
			c.JSON(http.StatusConflict, gin.H{"success": false, "message": "model configuration revision or request ID changed"})
		default:
			c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "billing write unavailable"})
		}
		return
	}
	updated, status := centerModelOpsSnapshotData(input.ModelID)
	if status != http.StatusOK {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "billing saved; readback unavailable"})
		return
	}
	pricing, ok := updated["pricing"].(gin.H)
	if !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "billing readback invalid"})
		return
	}
	matched := false
	if input.Mode == billing_setting.BillingModeTieredExpr {
		matched = pricing["billingMode"] == billing_setting.BillingModeTieredExpr && pricing["billingExpr"] == input.BillingExpr
	} else {
		matched = pricing["billingMode"] == billing_setting.BillingModeRatio && pricing["modelPrice"] == *input.PriceUSD
	}
	if !matched {
		c.JSON(http.StatusConflict, gin.H{"success": false, "message": "billing was changed after this request; read back current configuration"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"modelId": input.ModelID, "revision": updated["revision"], "pricing": pricing, "replayed": replayed,
	}})
}
