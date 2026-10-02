package experiment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/pricing"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
	"github.com/pigeaca/agentium/internal/term"
)

// A calibration is one short real run that checks an arm's environment (see run.Calibrator). `run calibrate` makes
// them on demand; an experiment makes the ones it lacks before its first pair. The limits below are `run calibrate`'s.
const (
	CalibrationBudgetUSD = 0.5 // a calibration run stops at this cost; the budget holds it for each, with its overshoot
	CalibrationTimeout   = 5 * time.Minute
	// fallbackCalibrationUSD is a calibration's estimate for a model without a list price.
	fallbackCalibrationUSD = 0.15
)

// calibrationProfile is one calibration run's tokens: Claude Code's first request, a few short turns and the 40000-line
// output read back. Estimated, then compared with the one real measurement: nine calibrations on Sonnet cost $0.70 in
// total, $0.08 each, on Claude Code 2.1.281.
func calibrationProfile() pricing.Usage {
	return pricing.Usage{CacheWrite1h: 15_000, CacheRead: 30_000, Input: 3_000, Output: 2_000}
}

// CalibrationEstimateUSD is the expected cost of one calibration run on model, never above its cap.
func CalibrationEstimateUSD(model string) float64 {
	rates, priced := pricing.Lookup(model)
	if !priced {
		return fallbackCalibrationUSD
	}
	return min(rates.Cost(calibrationProfile()), CalibrationBudgetUSD)
}

// CalibrationNeed is a calibration an experiment must make before its first pair: one per distinct context and model,
// whichever arm needs it first.
type CalibrationNeed struct {
	Arm   Arm
	Model string
	Why   string // what is missing or stale, in words
}

// CalibrationState reads arm a's newest calibration on its model (Project.CalibrationFor) and says whether it can be
// what the arm's runs are checked against: why is empty when it can, and else says in words what is missing or stale.
// version and signIn ("" skips the check) are what runs would see now. The stored calibration is returned when there is
// one, healthy or not for these runs.
func (p Project) CalibrationState(ctx context.Context, d Design, a Arm, version, signIn string) (stored store.Calibration, cal run.Calibration, why string, err error) {
	stored, err = p.CalibrationFor(ctx, d, a)
	if errors.Is(err, store.ErrNotFound) {
		return stored, cal, "is not calibrated", nil
	}
	if err != nil {
		return stored, cal, "", err
	}
	if err := json.Unmarshal(stored.Result, &cal); err != nil {
		return stored, cal, "", fmt.Errorf("calibration of %s: %w", a.Context, err)
	}
	if cal.SignIn == "" { // saved before calibrations recorded it: the calibration run's record has it
		if r, err := p.DB.RunByID(ctx, p.ID, stored.RunID); err == nil {
			var rec struct {
				SignIn string `json:"sign_in"`
			}
			if json.Unmarshal(r.Record, &rec) == nil {
				cal.SignIn = rec.SignIn
			}
		}
	}
	switch {
	case version != "" && cal.CLIVersion != version:
		why = fmt.Sprintf("was calibrated on Claude Code %s, not %s", cal.CLIVersion, version)
	case signIn != "" && cal.SignIn != signIn:
		why = fmt.Sprintf("was calibrated with sign-in %s, and runs would now use %s", term.OrNone(cal.SignIn), signIn)
	}
	return stored, cal, why, nil
}

// CalibrationNeeds lists what experiment d must calibrate when it runs: each distinct context and model without a
// calibration that fits (CalibrationState), in arm order. A model-ab experiment has one per model, a context A/B one
// per context and an A/A one.
func (p Project) CalibrationNeeds(ctx context.Context, d Design, version, signIn string) ([]CalibrationNeed, error) {
	var needs []CalibrationNeed
	var seen []Arm
	for _, a := range d.Arms {
		if slices.ContainsFunc(seen, func(s Arm) bool { return sameCalibration(d, s, a) }) {
			continue
		}
		seen = append(seen, a)
		_, _, why, err := p.CalibrationState(ctx, d, a, version, signIn)
		if err != nil {
			return nil, err
		}
		if why != "" {
			needs = append(needs, CalibrationNeed{Arm: a, Model: d.ArmModel(a), Why: why})
		}
	}
	return needs, nil
}

// sameCalibration reports whether arms a and b are covered by one calibration: the same context on the same model.
func sameCalibration(d Design, a, b Arm) bool {
	return a.Context == b.Context && a.Snapshot == b.Snapshot && d.ArmModel(a) == d.ArmModel(b)
}

// CalibrationCosts is the expected cost of needs, and the most they may spend: a cap for each, with the turn that may
// cross it (CapOvershootUSD).
func CalibrationCosts(needs []CalibrationNeed) (estimate, capUSD float64) {
	for _, n := range needs {
		estimate += CalibrationEstimateUSD(n.Model)
		capUSD += CalibrationBudgetUSD + CapOvershootUSD(CalibrationBudgetUSD)
	}
	return estimate, capUSD
}

