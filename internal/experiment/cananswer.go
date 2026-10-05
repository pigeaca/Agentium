package experiment

import (
	"context"
	"encoding/json"
	"math"
	"slices"

	"github.com/pigeaca/agentium/internal/pricing"
	"github.com/pigeaca/agentium/internal/stats"
	"github.com/pigeaca/agentium/internal/store"
)

// This file answers, before a run, "can this size answer the question?": the smallest change the design can see, the
// change its contexts' size alone would make, and how many runs seeing that would take. Everything here is advice
// for the preview: nothing blocks a run.

// ContextSize is what the preview of a context experiment knows of its contexts' size: the first request's tokens of
// each arm's calibration (zero for an arm without a calibration that fits) and how many requests a run makes, from the
// model's earlier runs. Review.Size holds it; LoadReview fills it.
type ContextSize struct {
	FirstRequest [2]int64
	Requests     float64 // the median turns of the model's earlier fair task runs; zero with fewer than MinPastRuns
	PastRuns     int     // earlier fair task runs on the model that reported their turns
}

// loadContextSize reads the size facts of d from its calibrations that fit and the project's runs. An arm that needs a
// calibration (Readiness.Calibrations) has none that fits, so it counts as unknown; so does a calibration that cannot
// be read.
func (p Project) loadContextSize(ctx context.Context, d Design, needs []CalibrationNeed, runs []store.Run) ContextSize {
	var size ContextSize
	if d.Template != TemplateContextAB || len(d.Arms) != 2 {
		return size
	}
	for i, a := range d.Arms {
		if slices.ContainsFunc(needs, func(n CalibrationNeed) bool { return sameCalibration(d, n.Arm, a) }) {
			continue
		}
		_, cal, why, err := p.CalibrationState(ctx, d, a, "", "")
		if err != nil { // the preview is advice: an unreadable calibration is an unknown size, never a failure
			return ContextSize{}
		}
		if why == "" {
			size.FirstRequest[i] = cal.FirstRequest
		}
	}
	size.Requests, size.PastRuns = MedianRequests(runs, d.Model, d.ArmEffort(d.Arms[0]))
	return size
}

// MedianRequests is the median number of requests (Claude Code's turns) a task run on model made, over the runs the
// cost estimate learns from (Project.EstimateFor's selection): fair task runs on the model that saw a result and cost
// something, at effort; while fewer than MinPastRuns of those exist, the runs from before efforts were recorded
// join them. Runs at another effort never count. Zero (and the count) with fewer than MinPastRuns that reported turns.
func MedianRequests(runs []store.Run, model, effort string) (requests float64, n int) {
	type pick struct {
		turns    float64 // zero: the run reported none
		recorded bool
		match    bool
	}
	var picks []pick
	matching := 0
	for _, r := range runs {
		if r.Kind != "task" || !Fair(r.Outcome) {
			continue
		}
		var rec struct {
			Model          string `json:"model"`
			Effort         string `json:"effort"`
			EffortRecorded bool   `json:"effort_recorded"`
			Metrics        struct {
				SawResult bool `json:"saw_result"`
				Turns     int  `json:"turns"`
			} `json:"metrics"`
		}
		if json.Unmarshal(r.Record, &rec) != nil || rec.Model != model || !rec.Metrics.SawResult || storedSpend(r).AgentUSD <= 0 {
			continue
		}
		pk := pick{turns: float64(max(rec.Metrics.Turns, 0)), recorded: rec.EffortRecorded, match: rec.EffortRecorded && rec.Effort == effort}
		if !pk.recorded || pk.match {
			picks = append(picks, pk)
		}
		if pk.match {
			matching++
		}
	}
	var turns []float64
	for _, pk := range picks {
		if (pk.match || (!pk.recorded && matching < MinPastRuns)) && pk.turns >= 1 {
			turns = append(turns, pk.turns)
		}
	}
	if len(turns) < MinPastRuns {
		return 0, len(turns)
	}
	return median(turns), len(turns)
}

// ExpectedChange is the cost change a context's size alone is expected to make: the first request writes the
// difference in tokens to the prompt cache once, and every later request reads it.
type ExpectedChange struct {
	TokensA, TokensB int64   // each context's first request
	Requests         float64 // requests a run
	RunUSD           float64 // cost of a run
	// Share is the expected change of a run's cost as a fraction, positive when B costs less: 0.04 is 4% less.
	Share float64
}

// ExpectedContextChange computes the change expected from two contexts' sizes on model: the difference in tokens is
// written once to the cache (the model's one-hour cache-write price, as Claude Code writes it) and read by each of the
// requests-1 later requests (the cache-read price); that sum over runUSD is the share. False without a list price for
// the model, without requests (at least one) or without a positive run cost.
func ExpectedContextChange(model string, tokensA, tokensB int64, requests, runUSD float64) (ExpectedChange, bool) {
	rates, priced := pricing.Lookup(model)
	if !priced || requests < 1 || !(runUSD > 0) || tokensA <= 0 || tokensB <= 0 {
		return ExpectedChange{}, false
	}
	saved := float64(tokensA-tokensB) * (rates.CacheWrite1h + (requests-1)*rates.CacheRead) / 1e6
	return ExpectedChange{TokensA: tokensA, TokensB: tokensB, Requests: requests, RunUSD: runUSD, Share: saved / runUSD}, true
}

