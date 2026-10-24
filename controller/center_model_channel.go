package controller

import (
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
)

type centerModelAliasInput struct {
	ModelID                 string `json:"modelId"`
	SourceModelID           string `json:"sourceModelId"`
	SourceChannelID         int    `json:"sourceChannelId"`
	RequestID               string `json:"requestId"`
	ExpectedGatewayRevision string `json:"expectedGatewayRevision"`
}

type centerModelChannelCreateInput struct {
	ModelID                 string `json:"modelId"`
	UpstreamModelID         string `json:"upstreamModelId"`
	Name                    string `json:"name"`
	Credential              string `json:"credential"`
	RequestID               string `json:"requestId"`
	ExpectedGatewayRevision string `json:"expectedGatewayRevision"`
}

// CenterModelOpsCreateChannel is a service-to-service credential sink. Control
// must receive the credential through its separate authenticated upload route,
// never from an MCP tool argument. Only a dedicated center-internal DeepSeek
// channel is allowed in this initial adapter.
func CenterModelOpsCreateChannel(c *gin.Context) {
	var raw map[string]any
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 8<<10), &raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid channel request"})
		return
	}
	allowed := map[string]bool{"modelId": true, "upstreamModelId": true, "name": true,
		"credential": true, "requestId": true, "expectedGatewayRevision": true}
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
	var input centerModelChannelCreateInput
	if err := common.Unmarshal(encoded, &input); err != nil ||
		!centerModelIDPattern.MatchString(input.ModelID) || !centerModelIDPattern.MatchString(input.UpstreamModelID) ||
		strings.Contains(input.ModelID, "..") || strings.Contains(input.UpstreamModelID, "..") ||
		len(input.Name) < 3 || len(input.Name) > 80 || strings.ContainsAny(input.Name, "\x00\r\n") ||
		len(input.Credential) < 16 || len(input.Credential) > 2048 ||
		strings.ContainsAny(input.Credential, "\x00\r\n\t ") ||
		!centerRequestIDPattern.MatchString(input.RequestID) || len(input.ExpectedGatewayRevision) != 64 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid channel request"})
		return
	}
	_, groupPersisted, groupErr := model.CenterInternalGroupSnapshot()
	if groupErr != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "internal group configuration unavailable"})
		return
	}
	if !groupPersisted || !ratio_setting.ContainsGroupRatio("center-internal") {
		c.JSON(http.StatusConflict, gin.H{"success": false, "message": "internal group pricing is not configured"})
		return
	}
	before, status := centerModelOpsSnapshotData(input.ModelID)
	if status != http.StatusOK {
		c.JSON(status, gin.H{"success": false, "message": "model configuration unavailable"})
		return
	}
	sequence := before["sequence"].(uint64)
	revisionMatches := before["revision"] == input.ExpectedGatewayRevision
	hash := model.CenterChannelCreatePayloadHash(input.ModelID, input.UpstreamModelID, input.Name, input.Credential)
	channelID, replayed, err := model.CreateCenterModelChannel(input.ModelID, input.UpstreamModelID,
		input.Name, input.Credential, input.RequestID, hash,
		func(current uint64) bool { return revisionMatches && current == sequence })
	if err != nil {
		switch {
		case errors.Is(err, model.ErrCenterModelRevision), errors.Is(err, model.ErrCenterOperationConflict),
			errors.Is(err, model.ErrCenterModelChannelInvalid):
			c.JSON(http.StatusConflict, gin.H{"success": false, "message": "model channel changed or already exists"})
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
		"modelId": input.ModelID, "channelId": channelID, "revision": after["revision"],
		"channels": after["channels"], "replayed": replayed,
	}})
}

