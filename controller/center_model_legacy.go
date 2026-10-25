package controller

import (
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

func CenterModelOpsPrepareLegacy(c *gin.Context) {
	var raw map[string]any
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 4<<10), &raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid legacy prepare request"})
		return
	}
	for key := range raw {
		if key != "modelId" && key != "legacyChannelId" && key != "requestId" && key != "expectedGatewayRevision" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "unsupported legacy prepare field"})
			return
		}
	}
	encoded, err := common.Marshal(raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid legacy prepare request"})
		return
	}
	var input struct {
		ModelID                 string `json:"modelId"`
		LegacyChannelID         int    `json:"legacyChannelId"`
		RequestID               string `json:"requestId"`
		ExpectedGatewayRevision string `json:"expectedGatewayRevision"`
	}
	if err := common.Unmarshal(encoded, &input); err != nil ||
		(input.ModelID != "deepseek-flash" && input.ModelID != "deepseek-v4-pro") ||
		input.LegacyChannelID <= 0 || !centerRequestIDPattern.MatchString(input.RequestID) || len(input.ExpectedGatewayRevision) != 64 ||
		strings.TrimSpace(input.ExpectedGatewayRevision) != input.ExpectedGatewayRevision {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid legacy prepare request"})
		return
	}
	before, status := centerModelOpsSnapshotData(input.ModelID)
	if status != http.StatusOK {
		c.JSON(status, gin.H{"success": false, "message": "model configuration unavailable"})
		return
	}
	sequence := before["sequence"].(uint64)
	revisionMatches := before["revision"] == input.ExpectedGatewayRevision
	hash := model.CenterLegacyPreparePayloadHash(input.ModelID, input.LegacyChannelID)
	channelID, replayed, err := model.PrepareCenterLegacyModelChannel(input.ModelID, input.LegacyChannelID, input.RequestID, hash,
		func(current uint64) bool { return revisionMatches && current == sequence })
	if err != nil {
		centerPublicRouteError(c, err)
		return
	}
	after, status := centerModelOpsSnapshotData(input.ModelID)
	if status != http.StatusOK {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "legacy channel saved; readback unavailable"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"modelId": input.ModelID, "legacyChannelId": input.LegacyChannelID,
		"channelId": channelID, "revision": after["revision"], "channels": after["channels"], "replayed": replayed,
	}})
}

func CenterModelOpsRecoverLegacy(c *gin.Context) {
	var raw map[string]any
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 4<<10), &raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid legacy recovery request"})
		return
	}
	for key := range raw {
		if key != "modelId" && key != "legacyChannelId" && key != "sourceChannelId" && key != "publicChannelId" &&
			key != "action" && key != "requestId" && key != "expectedGatewayRevision" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "unsupported legacy recovery field"})
			return
		}
	}
	encoded, err := common.Marshal(raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid legacy recovery request"})
		return
	}
	var input struct {
		ModelID                 string `json:"modelId"`
		LegacyChannelID         int    `json:"legacyChannelId"`
		SourceChannelID         int    `json:"sourceChannelId"`
		PublicChannelID         int    `json:"publicChannelId"`
		Action                  string `json:"action"`
		RequestID               string `json:"requestId"`
		ExpectedGatewayRevision string `json:"expectedGatewayRevision"`
	}
	if err := common.Unmarshal(encoded, &input); err != nil ||
		(input.ModelID != "deepseek-flash" && input.ModelID != "deepseek-v4-pro") ||
		input.LegacyChannelID <= 0 || input.SourceChannelID <= 0 || input.PublicChannelID <= 0 ||
		(input.Action != "abort" && input.Action != "rollback") ||
		!centerRequestIDPattern.MatchString(input.RequestID) || len(input.ExpectedGatewayRevision) != 64 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid legacy recovery request"})
		return
	}
	before, status := centerModelOpsSnapshotData(input.ModelID)
	if status != http.StatusOK {
		c.JSON(status, gin.H{"success": false, "message": "model configuration unavailable"})
		return
	}
	sequence := before["sequence"].(uint64)
	revisionMatches := before["revision"] == input.ExpectedGatewayRevision
	hash := model.CenterLegacyRecoveryPayloadHash(input.ModelID, input.LegacyChannelID, input.SourceChannelID,
		input.PublicChannelID, input.Action)
	replayed, err := model.RecoverCenterLegacyPublicRoute(input.ModelID, input.LegacyChannelID, input.SourceChannelID,
		input.PublicChannelID, input.Action == "rollback", input.RequestID, hash,
		func(current uint64) bool { return revisionMatches && current == sequence })
	if err != nil {
		centerPublicRouteError(c, err)
		return
	}
	after, status := centerModelOpsSnapshotData(input.ModelID)
	if status != http.StatusOK {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "legacy recovery saved; readback unavailable"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"modelId": input.ModelID, "legacyChannelId": input.LegacyChannelID,
		"sourceChannelId": input.SourceChannelID, "publicChannelId": input.PublicChannelID,
		"action": input.Action, "revision": after["revision"], "channels": after["channels"], "replayed": replayed,
	}})
}
