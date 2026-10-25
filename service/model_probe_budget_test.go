package service

import (
	"testing"

	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type probeFundingSpy struct{ settled int }

func (*probeFundingSpy) Source() string       { return BillingSourceWallet }
func (*probeFundingSpy) PreConsume(int) error { return nil }
func (f *probeFundingSpy) Settle(delta int) error {
	f.settled += delta
	return nil
}

func TestCenterModelProbeUsageBoundFailsClosed(t *testing.T) {
	info := &relaycommon.RelayInfo{ProbeQuotaLimit: 5000}
	valid := textQuotaSummary{Quota: 5000, PromptTokens: 512, CompletionTokens: 16}
	assert.True(t, centerModelProbeWithinBounds(info, valid, true))
	for _, changed := range []textQuotaSummary{
		{Quota: 5001, PromptTokens: 512, CompletionTokens: 16},
		{Quota: 5000, PromptTokens: 513, CompletionTokens: 16},
		{Quota: 5000, PromptTokens: 512, CompletionTokens: 17},
		{Quota: -1, PromptTokens: 512, CompletionTokens: 16},
	} {
		assert.False(t, centerModelProbeWithinBounds(info, changed, true))
	}
	info.TieredBillingSnapshot = &billingexpr.BillingSnapshot{BillingMode: "tiered_expr"}
	assert.False(t, centerModelProbeWithinBounds(info, valid, false))
}
func (*probeFundingSpy) Refund() error { return nil }

func TestCenterModelProbeSettlementRejectsOverBudgetBeforeDebit(t *testing.T) {
	funding := &probeFundingSpy{}
	session := &BillingSession{relayInfo: &relaycommon.RelayInfo{ProbeQuotaLimit: 5000, TokenUnlimited: true},
		funding: funding, preConsumedQuota: 100}
	require.ErrorContains(t, session.Settle(5001), "exceeds authorized budget")
	assert.Zero(t, funding.settled)
	require.NoError(t, session.Settle(5000))
	assert.Equal(t, 4900, funding.settled)
}