func CenterModelOpsDisableChannel(c *gin.Context) {
	var raw map[string]any
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 4<<10), &raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid channel request"})
		return
	}
	for key := range raw {
		if key != "modelId" && key != "channelId" && key != "requestId" && key != "expectedGatewayRevision" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "unsupported channel field"})
			return
		}
	}
	encoded, err := common.Marshal(raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid channel request"})
		return
	}
	var input struct {
		ModelID                 string `json:"modelId"`
		ChannelID               int    `json:"channelId"`
		RequestID               string `json:"requestId"`
		ExpectedGatewayRevision string `json:"expectedGatewayRevision"`
	}
	if err := common.Unmarshal(encoded, &input); err != nil || !centerModelIDPattern.MatchString(input.ModelID) ||
		strings.Contains(input.ModelID, "..") || input.ChannelID <= 0 ||
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
	hash := model.CenterChannelDisablePayloadHash(input.ModelID, input.ChannelID)
	replayed, err := model.DisableCenterModelChannel(input.ModelID, input.ChannelID, input.RequestID, hash,
		func(current uint64) bool { return revisionMatches && current == sequence })
	if err != nil {
		if errors.Is(err, model.ErrCenterModelRevision) || errors.Is(err, model.ErrCenterOperationConflict) || errors.Is(err, model.ErrCenterModelChannelInvalid) {
			c.JSON(http.StatusConflict, gin.H{"success": false, "message": "channel changed or shared; disable refused"})
		} else {
			c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "channel disable unavailable"})
		}
		return
	}
	after, status := centerModelOpsSnapshotData(input.ModelID)
	if status != http.StatusOK {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "channel disabled; readback unavailable"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"modelId": input.ModelID, "revision": after["revision"], "channels": after["channels"], "replayed": replayed,
	}})
}

func CenterModelOpsRotateCredential(c *gin.Context) {
	var raw map[string]any
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 8<<10), &raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid credential request"})
		return
	}
	for key := range raw {
		if key != "modelId" && key != "channelId" && key != "credential" && key != "requestId" && key != "expectedGatewayRevision" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "unsupported credential field"})
			return
		}
	}
	encoded, err := common.Marshal(raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid credential request"})
		return
	}
	var input struct {
		ModelID                 string `json:"modelId"`
		ChannelID               int    `json:"channelId"`
		Credential              string `json:"credential"`
		RequestID               string `json:"requestId"`
		ExpectedGatewayRevision string `json:"expectedGatewayRevision"`
	}
	if err := common.Unmarshal(encoded, &input); err != nil || !centerModelIDPattern.MatchString(input.ModelID) ||
		strings.Contains(input.ModelID, "..") || input.ChannelID <= 0 ||
		len(input.Credential) < 16 || len(input.Credential) > 2048 || strings.ContainsAny(input.Credential, "\x00\r\n\t ") ||
		!centerRequestIDPattern.MatchString(input.RequestID) || len(input.ExpectedGatewayRevision) != 64 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid credential request"})
		return
	}
	before, status := centerModelOpsSnapshotData(input.ModelID)
	if status != http.StatusOK {
		c.JSON(status, gin.H{"success": false, "message": "model configuration unavailable"})
		return
	}
	sequence := before["sequence"].(uint64)
	revisionMatches := before["revision"] == input.ExpectedGatewayRevision
	hash := model.CenterCredentialRotatePayloadHash(input.ModelID, input.ChannelID, input.Credential)
	replayed, err := model.RotateCenterModelCredential(input.ModelID, input.ChannelID, input.Credential,
		input.RequestID, hash, func(current uint64) bool { return revisionMatches && current == sequence })
	if err != nil {
		if errors.Is(err, model.ErrCenterModelRevision) || errors.Is(err, model.ErrCenterOperationConflict) || errors.Is(err, model.ErrCenterModelChannelInvalid) {
			c.JSON(http.StatusConflict, gin.H{"success": false, "message": "channel changed or shared; rotation refused"})
		} else {
			c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "credential rotation unavailable"})
		}
		return
	}
	after, status := centerModelOpsSnapshotData(input.ModelID)
	if status != http.StatusOK {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "credential rotated; readback unavailable"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"modelId": input.ModelID, "revision": after["revision"], "channels": after["channels"], "replayed": replayed,
	}})
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
