package controller

import (
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

type centerModelMetadataInput struct {
	ModelID                 string `json:"modelId"`
	Description             string `json:"description"`
	Endpoints               string `json:"endpoints"`
	Status                  int    `json:"status"`
	RequestID               string `json:"requestId"`
	ExpectedGatewayRevision string `json:"expectedGatewayRevision"`
}

func CenterModelOpsConfigureMetadata(c *gin.Context) {
	var raw map[string]any
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 8<<10), &raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid metadata request"})
		return
	}
	allowed := map[string]bool{"modelId": true, "description": true, "endpoints": true,
		"status": true, "requestId": true, "expectedGatewayRevision": true}
	for key := range raw {
		if !allowed[key] {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "unsupported metadata field"})
			return
		}
	}
	encoded, err := common.Marshal(raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid metadata request"})
		return
	}
	var input centerModelMetadataInput
	if err := common.Unmarshal(encoded, &input); err != nil || !centerModelIDPattern.MatchString(input.ModelID) ||
		strings.Contains(input.ModelID, "..") || len(input.Description) < 10 || len(input.Description) > 2000 ||
		strings.ContainsAny(input.Description, "\x00") || input.Endpoints != "/v1/chat/completions" ||
		(input.Status != 0 && input.Status != 1) || !centerRequestIDPattern.MatchString(input.RequestID) ||
		len(input.ExpectedGatewayRevision) != 64 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid metadata request"})
		return
	}
	before, status := centerModelOpsSnapshotData(input.ModelID)
	if status != http.StatusOK {
		c.JSON(status, gin.H{"success": false, "message": "model configuration unavailable"})
		return
	}
	sequence := before["sequence"].(uint64)
	revisionMatches := before["revision"] == input.ExpectedGatewayRevision
	hash := model.CenterMetadataPayloadHash(input.ModelID, input.Description, input.Endpoints, input.Status)
	replayed, err := model.ConfigureCenterModelMetadata(input.ModelID, input.Description, input.Endpoints,
		input.Status, input.RequestID, hash, func(current uint64) bool { return revisionMatches && current == sequence })
	if err != nil {
		if errors.Is(err, model.ErrCenterModelRevision) || errors.Is(err, model.ErrCenterOperationConflict) {
			c.JSON(http.StatusConflict, gin.H{"success": false, "message": "model metadata revision or request ID changed"})
		} else {
			c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "model metadata write unavailable"})
		}
		return
	}
	after, status := centerModelOpsSnapshotData(input.ModelID)
	if status != http.StatusOK {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "model metadata saved; readback unavailable"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"modelId": input.ModelID, "revision": after["revision"], "metadata": after["metadata"], "replayed": replayed,
	}})
}
