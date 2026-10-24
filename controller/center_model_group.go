package controller

import (
	"errors"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

// CenterModelOpsGroupPreview is deliberately scoped to the dedicated internal
// test group. It never changes or silently exposes customer group pricing.
func CenterModelOpsGroupPreview(c *gin.Context) {
	revision, exists, err := model.CenterInternalGroupSnapshot()
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "group configuration unavailable"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"group": "center-internal", "ratio": 1, "exists": exists,
		"expectedGroupRevision": revision, "impact": "dedicated internal test group only",
	}})
}

func CenterModelOpsGroupApply(c *gin.Context) {
	var raw map[string]any
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 4<<10), &raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid group request"})
		return
	}
	for key := range raw {
		if key != "requestId" && key != "expectedGroupRevision" && key != "group" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "unsupported group field"})
			return
		}
	}
	requestID, _ := raw["requestId"].(string)
	expected, _ := raw["expectedGroupRevision"].(string)
	group, _ := raw["group"].(string)
	if group != "center-internal" || !centerRequestIDPattern.MatchString(requestID) || len(expected) != 64 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid group request"})
		return
	}
	replayed, err := model.CreateCenterInternalGroup(expected, requestID)
	if err != nil {
		if errors.Is(err, model.ErrCenterModelRevision) || errors.Is(err, model.ErrCenterOperationConflict) || errors.Is(err, model.ErrCenterModelChannelInvalid) {
			c.JSON(http.StatusConflict, gin.H{"success": false, "message": "group configuration changed or exists"})
		} else {
			c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "group write unavailable"})
		}
		return
	}
	revision, exists, err := model.CenterInternalGroupSnapshot()
	if err != nil || !exists {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "group saved; readback unavailable"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"group": "center-internal", "ratio": 1, "revision": revision, "replayed": replayed,
	}})
}