// SmallestCostChange is the smallest cost reduction design d can see, as a fraction (0.25 is 25%): the planner's
// figures (SigmaLogCost, TauLow: the low end of Detect's range) at d's tasks and repeats. A design that checks its
// answer early (seq-v1) is held to its last check's bound in place of the fixed 1.96, which is higher: it needs a
// larger change than a fixed design of the same size. False for a design without tasks.
func SmallestCostChange(d Design) (float64, bool) {
	tasks, repeats := len(d.Tasks), d.Repeats
	if tasks < 1 || repeats < 1 {
		return 0, false
	}
	if !d.Sequential() {
		return Detect(tasks, repeats).Cost[0], true
	}
	seq, err := NewSequential(tasks, !d.NoFutility)
	if err != nil {
		return 0, false
	}
	planned, err := seq.planned()
	if err != nil {
		return 0, false
	}
	return 1 - math.Exp(-mdeAtBound(planned[len(planned)-1].EffBound, TauLow*TauLow, SigmaLogCost*SigmaLogCost, repeats, tasks)), true
}

// mdeAtBound is stats.MDE with a different two-sided bound in place of 1.96: the same 80% power, so the power's term
// stays and only the bound changes. The two terms come from the normal quantiles, as MDE's constants do.
func mdeAtBound(bound, tau2, within float64, repeats, tasks int) float64 {
	zTwoSided, zPower := stats.NormalQuantile(0.975), stats.NormalQuantile(0.80)
	return stats.MDE(tau2, within, repeats, tasks) * (bound + zPower) / (zTwoSided + zPower)
}

// RunsToSeeCostChange is the runs of a fixed design with one run per version (both versions counted) that can see a
// cost change of share (a fraction, either sign) with 80% power at the planner's σ and τ, rounded to two significant
// digits: stats.MDE inverted, as it falls with the square root of the tasks. Zero for no change.
func RunsToSeeCostChange(share float64) int {
	effect := math.Abs(math.Log(1 - share))
	if !(effect > 0) || math.IsInf(effect, 0) {
		return 0
	}
	one := stats.MDE(TauLow*TauLow, SigmaLogCost*SigmaLogCost, 1, 1) // the log change one task of one run per version sees
	return roundTwoDigits(2 * one * one / (effect * effect))
}

// roundTwoDigits rounds v to a whole number with two significant digits at most: 688.4 is 690.
func roundTwoDigits(v float64) int {
	if v < 100 {
		return int(math.Round(v))
	}
	scale := math.Pow(10, math.Floor(math.Log10(v))-1)
	return int(math.Round(v/scale) * scale)
}

// CanAnswer is what a design's size can see, for the preview.
type CanAnswer struct {
	// Metric is what the experiment's goal asks about: MetricCost (--goal cheaper) or MetricSuccess (--goal better).
	Metric string
	// FloorMet is whether the design has the tasks and runs a verdict on Metric needs (FloorsFor); below it the metric
	// gets no verdict at any effect, and Smallest is nil.
	FloorMet                 bool
	FloorTasks, FloorRepeats int
	// Smallest is the smallest change the size can see, as a fraction (cost: a share, success: points as a fraction);
	// nil below the floor. For success, 1 or more means no change at this size can be seen.
	Smallest *float64
	// Expected is the change a context experiment's contexts' size alone is expected to make (cost experiments of
	// contexts only); nil when it is not known.
	Expected *ExpectedChange
	// NotSure is set when Expected is smaller than Smallest: the likely result is not sure. When Expected is larger,
	// no likely result is promised.
	NotSure bool
	// RunsToSee is the runs seeing Expected would take (RunsToSeeCostChange), never fewer than the cost floor's runs
	// (both versions); zero when there is no Expected.
	RunsToSee int
}

// CanAnswer works out what the review's design can see and, for a context experiment, what its contexts' size is
// expected to change.
func (r Review) CanAnswer() CanAnswer {
	d := r.Design
	metric := MetricCost
	if d.Goal == GoalBetter {
		metric = MetricSuccess
	}
	c := CanAnswer{Metric: metric}
	c.FloorTasks, c.FloorRepeats = FloorsFor(d.LockMethod()).Metric(metric)
	tasks := len(d.Tasks)
	if metric == MetricSuccess {
		tasks -= d.judgedTasks() // the judge-graded tasks never count toward passes (SuccessFloorWarning)
	}
	c.FloorMet = tasks >= c.FloorTasks && d.Repeats >= c.FloorRepeats
	if !c.FloorMet {
		return c
	}
	if metric == MetricSuccess {
		v := Detect(tasks, d.Repeats).Success[0]
		c.Smallest = &v
		return c
	}
	smallest, ok := SmallestCostChange(d)
	if !ok {
		return c
	}
	c.Smallest = &smallest
	if d.Template != TemplateContextAB || len(r.Estimates) < 2 {
		return c
	}
	size := r.Size
	runUSD, known := r.Estimates[0].MeanUSD(d.Tasks)
	if r.Estimates[0].EstimateBasis() != BasisHistory { // the cost of a run must rest on earlier runs, as the requests do
		return c
	}
	if size.FirstRequest[0] <= 0 || size.FirstRequest[1] <= 0 || size.PastRuns < MinPastRuns || !known {
		return c
	}
	if e, ok := ExpectedContextChange(d.Model, size.FirstRequest[0], size.FirstRequest[1], size.Requests, runUSD); ok {
		c.Expected = &e
		// Compared on the log scale, where a rise and a fall of the same ratio are equally easy to see: a rise of x is seen
		// from exp(mde)-1, not from the smallest reduction.
		c.NotSure = math.Abs(math.Log(1-e.Share)) < -math.Log(1-smallest)
		// A verdict on cost needs the floor's runs at any effect: seeing a large change never takes fewer.
		c.RunsToSee = max(RunsToSeeCostChange(e.Share), 2*c.FloorTasks*c.FloorRepeats)
	}
	return c
}