// CalibrationSpend is what the experiment's calibration runs spent (the judge has no part in them).
func (p Project) CalibrationSpend(ctx context.Context, experimentID int64) (float64, error) {
	runs, err := p.DB.ExperimentCalibrationRuns(ctx, experimentID)
	if err != nil {
		return 0, err
	}
	spent := 0.0
	for _, r := range runs {
		spent += storedSpend(r).TotalUSD()
	}
	return spent, nil
}

// calibrate makes the calibrations the experiment lacks, before it is locked, so that its runs are checked against an
// environment measured on this Claude Code, sign-in and model. Calibrations that are in place are not repeated, and a
// locked experiment never gets here. The runs are stored with the experiment (as calibration runs, not slots), so
// they count against its budget and its progress. A calibration that fails its checks stops the experiment with the
// reasons `run calibrate` prints; the healthy ones found with it are kept.
func (r Runner) calibrate(ctx context.Context, stored store.Experiment, d Design, version string) error {
	p, out, st := r.Project, r.Out, r.Style
	needs, err := p.CalibrationNeeds(ctx, d, version, r.SignIn)
	if err != nil || len(needs) == 0 {
		return err
	}
	// The calibrations count against the budget, and the experiment must still fit a pair after them: refuse before
	// spending, with nothing locked.
	prior, err := p.CalibrationSpend(ctx, stored.ID)
	if err != nil {
		return err
	}
	_, capUSD := CalibrationCosts(needs)
	if need := prior + capUSD + d.PairCapUSD(); need > d.BudgetUSD+1e-9 {
		return UsageError(fmt.Sprintf("the budget $%.2f cannot hold the calibrations this experiment needs (up to $%.2f, with $%.2f already spent on earlier ones) and one pair of runs at their caps ($%.2f): "+
			"raise it with --budget, or calibrate ahead with agentium run calibrate", d.BudgetUSD, capUSD, prior, d.PairCapUSD()))
	}
	head, err := r.KeepHead(ctx)
	if err != nil {
		return err
	}
	runEnv, err := r.NewRunEnv(time.Minute)
	if err != nil {
		return err
	}
	var models []string // in arm order
	for _, n := range needs {
		if !slices.Contains(models, n.Model) {
			models = append(models, n.Model)
		}
	}
	for _, model := range models {
		var arms []task.Arm
		base := false
		for _, n := range needs {
			switch {
			case n.Model != model:
			case n.Arm.Snapshot == "":
				base = true
			default:
				arms = append(arms, task.Arm{Name: n.Arm.Context, Snapshot: n.Arm.Snapshot})
			}
		}
		sources, err := run.ArmSources(ctx, p.Bare, head, arms)
		if err != nil {
			return err
		}
		if !base {
			sources = sources[1:] // the base context is the first
		}
		fmt.Fprintf(out, "Calibrating %d context(s) on %s with real Claude Code runs (sign-in %s): up to $%.2f each, counted in the budget.\n",
			len(sources), model, runEnv.SignIn, CalibrationBudgetUSD)
		results, err := run.Calibrator{Head: head, Arms: sources, Model: model, Budget: CalibrationBudgetUSD, Timeout: CalibrationTimeout,
			SignIn: runEnv.SignIn, Now: r.Now,
			Execute: func(ctx context.Context, _ task.Arm, spec run.Spec) (run.Record, error) {
				return r.ExecuteRun(ctx, runEnv, RunMeta{Kind: KindCalibration, ExperimentID: stored.ID}, spec)
			},
			Save: func(ctx context.Context, c run.Calibration) error { return p.saveCalibration(ctx, c, r.Now()) },
		}.Run(ctx)
		if err != nil {
			return err
		}
		if err := run.WriteCalibrations(out, st, results); err != nil {
			return err
		}
		if !run.AllHealthy(results) {
			var failed []string
			for _, c := range results {
				if !c.Healthy() {
					failed = append(failed, c.Arm)
				}
			}
			return errors.New("a calibration on " + model + " failed its checks (context " + strings.Join(failed, ", ") +
				"): failed arms were not saved as calibrations (see the table above and the run records), so the experiment did not start; " +
				"nothing is locked, and healthy calibrations are kept")
		}
	}
	return nil
}

// KindCalibration is the kind of a calibration run (RunMeta.Kind); a task run's kind is empty.
const KindCalibration = "calibration"

// saveCalibration stores a healthy calibration as what later runs of its arm are checked against, as `run calibrate`
// does.
func (p Project) saveCalibration(ctx context.Context, c run.Calibration, now time.Time) error {
	encoded, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode calibration: %w", err)
	}
	return p.DB.SaveCalibration(ctx, store.Calibration{ProjectID: p.ID, Arm: c.Arm, Snapshot: c.Snapshot, RunID: c.RunID, Result: encoded, CreatedAt: now})
}
