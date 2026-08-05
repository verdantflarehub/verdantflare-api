package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/task/wxmaasseedance"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func wxmaasSafetySetting() *string {
	setting := `{"create_enabled":false,"poll_enabled":true,"max_concurrency":1,"max_task_cost_microunits_cny":1000000,"hard_daily_budget_microunits_cny":10000000}`
	return &setting
}

func TestValidateWxmaasChannelAppliesSafeDefaults(t *testing.T) {
	channel := &model.Channel{
		Type:    constant.ChannelTypeWxmaasSeedance,
		Key:     "  provider-key  ",
		Setting: wxmaasSafetySetting(),
	}

	err := validateChannel(channel, true)
	require.NoError(t, err)
	assert.Equal(t, "provider-key", channel.Key)
	assert.Equal(t, wxmaasseedance.DefaultBaseURL, channel.GetBaseURL())
	assert.Equal(t, wxmaasseedance.PublicModel, channel.Models)
	assert.Equal(t, wxmaasseedance.DefaultModelMapping, channel.GetModelMapping())
}

func TestValidateWxmaasChannelRequiresExplicitSafetyConfiguration(t *testing.T) {
	channel := &model.Channel{Type: constant.ChannelTypeWxmaasSeedance, Key: "provider-key"}
	require.ErrorContains(t, validateChannel(channel, true), "safety configuration is invalid")

	invalid := `{"create_enabled":true,"poll_enabled":true,"max_concurrency":2,"max_task_cost_microunits_cny":1000,"hard_daily_budget_microunits_cny":10000}`
	channel.Setting = &invalid
	require.ErrorContains(t, validateChannel(channel, true), "max_concurrency must equal 1")

	invalid = `{"create_enabled":true,"poll_enabled":true,"max_concurrency":1,"max_task_cost_microunits_cny":1000,"hard_daily_budget_microunits_cny":999}`
	channel.Setting = &invalid
	require.ErrorContains(t, validateChannel(channel, true), "at least the single-task limit")
}

func TestValidateWxmaasChannelRejectsUnsafeConfiguration(t *testing.T) {
	wrongBaseURL := "https://proxy.example.com"
	wrongMapping := `{"verdantflare-sd2":"other-model"}`
	tests := []struct {
		name    string
		channel *model.Channel
		want    string
	}{
		{
			name: "multi-key flag",
			channel: &model.Channel{
				Type:        constant.ChannelTypeWxmaasSeedance,
				Key:         "provider-key",
				ChannelInfo: model.ChannelInfo{IsMultiKey: true},
			},
			want: "does not support multi-key",
		},
		{
			name: "newline-separated keys",
			channel: &model.Channel{
				Type: constant.ChannelTypeWxmaasSeedance,
				Key:  "provider-key-1\nprovider-key-2",
			},
			want: "exactly one channel key",
		},
		{
			name: "base URL",
			channel: &model.Channel{
				Type:    constant.ChannelTypeWxmaasSeedance,
				Key:     "provider-key",
				BaseURL: &wrongBaseURL,
			},
			want: "base URL must be",
		},
		{
			name: "public models",
			channel: &model.Channel{
				Type:   constant.ChannelTypeWxmaasSeedance,
				Key:    "provider-key",
				Models: wxmaasseedance.UpstreamModel,
			},
			want: "models must be verdantflare-sd2",
		},
		{
			name: "model mapping",
			channel: &model.Channel{
				Type:         constant.ChannelTypeWxmaasSeedance,
				Key:          "provider-key",
				Models:       wxmaasseedance.PublicModel,
				ModelMapping: &wrongMapping,
			},
			want: "model mapping must map",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateChannel(test.channel, true)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestValidateWxmaasChannelUpdateUsesPersistedType(t *testing.T) {
	wrongBaseURL := "https://proxy.example.com"
	patch := &model.Channel{BaseURL: &wrongBaseURL}
	origin := &model.Channel{Type: constant.ChannelTypeWxmaasSeedance, Setting: wxmaasSafetySetting()}

	err := normalizeAndValidateWxmaasChannelUpdate(patch, origin)
	require.ErrorContains(t, err, "base URL must be")

	patch = &model.Channel{}
	require.NoError(t, normalizeAndValidateWxmaasChannelUpdate(patch, origin))
	assert.Equal(t, constant.ChannelTypeWxmaasSeedance, patch.Type)
	assert.Equal(t, wxmaasseedance.DefaultBaseURL, patch.GetBaseURL())
	assert.Equal(t, wxmaasseedance.PublicModel, patch.Models)
	assert.Equal(t, wxmaasseedance.DefaultModelMapping, patch.GetModelMapping())
}

func TestAddWxmaasChannelRejectsNonSingleModeBeforeDatabaseWrites(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel", strings.NewReader(`{
		"mode":"multi_to_single",
		"channel":{"type":60,"key":"provider-key","name":"wxmaas","setting":"{\"create_enabled\":false,\"poll_enabled\":true,\"max_concurrency\":1,\"max_task_cost_microunits_cny\":1000000,\"hard_daily_budget_microunits_cny\":10000000}"}
	}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	AddChannel(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `"success":false`)
	assert.Contains(t, recorder.Body.String(), "single-key mode")
}

func TestWxmaasChannelTestNeverCallsCreateEndpoint(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer server.Close()

	result := testChannel(nil, &model.Channel{
		Type:    constant.ChannelTypeWxmaasSeedance,
		Key:     "provider-key",
		BaseURL: &server.URL,
		Models:  wxmaasseedance.PublicModel,
	}, 1, "", "", false)

	require.ErrorContains(t, result.localErr, "channel configuration is invalid")
	assert.False(t, called)

	result = testChannel(nil, &model.Channel{
		Type:    constant.ChannelTypeWxmaasSeedance,
		Key:     "provider-key",
		Models:  wxmaasseedance.PublicModel,
		Setting: wxmaasSafetySetting(),
	}, 1, "", "", false)
	require.ErrorContains(t, result.localErr, "channel test is not supported")

	result = testChannel(nil, &model.Channel{
		Type:    constant.ChannelTypeWxmaasSeedance,
		Models:  wxmaasseedance.PublicModel,
		Setting: wxmaasSafetySetting(),
	}, 1, "", "", false)
	require.ErrorContains(t, result.localErr, "channel key is required")
	assert.False(t, called)
}
