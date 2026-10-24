package controller

import (
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

type centerModelAliasInput struct {
	ModelID                 string `json:"modelId"`
	SourceModelID           string `json:"sourceModelId"`
	SourceChannelID         int    `json:"sourceChannelId"`
	RequestID               string `json:"requestId"`
	ExpectedGatewayRevision string `json:"expectedGatewayRevision"`
}

// CenterModelOpsAttachAlias is the first narrow channel write. It reuses one
// configured channel and its exact upstream model; it cannot create channels,
// accept upstream secrets, or alter an existing target model.
func CenterModelOpsAttachAlias(c *gin.Context) {
	var raw map[string]any
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10), &raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid channel request"})
		return
	}
	allowed := map[string]bool{"modelId": true, "sourceModelId": true, "sourceChannelId": true,
		"requestId": true, "expectedGatewayRevision": true}
	for key := range raw {
		if !allowed[key] {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "unsupported channel field"})
			return
		}
	}
	encoded, err := common.Marshal(raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid channel request"})
		return
	}
	var input centerModelAliasInput
	if err := common.Unmarshal(encoded, &input); err != nil || !centerModelIDPattern.MatchString(input.ModelID) ||
		!centerModelIDPattern.MatchString(input.SourceModelID) || input.ModelID == input.SourceModelID ||
		strings.Contains(input.ModelID, "..") || input.SourceChannelID <= 0 ||
		!centerRequestIDPattern.MatchString(input.RequestID) || len(input.ExpectedGatewayRevision) != 64 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid channel request"})
		return
	}
	before, status := centerModelOpsSnapshotData(input.ModelID)
	if status != http.StatusOK {
		c.JSON(status, gin.H{"success": false, "message": "model configuration unavailable"})
		return
	}
	sequence := before["sequence"].(uint64)
	revisionMatches := before["revision"] == input.ExpectedGatewayRevision
	hash := model.CenterAliasPayloadHash(input.ModelID, input.SourceModelID, input.SourceChannelID)
	replayed, err := model.AttachCenterModelAlias(input.ModelID, input.SourceModelID,
		input.SourceChannelID, input.RequestID, hash,
		func(current uint64) bool { return revisionMatches && current == sequence })
	if err != nil {
		switch {
		case errors.Is(err, model.ErrCenterModelRevision), errors.Is(err, model.ErrCenterOperationConflict):
			c.JSON(http.StatusConflict, gin.H{"success": false, "message": "model configuration revision or request ID changed"})
		case errors.Is(err, model.ErrCenterModelChannelInvalid):
			c.JSON(http.StatusConflict, gin.H{"success": false, "message": "source channel is unavailable for a new alias"})
		default:
			c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "channel write unavailable"})
		}
		return
	}
	after, status := centerModelOpsSnapshotData(input.ModelID)
	if status != http.StatusOK {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "channel saved; readback unavailable"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"modelId": input.ModelID, "revision": after["revision"], "channels": after["channels"], "replayed": replayed,
	}})
}
