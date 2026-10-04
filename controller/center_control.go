package controller

import (
	"errors"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

var centerOrganizationIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,79}$`)
var centerRequestIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{16,80}$`)

func centerOrganizationID(c *gin.Context) (string, bool) {
	id := c.Param("organizationID")
	if !centerOrganizationIDPattern.MatchString(id) {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid organization ID"})
		return "", false
	}
	return id, true
}

func centerError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "organization or key not found"})
	case errors.Is(err, model.ErrCenterOperationConflict), errors.Is(err, model.ErrCenterQuotaOverflow):
		c.JSON(http.StatusConflict, gin.H{"success": false, "message": err.Error()})
	case errors.Is(err, model.ErrCenterAccountDisabled):
		c.JSON(http.StatusForbidden, gin.H{"success": false, "message": err.Error()})
	default:
		common.SysError("Center gateway operation failed: " + err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "gateway operation failed"})
	}
}

func centerBalance(user model.User) gin.H {
	return gin.H{
		"remainingQuota": user.Quota,
		"usedQuota":      user.UsedQuota,
		"enabled":        user.Status == common.UserStatusEnabled,
	}
}

func CenterOrganizationBalance(c *gin.Context) {
	id, ok := centerOrganizationID(c)
	if !ok {
		return
	}
	user, err := model.GetCenterUser(id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"remainingQuota": 0, "usedQuota": 0, "enabled": true}})
		return
	}
	if err != nil {
		centerError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": centerBalance(user)})
}

func CenterGrantCredit(c *gin.Context) {
	id, ok := centerOrganizationID(c)
	if !ok {
		return
	}
	var input struct {
		RequestID   string `json:"requestId"`
		AmountCents int    `json:"amountCents"`
		Actor       string `json:"actor"`
	}
	if err := common.DecodeJson(io.LimitReader(c.Request.Body, 16<<10), &input); err != nil ||
		!centerRequestIDPattern.MatchString(input.RequestID) || input.AmountCents < 1 || input.AmountCents > 100000 ||
		strings.TrimSpace(input.Actor) == "" || len(input.Actor) > 160 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid credit grant"})
		return
	}
	user, err := model.GrantCenterCredit(id, input.RequestID, input.Actor, input.AmountCents)
	if err != nil {
		centerError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": centerBalance(user)})
}

func CenterSetOrganizationStatus(c *gin.Context) {
	id, ok := centerOrganizationID(c)
	if !ok {
		return
	}
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	if err := common.DecodeJson(io.LimitReader(c.Request.Body, 4<<10), &input); err != nil || input.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "enabled is required"})
		return
	}
	if err := model.SetCenterAccountEnabled(id, *input.Enabled); err != nil {
		centerError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func centerAvailableModels() map[string]bool {
	available := make(map[string]bool)
	for _, id := range model.GetGroupEnabledModels("default") {
		if helper.HasModelBillingConfig(id) {
			available[id] = true
		}
	}
	return available
}

func CenterCreateKey(c *gin.Context) {
	id, ok := centerOrganizationID(c)
	if !ok {
		return
	}
	var input struct {
		RequestID     string   `json:"requestId"`
		Name          string   `json:"name"`
		Models        []string `json:"models"`
		ExpiresInDays int      `json:"expiresInDays"`
	}
	if err := common.DecodeJson(io.LimitReader(c.Request.Body, 16<<10), &input); err != nil ||
		!centerRequestIDPattern.MatchString(input.RequestID) || len(strings.TrimSpace(input.Name)) < 2 ||
		len(input.Name) > 50 || len(input.Models) < 1 || len(input.Models) > 20 ||
		input.ExpiresInDays < 1 || input.ExpiresInDays > 365 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid key request"})
		return
	}
	available := centerAvailableModels()
	seen := make(map[string]bool)
	for _, modelID := range input.Models {
		if !available[modelID] || seen[modelID] {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "model is unavailable or duplicated"})
			return
		}
		seen[modelID] = true
	}
	sort.Strings(input.Models)
	user, err := model.GetCenterUser(id)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		centerError(c, err)
		return
	}
	if err == nil && user.Status != common.UserStatusEnabled {
		c.JSON(http.StatusForbidden, gin.H{"success": false, "message": "organization gateway account is disabled"})
		return
	}
	token, err := model.CreateCenterToken(id, input.RequestID, strings.TrimSpace(input.Name), input.Models, input.ExpiresInDays)
	if err != nil {
		centerError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": gin.H{
		"id": token.Id, "name": token.Name, "secret": "sk-" + token.Key,
		"models": input.Models, "createdAt": token.CreatedTime, "expiresAt": token.ExpiredTime,
	}})
}

func centerTokenStatus(token model.Token) string {
	if token.Status != common.TokenStatusEnabled {
		return "revoked"
	}
	if token.ExpiredTime != -1 && token.ExpiredTime < time.Now().Unix() {
		return "expired"
	}
	return "active"
}

func CenterListKeys(c *gin.Context) {
	id, ok := centerOrganizationID(c)
	if !ok {
		return
	}
	tokens, err := model.ListCenterTokens(id)
	if err != nil {
		centerError(c, err)
		return
	}
	items := make([]gin.H, 0, len(tokens))
	for _, token := range tokens {
		items = append(items, gin.H{
			"id": token.Id, "name": token.Name, "prefix": token.GetMaskedKey(),
			"models": token.GetModelLimits(), "createdAt": token.CreatedTime,
			"expiresAt": token.ExpiredTime, "lastUsedAt": token.AccessedTime,
			"status": centerTokenStatus(token),
		})
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": items})
}

func centerTokenID(c *gin.Context) (int, bool) {
	id, err := strconv.Atoi(c.Param("tokenID"))
	if err != nil || id < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid token ID"})
		return 0, false
	}
	return id, true
}

func CenterRevokeKey(c *gin.Context) {
	organizationID, ok := centerOrganizationID(c)
	if !ok {
		return
	}
	tokenID, ok := centerTokenID(c)
	if !ok {
		return
	}
	token, err := model.GetCenterToken(organizationID, tokenID)
	if err != nil {
		centerError(c, err)
		return
	}
	if token.Status != common.TokenStatusDisabled {
		token.Status = common.TokenStatusDisabled
		if err := token.Update(); err != nil {
			centerError(c, err)
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func CenterProbeKey(c *gin.Context) {
	organizationID, ok := centerOrganizationID(c)
	if !ok {
		return
	}
	tokenID, ok := centerTokenID(c)
	if !ok {
		return
	}
	token, err := model.GetCenterToken(organizationID, tokenID)
	if err != nil {
		centerError(c, err)
		return
	}
	user, err := model.GetCenterUser(organizationID)
	if err != nil {
		centerError(c, err)
		return
	}
	available := centerAvailableModels()
	models := make([]string, 0)
	for _, id := range token.GetModelLimits() {
		if available[id] {
			models = append(models, id)
		}
	}
	status := centerTokenStatus(token)
	reason := "ok"
	switch {
	case status != "active":
		reason = status
	case user.Status != common.UserStatusEnabled:
		reason = "organization_disabled"
	case user.Quota <= 0:
		reason = "insufficient_quota"
	case len(models) == 0:
		reason = "no_available_models"
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"ok": reason == "ok", "reason": reason, "models": models,
		"remainingQuota": user.Quota, "readOnly": true,
	}})
}
