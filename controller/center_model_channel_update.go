package controller

import (
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

func CenterModelOpsUpdateChannel(c *gin.Context) {
	var raw map[string]any
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 4<<10), &raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid channel update"})
		return
	}
	for key := range raw {
		if key != "modelId" && key != "channelId" && key != "name" && key != "upstreamModelId" && key != "weight" && key != "priority" && key != "requestId" && key != "expectedGatewayRevision" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "unsupported channel field"})
			return
		}
	}
	encoded, err := common.Marshal(raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid channel update"})
		return
	}
	var input struct {
		ModelID                 string `json:"modelId"`
		ChannelID               int    `json:"channelId"`
		Name                    string `json:"name"`
		UpstreamModelID         string `json:"upstreamModelId"`
		Weight                  int    `json:"weight"`
		Priority                int64  `json:"priority"`
		RequestID               string `json:"requestId"`
		ExpectedGatewayRevision string `json:"expectedGatewayRevision"`
	}
	if err := common.Unmarshal(encoded, &input); err != nil || !centerModelIDPattern.MatchString(input.ModelID) || strings.Contains(input.ModelID, "..") ||
		!centerModelIDPattern.MatchString(input.UpstreamModelID) || strings.Contains(input.UpstreamModelID, "..") ||
		input.ChannelID <= 0 || len(input.Name) < 3 || len(input.Name) > 80 || strings.ContainsAny(input.Name, "\x00\r\n") ||
		input.Weight < 1 || input.Weight > 100 || input.Priority < 0 || input.Priority > 100 ||
		!centerRequestIDPattern.MatchString(input.RequestID) || len(input.ExpectedGatewayRevision) != 64 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid channel update"})
		return
	}
	before, status := centerModelOpsSnapshotData(input.ModelID)
	if status != http.StatusOK {
		c.JSON(status, gin.H{"success": false, "message": "model configuration unavailable"})
		return
	}
	sequence := before["sequence"].(uint64)
	revisionMatches := before["revision"] == input.ExpectedGatewayRevision
	hash := model.CenterChannelUpdatePayloadHash(input.ModelID, input.ChannelID, input.Name, input.UpstreamModelID, uint(input.Weight), input.Priority)
	replayed, err := model.UpdateCenterModelChannel(input.ModelID, input.ChannelID, input.Name, input.UpstreamModelID, input.RequestID, hash,
		uint(input.Weight), input.Priority, func(current uint64) bool { return revisionMatches && current == sequence })
	if err != nil {
		if errors.Is(err, model.ErrCenterModelRevision) || errors.Is(err, model.ErrCenterOperationConflict) || errors.Is(err, model.ErrCenterModelChannelInvalid) {
			c.JSON(http.StatusConflict, gin.H{"success": false, "message": "channel changed or shared; update refused"})
		} else {
			c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "channel update unavailable"})
		}
		return
	}
	after, status := centerModelOpsSnapshotData(input.ModelID)
	if status != http.StatusOK {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "channel updated; readback unavailable"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"modelId": input.ModelID, "revision": after["revision"], "channels": after["channels"], "replayed": replayed}})
}
