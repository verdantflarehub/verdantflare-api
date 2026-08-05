package common

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/require"
)

func TestWxmaasSeedanceUsesOpenAIVideoEndpoint(t *testing.T) {
	require.Equal(t,
		[]constant.EndpointType{constant.EndpointTypeOpenAIVideo},
		GetEndpointTypesByChannelType(constant.ChannelTypeWxmaasSeedance, "verdantflare-sd2"),
	)
}
