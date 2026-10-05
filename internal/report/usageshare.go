package report

import (
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/experiment"
)

// PlanShare is the share of a subscription's five-hour usage window the experiment's runs used, estimated as the
// preview does (experiment.UsagePerRun over the runs' usage readings: the window's rise across the runs' stretches,
// divided by the runs) times the experiment's runs. All the runs are measured together, whatever their model: the
// arms of a model A/B run side by side, so the window's rise is theirs together, and measuring each model alone would
// count it once per model. ok is false for an API-key sign-in, which reports no usage, and when the runs' readings do
// not give a rise (fewer than experiment.MinUsageRuns in one window): the default rate is a guess, not something the
// experiment used.
func PlanShare(rep Report) (share float64, ok bool) {
	if rep.Lock.SignIn == "" || rep.Lock.SignIn == claude.SignInAPIKey {
		return 0, false
	}
	var samples []experiment.UsageSample
	for _, r := range rep.Runs {
		first, last := r.Metrics.UsageFirst, r.Metrics.UsageLast
		if first == nil || last == nil {
			continue
		}
		s := experiment.UsageSample{First: *first, Last: *last}
		s.Started, _ = time.Parse(time.RFC3339, r.Started)
		s.Finished, _ = time.Parse(time.RFC3339, r.Finished)
		samples = append(samples, s)
	}
	rate := experiment.UsagePerRun(samples, "")
	if rate.Runs == 0 {
		return 0, false
	}
	return rate.PerRun * float64(len(rep.Runs)), true
}
