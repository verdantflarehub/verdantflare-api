package controller

import (
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

type centerPublicRouteInput struct {
	ModelID                 string `json:"modelId"`
	SourceChannelID         int    `json:"sourceChannelId"`
	PublicChannelID         int    `json:"publicChannelId"`
	CatalogToken            string `json:"catalogToken"`
	RequestID               string `json:"requestId"`
	ExpectedGatewayRevision string `json:"expectedGatewayRevision"`
}

func centerPublicRouteRequest(c *gin.Context, activate bool) (centerPublicRouteInput, bool) {
	var raw map[string]any
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 4<<10), &raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid public route request"})
		return centerPublicRouteInput{}, false
	}
	for key := range raw {
		if key != "modelId" && key != "sourceChannelId" && key != "publicChannelId" && key != "catalogToken" && key != "requestId" && key != "expectedGatewayRevision" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "unsupported public route field"})
			return centerPublicRouteInput{}, false
		}
	}
	encoded, err := common.Marshal(raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid public route request"})
		return centerPublicRouteInput{}, false
	}
	var input centerPublicRouteInput
	if err := common.Unmarshal(encoded, &input); err != nil || !centerModelIDPattern.MatchString(input.ModelID) ||
		strings.Contains(input.ModelID, "..") || input.SourceChannelID <= 0 ||
		(activate && input.PublicChannelID <= 0) || (!activate && input.PublicChannelID != 0) ||
		(activate && input.CatalogToken != "") || (!activate && (!strings.HasPrefix(input.CatalogToken, "sk-") || len(input.CatalogToken) < 24 || len(input.CatalogToken) > 160)) ||
		!centerRequestIDPattern.MatchString(input.RequestID) || len(input.ExpectedGatewayRevision) != 64 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid public route request"})
		return centerPublicRouteInput{}, false
	}
	return input, true
}

func CenterModelOpsStagePublicRoute(c *gin.Context) {
	input, ok := centerPublicRouteRequest(c, false)
	if !ok {
		return
	}
	before, status := centerModelOpsSnapshotData(input.ModelID)
	if status != http.StatusOK {
		c.JSON(status, gin.H{"success": false, "message": "model configuration unavailable"})
		return
	}
	sequence := before["sequence"].(uint64)
	revisionMatches := before["revision"] == input.ExpectedGatewayRevision
	hash := model.CenterPublicRouteStagePayloadHash(input.ModelID, input.SourceChannelID, input.CatalogToken)
	publicID, replayed, err := model.StageCenterPublicRoute(input.ModelID, input.SourceChannelID, input.CatalogToken, input.RequestID, hash,
		func(current uint64) bool { return revisionMatches && current == sequence })
	if err != nil {
		centerPublicRouteError(c, err)
		return
	}
	centerPublicRouteReadback(c, input.ModelID, input.SourceChannelID, publicID, replayed)
}

func CenterModelOpsActivatePublicRoute(c *gin.Context) {
	input, ok := centerPublicRouteRequest(c, true)
	if !ok {
		return
	}
	before, status := centerModelOpsSnapshotData(input.ModelID)
	if status != http.StatusOK {
		c.JSON(status, gin.H{"success": false, "message": "model configuration unavailable"})
		return
	}
	sequence := before["sequence"].(uint64)
	revisionMatches := before["revision"] == input.ExpectedGatewayRevision
	hash := model.CenterPublicRoutePayloadHash(input.ModelID, input.SourceChannelID, input.PublicChannelID)
	replayed, err := model.ActivateCenterPublicRoute(input.ModelID, input.SourceChannelID, input.PublicChannelID,
		input.RequestID, hash, func(current uint64) bool { return revisionMatches && current == sequence })
	if err != nil {
		centerPublicRouteError(c, err)
		return
	}
	centerPublicRouteReadback(c, input.ModelID, input.SourceChannelID, input.PublicChannelID, replayed)
}

func centerPublicRouteError(c *gin.Context, err error) {
	if errors.Is(err, model.ErrCenterModelRevision) || errors.Is(err, model.ErrCenterOperationConflict) || errors.Is(err, model.ErrCenterModelChannelInvalid) {
		c.JSON(http.StatusConflict, gin.H{"success": false, "message": "public route changed or unsafe"})
	} else {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "public route write unavailable"})
	}
}

func centerPublicRouteReadback(c *gin.Context, modelID string, sourceID, publicID int, replayed bool) {
	after, status := centerModelOpsSnapshotData(modelID)
	if status != http.StatusOK {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "public route saved; readback unavailable"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"modelId": modelID, "sourceChannelId": sourceID, "publicChannelId": publicID,
		"revision": after["revision"], "channels": after["channels"], "replayed": replayed,
	}})
}
