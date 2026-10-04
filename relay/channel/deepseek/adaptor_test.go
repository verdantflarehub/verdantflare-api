package deepseek

import (
	"testing"

	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/constant"
	"github.com/stretchr/testify/require"
)

func TestCurrentDeepSeekModelsUseChatCompletions(t *testing.T) {
	adaptor := &Adaptor{}
	for _, model := range []string{"deepseek-flash", "deepseek-v4-pro"} {
		t.Run(model, func(t *testing.T) {
			require.Contains(t, adaptor.GetModelList(), model)
			info := &relaycommon.RelayInfo{
				ChannelMeta: &relaycommon.ChannelMeta{ChannelBaseUrl: "https://api.deepseek.com"},
				RelayMode:   constant.RelayModeChatCompletions,
			}
			url, err := adaptor.GetRequestURL(info)
			require.NoError(t, err)
			require.Equal(t, "https://api.deepseek.com/v1/chat/completions", url)

			request := &dto.GeneralOpenAIRequest{Model: model}
			converted, err := adaptor.ConvertOpenAIRequest(nil, info, request)
			require.NoError(t, err)
			require.Same(t, request, converted)
			require.Equal(t, model, request.Model)
		})
	}

	require.NotContains(t, adaptor.GetModelList(), "deepseek-chat")
	require.NotContains(t, adaptor.GetModelList(), "deepseek-reasoner")
}
