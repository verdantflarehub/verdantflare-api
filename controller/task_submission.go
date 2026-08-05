package controller

import (
	"net/http"

	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// GetVideoSubmission exposes only the owner-safe submission recovery view.
// A missing, malformed, or foreign request ID is deliberately indistinguishable.
func GetVideoSubmission(c *gin.Context) {
	view, err := service.GetTaskSubmissionUserView(c.GetInt("id"), c.Param("client_request_id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    "submission_lookup_failed",
			"message": "unable to inspect the video submission",
		})
		return
	}
	if view == nil {
		c.JSON(http.StatusNotFound, gin.H{
			"code":    "submission_not_found",
			"message": "video submission not found",
		})
		return
	}
	c.JSON(http.StatusOK, view)
}
