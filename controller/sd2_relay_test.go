package controller

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/require"
)

func TestSD2ChannelEligibilityRejectsSeedUnsupportedByJD(t *testing.T) {
	request := &service.SD2NormalizedCreate{Duration: 10, Seed: 123}
	taskErr := sd2ChannelEligibility(1, constant.ChannelTypeJDSeedance, false, request)
	require.NotNil(t, taskErr)
	require.Equal(t, "provider_capability_mismatch", taskErr.Code)

	request.Seed = service.SD2DefaultSeed
	require.Nil(t, sd2ChannelEligibility(1, constant.ChannelTypeJDSeedance, false, request))
}

func TestIsSD2RemixRequestDoesNotDependOnLedgerRollout(t *testing.T) {
	require.True(t, isSD2RemixRequest("/v1/videos/source/remix", constant.TaskActionGenerate))
	require.True(t, isSD2RemixRequest("/v1/videos", constant.TaskActionRemix))
	require.False(t, isSD2RemixRequest("/v1/videos", constant.TaskActionGenerate))
}
