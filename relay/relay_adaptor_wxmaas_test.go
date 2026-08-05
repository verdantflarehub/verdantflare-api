package relay

import (
	"strconv"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel/task/wxmaasseedance"
	"github.com/stretchr/testify/require"
)

func TestGetTaskAdaptorRegistersWxmaasSeedance(t *testing.T) {
	adaptor := GetTaskAdaptor(constant.TaskPlatform(strconv.Itoa(constant.ChannelTypeWxmaasSeedance)))
	require.IsType(t, &wxmaasseedance.TaskAdaptor{}, adaptor)
	require.Equal(t, []string{wxmaasseedance.PublicModel}, adaptor.GetModelList())
}
