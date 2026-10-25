package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCenterModelProbePriceBound(t *testing.T) {
	cheap := 0.005
	expensive := 0.02
	assert.True(t, centerModelProbePriceBound("new-test-model", "ratio", &cheap, 0, 0, ""))
	assert.False(t, centerModelProbePriceBound("new-test-model", "ratio", &expensive, 0, 0, ""))
	assert.True(t, centerModelProbePriceBound("deepseek-flash", "ratio", nil, 0.75, 1, ""))
	assert.False(t, centerModelProbePriceBound("deepseek-flash", "ratio", &cheap, 0.75, 1, ""))
	assert.False(t, centerModelProbePriceBound("deepseek-flash", "ratio", nil, 0.8, 1, ""))
	assert.False(t, centerModelProbePriceBound("deepseek-flash", "ratio", nil, 0.75, 2, ""))
	assert.False(t, centerModelProbePriceBound("deepseek-flash", "ratio", nil, 200, 1, ""))
	assert.False(t, centerModelProbePriceBound("unreviewed-model", "ratio", nil, 0.75, 1, ""))
	assert.True(t, centerModelProbePriceBound("deepseek-v4-pro", "tiered_expr", nil, 0, 0, centerV4ProProbeExpr))
	assert.False(t, centerModelProbePriceBound("deepseek-v4-pro", "tiered_expr", &cheap, 0, 0, centerV4ProProbeExpr))
	assert.False(t, centerModelProbePriceBound("deepseek-v4-pro", "tiered_expr", nil, 0, 0, `p * 999 + c * 999`))
}
