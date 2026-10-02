package claude

import "github.com/pigeaca/agentium/internal/pricing"

// A cap is soft: Claude Code checks --max-budget-usd after each turn, so a run stops only once a turn has crossed it,
// and spends its cap plus that turn. In the seq-v1 smoke check (2026-10-02, claude-sonnet-5, $0.50 caps) a run ended
// at $0.507; across its seven transcripts the dearest turn cost $0.105 (the first, which writes the context to the
// cache, so it cannot cross a cap above it) and no later turn more than $0.058. A turn's cost does not grow with the
// cap but with the context, the output and the model's prices, so the allowance is a share of the cap with a floor:
// CapOvershootMinUSD, about 2.5 times the dearest later turn measured (and a subagent's turn in flight beside the
// session's), scaled by the model's output price against claude-sonnet-5's (output dominates a later turn's cost, and
// grows with effort): $0.30 on claude-opus-5-5, $0.375 on claude-opus-5. A model without a list price is assumed to
// cost the dearest output rate in the table. CapOvershootShare covers the larger contexts of runs allowed to go
// further ($0.30 at a $3 cap). It is an estimate, as the cap is: a single turn dearer than the allowance would still
// pass it, which Overshoot records.
const (
	CapOvershootShare  = 0.10
	CapOvershootMinUSD = 0.15 // on a model whose output costs capOvershootRefOutput
	// capOvershootRefOutput is claude-sonnet-5's output price (USD per million tokens), the measurement's.
	capOvershootRefOutput = 10.0
)

// CapOvershootFloorUSD is the allowance's floor on model: CapOvershootMinUSD scaled by its output price.
func CapOvershootFloorUSD(model string) float64 {
	output := pricing.MaxOutput()
	if rates, ok := pricing.Lookup(model); ok {
		output = rates.Output
	}
	return CapOvershootMinUSD * output / capOvershootRefOutput
}

// CapOvershootUSD is how far past capUSD a run of model capped there may spend: what every reserve, budget check and
// worst case holds back beside the cap.
func CapOvershootUSD(capUSD float64, model string) float64 {
	if capUSD <= 0 {
		return 0
	}
	return max(CapOvershootShare*capUSD, CapOvershootFloorUSD(model))
}

// Overshoot is how far a run went past its cost cap (stopped there, or finished on the turn that crossed it), against
// the allowance the budget held.
type Overshoot struct {
	CapUSD       float64 `json:"cap_usd"`
	OverUSD      float64 `json:"over_usd"` // the reported cost less the cap; negative when it stopped below it
	AllowanceUSD float64 `json:"allowance_usd"`
}

// Exceeded reports whether the run passed its cap by more than the allowance: spending may then pass a budget.
func (o Overshoot) Exceeded() bool { return o.OverUSD > o.AllowanceUSD+1e-9 }

// CapOvershoot is the Overshoot of a run that Claude Code stopped at its cost cap, or that went past the cap whatever
// its result (one that finished on the turn that crossed it), with m its metrics and costUSD its reported cost; nil for
// any other run.
func CapOvershoot(m Metrics, costUSD, capUSD float64, model string) *Overshoot {
	if capUSD <= 0 || m.Result != "error_max_budget_usd" && costUSD <= capUSD {
		return nil
	}
	return &Overshoot{CapUSD: capUSD, OverUSD: costUSD - capUSD, AllowanceUSD: CapOvershootUSD(capUSD, model)}
}
