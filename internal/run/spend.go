package run

import "encoding/json"

// Spend is what a run spent, by who spent it: the one place a run's costs are read from.
//   - Every total of what runs spent (an experiment's budget and reserve, its status line and progress lines, show, the
//     report's spend) is TotalUSD, pair comparisons included.
//   - Every measure of the agent's cost (the cost metric and its analysis, the arms' and tasks' costs, estimates from
//     past runs) is AgentUSD alone: the judges' calls are spent, but they are not the agent's cost.
//
// A new cost source belongs here, in Record.Spend and StoredSpend, and in TotalUSD; TestSpendCoversEveryCost fails
// until it is.
type Spend struct {
	// AgentUSD is Claude Code's run: Metrics.CostUSD, which the runs table keeps as cost_usd. A run stopped before
	// Claude Code reported its cost has its transcript's estimate here (Metrics.EstimatedCostUSD, AgentEstimated).
	AgentUSD float64
	// JudgeUSD is what the judge's calls spent on the run (Judge.CostUSD, earlier stopped judgements included); zero
	// without a verdict.
	JudgeUSD float64
	// PairJudgeUSD is what the pair judge spent comparing the run with its pair's arm-A run (PairJudge, earlier stopped
	// comparisons included): only an arm-B run holds one, so a pair counts once.
	PairJudgeUSD float64
	// AgentEstimated: Claude Code reported no cost, so AgentUSD prices the transcript's requests at list prices
	// (Record.CostEstimated).
	AgentEstimated bool
}

// TotalUSD is everything the run spent: the agent's run, its judgement and its pair's comparison.
func (s Spend) TotalUSD() float64 { return s.AgentUSD + s.JudgeUSD + s.PairJudgeUSD }

// Spend is what the run spent.
func (r Record) Spend() Spend {
	s := Spend{AgentUSD: r.Metrics.CostUSD, AgentEstimated: r.CostEstimated}
	if r.Judge != nil {
		s.JudgeUSD = r.Judge.CostUSD
	}
	if r.PairJudge != nil {
		s.PairJudgeUSD = r.PairJudge.Verdict.CostUSD
	}
	return s
}

// JudgeCostUSD is Spend().JudgeUSD: what the judge spent on the run, none without a verdict. It is kept only for the
// existing tests (report/judge_test.go, run/judge_test.go); drop it when step 2 of the refactor round moves them.
func (r Record) JudgeCostUSD() float64 { return r.Spend().JudgeUSD }

// StoredSpend is a stored run's spend: agentUSD is its cost column (the record's Metrics.CostUSD when it was saved),
// and the rest comes from its encoded record. It reads only the fields it needs, so a record this Agentium cannot
// otherwise decode still counts its agent's cost; an unreadable record adds nothing to it.
func StoredSpend(agentUSD float64, record []byte) Spend {
	var r struct {
		CostEstimated bool `json:"cost_estimated"`
		Judge         *struct {
			CostUSD float64 `json:"cost_usd"`
		} `json:"judge"`
		PairJudge *struct {
			Verdict struct {
				CostUSD float64 `json:"cost_usd"`
			} `json:"verdict"`
		} `json:"pair_judge"`
	}
	s := Spend{AgentUSD: agentUSD}
	if json.Unmarshal(record, &r) != nil {
		return s
	}
	s.AgentEstimated = r.CostEstimated
	if r.Judge != nil {
		s.JudgeUSD = r.Judge.CostUSD
	}
	if r.PairJudge != nil {
		s.PairJudgeUSD = r.PairJudge.Verdict.CostUSD
	}
	return s
}
