package cli

import (
	"math"
	"testing"

	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
)

// The scheduler's result and a stored run's spend both come from run.Spend: the budget counts the total, the cost
// column and the progress line's first figure are the agent's alone.
func TestSpendReachesTheBudget(t *testing.T) {
	t.Parallel()
	r := spentResult(run.Spend{AgentUSD: 0.3, JudgeUSD: 0.1})
	if math.Abs(r.CostUSD-0.4) > 1e-12 || r.JudgeUSD != 0.1 || math.Abs(r.AgentUSD()-0.3) > 1e-12 {
		t.Errorf("result spends $%v (judge $%v, agent $%v), want $0.40, $0.10 and $0.30", r.CostUSD, r.JudgeUSD, r.AgentUSD())
	}
	s := storedSpend(store.Run{CostUSD: 0.3, Record: []byte(`{"cost_estimated":true,"judge":{"cost_usd":0.1}}`)})
	if s != (run.Spend{AgentUSD: 0.3, JudgeUSD: 0.1, AgentEstimated: true}) {
		t.Errorf("stored spend = %+v", s)
	}
}
