package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// CenterModelOpsSnapshot is a credential-free readback of new-api's actual
// model configuration. It is intentionally not a write API or a publication
// readiness signal: Center must still verify the caller's group and token.
func CenterModelOpsSnapshot(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	modelID := strings.TrimSpace(c.Query("modelId"))
	if modelID == "" || len(modelID) > 128 || strings.ContainsAny(modelID, "\x00\r\n") {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid model ID"})
		return
	}
	data, status := centerModelOpsSnapshotData(modelID)
	if status != http.StatusOK {
		c.JSON(status, gin.H{"success": false, "message": "model configuration unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

func centerModelOpsSnapshotData(modelID string) (gin.H, int) {
	var modelRevision model.CenterModelRevision
	revisionErr := model.DB.Where("model_id = ?", modelID).Take(&modelRevision).Error
	if revisionErr != nil && !errors.Is(revisionErr, gorm.ErrRecordNotFound) {
		return nil, http.StatusServiceUnavailable
	}
	var metadata model.Model
	metaErr := model.DB.Where("model_name = ?", modelID).First(&metadata).Error
	if metaErr != nil && !errors.Is(metaErr, gorm.ErrRecordNotFound) {
		return nil, http.StatusServiceUnavailable
	}
	channels, err := model.GetAllChannels(0, 0, true, true)
	if err != nil {
		return nil, http.StatusServiceUnavailable
	}
	type channelView struct {
		ID                   int      `json:"id"`
		Name                 string   `json:"name"`
		Type                 int      `json:"type"`
		Status               int      `json:"status"`
		Groups               []string `json:"groups"`
		UpstreamModel        string   `json:"upstreamModel"`
		UpstreamHost         string   `json:"upstreamHost,omitempty"`
		CredentialConfigured bool     `json:"credentialConfigured"`
		Weight               int      `json:"weight"`
		Priority             int64    `json:"priority"`
	}
	views := make([]channelView, 0)
	matchedChannels := make([]*model.Channel, 0)
	groups := make(map[string]struct{})
	for _, channel := range channels {
		listed := false
		for _, candidate := range channel.GetModels() {
			if strings.TrimSpace(candidate) == modelID {
				listed = true
				break
			}
		}
		if !listed {
			continue
		}
		matchedChannels = append(matchedChannels, channel)
		for _, group := range channel.GetGroups() {
			groups[group] = struct{}{}
		}
		upstreamModel := modelID
		if mapping := channel.GetModelMapping(); mapping != "" {
			var entries map[string]string
			if err := common.Unmarshal([]byte(mapping), &entries); err == nil && entries[modelID] != "" {
				upstreamModel = entries[modelID]
			}
		}
		upstreamURL := ""
		if channel.BaseURL != nil {
			upstreamURL = *channel.BaseURL
		}
		if upstreamURL == "" && channel.Type >= 0 && channel.Type < len(constant.ChannelBaseURLs) {
			upstreamURL = constant.ChannelBaseURLs[channel.Type]
		}
		upstreamHost := ""
		if parsed, err := url.Parse(upstreamURL); err == nil {
			upstreamHost = parsed.Hostname()
		}
		views = append(views, channelView{ID: channel.Id, Name: channel.Name, Type: channel.Type,
			Status: channel.Status, Groups: channel.GetGroups(), UpstreamModel: upstreamModel,
			UpstreamHost: upstreamHost, CredentialConfigured: channel.Key != "",
			Weight: channel.GetWeight(), Priority: channel.GetPriority()})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })
	pricing := gin.H{"billingMode": billing_setting.GetBillingMode(modelID)}
	if expr, exists := billing_setting.GetBillingExpr(modelID); exists {
		pricing["billingExpr"] = expr
	}
	if price, exists := ratio_setting.GetModelPriceCopy()[modelID]; exists {
		pricing["modelPrice"] = price
	}
	if ratio, exists := ratio_setting.GetModelRatioCopy()[modelID]; exists {
		pricing["modelRatio"] = ratio
	}
	if ratio, exists := ratio_setting.GetCompletionRatioCopy()[modelID]; exists {
		pricing["completionRatio"] = ratio
	}
	for _, entry := range []struct {
		field  string
		values map[string]float64
	}{
		{"cacheRatio", ratio_setting.GetCacheRatioCopy()},
		{"createCacheRatio", ratio_setting.GetCreateCacheRatioCopy()},
		{"imageRatio", ratio_setting.GetImageRatioCopy()},
		{"audioRatio", ratio_setting.GetAudioRatioCopy()},
		{"audioCompletionRatio", ratio_setting.GetAudioCompletionRatioCopy()},
	} {
		if value, exists := entry.values[modelID]; exists {
			pricing[entry.field] = value
		}
	}
	groupRatios := make(map[string]float64)
	configuredGroupRatios := ratio_setting.GetGroupRatioCopy()
	for group := range groups {
		if value, exists := configuredGroupRatios[group]; exists {
			groupRatios[group] = value
		}
	}
	pricing["groupRatios"] = groupRatios
	metadataView := gin.H{"exists": metaErr == nil}
	if metaErr == nil {
		metadataView["status"] = metadata.Status
		metadataView["description"] = metadata.Description
		metadataView["endpoints"] = metadata.Endpoints
	}
	// Keep credential and route configuration in the digest, but exclude
	// runtime counters. A paid probe increments UsedQuota; that must not
	// invalidate its own configuration-bound receipt immediately after billing.
	// Neither the key nor its standalone hash is returned to the caller.
	revisionChannels := make([]*model.Channel, 0, len(matchedChannels))
	for _, channel := range matchedChannels {
		stable := *channel
		stable.UsedQuota = 0
		stable.TestTime = 0
		stable.ResponseTime = 0
		stable.Balance = 0
		stable.BalanceUpdatedTime = 0
		revisionChannels = append(revisionChannels, &stable)
	}
	revisionState, err := common.Marshal(struct {
		Metadata model.Model      `json:"metadata"`
		Channels []*model.Channel `json:"channels"`
		Pricing  gin.H            `json:"pricing"`
		Sequence uint64           `json:"sequence"`
	}{Metadata: metadata, Channels: revisionChannels, Pricing: pricing, Sequence: modelRevision.Sequence})
	if err != nil {
		return nil, http.StatusServiceUnavailable
	}
	revisionDigest := sha256.Sum256(revisionState)
	return gin.H{
		"modelId": modelID, "revision": hex.EncodeToString(revisionDigest[:]),
		"sequence": modelRevision.Sequence,
		"channels": views, "pricing": pricing, "metadata": metadataView,
	}, http.StatusOK
}
