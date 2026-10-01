package experiment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/project"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
	"github.com/pigeaca/agentium/internal/term"
)

// minValidateRepeats is how many validation repeats experiment plan asks for before it stops warning.
const minValidateRepeats = 3

// Check is one line of a readiness report: "ok", "MISSING" or "WARNING", and what it found.
type Check struct{ Status, Text string }

// Readiness is what running an experiment needs, checked: Ready is false when anything is MISSING (warnings do not
// count).
type Readiness struct {
	Checks []Check
	Ready  bool
}

// Write prints the checks, one per line.
func (r Readiness) Write(out io.Writer, st term.Style) {
	for _, c := range r.Checks {
		fmt.Fprintf(out, "  %s %s\n", st.Status(fmt.Sprintf("%-8s", c.Status)), c.Text)
	}
}

// ReadinessEnv is what the checks need from the machine, which the command line supplies.
type ReadinessEnv struct {
	Claude func() (string, error) // where Claude Code is
	SignIn string                 // how runs would sign in now (claude.SignInAPIKey, ...)
	Style  term.Style             // styles the commands the checks suggest
}

// checker collects a readiness report's lines.
type checker struct {
	r Readiness
}

func (c *checker) check(status, text string) { c.r.Checks = append(c.r.Checks, Check{status, text}) }

func (c *checker) line(ok bool, format string, a ...any) {
	c.r.Ready = c.r.Ready && ok
	c.check(map[bool]string{true: "ok", false: "MISSING"}[ok], fmt.Sprintf(format, a...))
}

// CheckReadiness checks what running needs: Claude Code, each context's calibration on its version and the
// experiment's model, the snapshot commits, and every task still eligible. The report says whether all is in place.
func CheckReadiness(ctx context.Context, p Project, e ReadinessEnv, d Design, eligible []string, reasons map[string]string, est Estimate) Readiness {
	c := &checker{r: Readiness{Ready: true}}
	version := c.claude(ctx, e)
	c.contexts(ctx, p, e, d, version)
	c.tasks(d, eligible, reasons)
	c.fairness(ctx, p, e, d, eligible)
	if c.r.Ready {
		c.check("ok", fmt.Sprintf("%d task(s), each valid in every arm's context", len(d.Tasks)))
	}
	expected, known := est.DesignUSD(d)
	expected += d.JudgeEstimateUSD()
	if reserve := Reserve(d); known && d.BudgetUSD < expected+reserve {
		c.check("WARNING", fmt.Sprintf("the budget $%.2f is below the estimated $%.2f plus $%.2f held for runs in flight: expect it to stop the experiment early",
			d.BudgetUSD, expected, reserve))
	}
	return c.r
}

// claude checks that Claude Code is found and reports its version, "" when it is not known.
func (c *checker) claude(ctx context.Context, e ReadinessEnv) string {
	version := ""
	if cli, err := e.Claude(); err != nil {
		c.line(false, "%v", err)
	} else if version, err = project.ClaudeVersion(ctx, cli); err != nil {
		c.line(false, "Claude Code at %s: its version could not be read: %v", cli, err)
	} else {
		c.line(true, "Claude Code %s at %s", version, cli)
	}
	return version
}

// contexts checks each distinct context: its snapshot commit is kept, and its calibration is on this Claude Code, this
// model and this sign-in.
func (c *checker) contexts(ctx context.Context, p Project, e ReadinessEnv, d Design, version string) {
	var seen []Arm
	for _, a := range d.Arms {
		if slices.ContainsFunc(seen, func(s Arm) bool { return s.Context == a.Context && s.Snapshot == a.Snapshot }) {
			continue
		}
		seen = append(seen, a)
		if a.Snapshot != "" {
			if _, err := gitx.Run(ctx, "--git-dir", p.Bare, "cat-file", "-e", a.Snapshot+"^{commit}"); err != nil {
				c.line(false, "context %s: its snapshot commit %s is gone from Agentium's repository", a.Context, ShortCommit(a.Snapshot))
				continue
			}
		}
		calibrate := "agentium run calibrate --model " + d.Model
		if a.Snapshot != "" {
			calibrate += " --snapshot " + a.Context
		}
		c.calibrated(ctx, p, e, d, a, version, e.Style.Command(calibrate))
	}
}

// calibrated checks arm a's latest calibration; calibrate is the command that makes a new one.
func (c *checker) calibrated(ctx context.Context, p Project, e ReadinessEnv, d Design, a Arm, version, calibrate string) {
	stored, err := p.DB.LatestCalibration(ctx, p.ID, a.Context, a.Snapshot)
	if errors.Is(err, store.ErrNotFound) {
		c.line(false, "context %s is not calibrated: %s", a.Context, calibrate)
		return
	}
	if err != nil {
		c.line(false, "context %s: %v", a.Context, err)
		return
	}
	var cal run.Calibration
	if err := json.Unmarshal(stored.Result, &cal); err != nil {
		c.line(false, "context %s: its calibration cannot be read: %v", a.Context, err)
		return
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
		c.line(false, "context %s was calibrated on Claude Code %s, not %s: %s", a.Context, cal.CLIVersion, version, calibrate)
	case cal.RequestedModel != d.Model:
		c.line(false, "context %s was calibrated with %s, not %s: %s", a.Context, orNone(cal.RequestedModel), d.Model, calibrate)
	case cal.SignIn != e.SignIn:
		c.line(false, "context %s was calibrated with sign-in %s, and runs would now use %s: %s", a.Context, orNone(cal.SignIn), e.SignIn, calibrate)
	default:
		c.line(true, "context %s calibrated %s: first request %d tokens", a.Context, stored.CreatedAt.Format("2006-01-02 15:04"), cal.FirstRequest)
	}
}

// tasks checks that every task of the design can still be in it.
func (c *checker) tasks(d Design, eligible []string, reasons map[string]string) {
	var missing []string
	for _, t := range d.Tasks {
		if why, known := reasons[t]; known {
			c.line(false, "task %s: %s", t, why)
		} else if !slices.Contains(eligible, t) {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		c.line(false, "task(s) removed since the experiment was made: %s", strings.Join(missing, ", "))
	}
}

// fairness warns about tasks whose hidden tests need what nothing states, and about thin validation.
func (c *checker) fairness(ctx context.Context, p Project, e ReadinessEnv, d Design, eligible []string) {
	var unfair, unchecked, few, unreadable []string
	fair := task.NewFairness("--git-dir", p.Bare)
	for _, name := range d.Tasks {
		t, err := p.DB.TaskByName(ctx, p.ID, name)
		if err != nil {
			continue // reported above as removed
		}
		if gaps, err := task.Gaps(ctx, fair, t); err != nil {
			unchecked = append(unchecked, name)
		} else if len(gaps) > 0 {
			unfair = append(unfair, fmt.Sprintf("%s (%d)", name, len(gaps)))
		}
		if t.Validation != nil && slices.Contains(eligible, name) {
			var v task.Validation
			if err := json.Unmarshal(t.Validation, &v); err != nil {
				unreadable = append(unreadable, name)
			} else if v.RepeatCount() < minValidateRepeats {
				few = append(few, name)
			}
		}
	}
	if len(unfair) > 0 {
		c.check("WARNING", "hidden tests require what nothing states, so a fair agent may fail them (task show lists it): "+strings.Join(unfair, ", "))
	}
	if len(unchecked) > 0 {
		c.check("WARNING", "what the hidden tests require could not be checked for: "+strings.Join(unchecked, ", "))
	}
	if len(unreadable) > 0 {
		c.check("WARNING", "the validation of these tasks cannot be read, so their repeats are unknown: "+strings.Join(unreadable, ", "))
	}
	if len(few) > 0 {
		c.check("WARNING", fmt.Sprintf("validated with fewer than %d repeats, so flaky checks may not show yet: %s (%s)", minValidateRepeats,
			strings.Join(few, ", "), e.Style.Command(fmt.Sprintf("agentium task validate NAME --repeat %d%s", minValidateRepeats, snapshotFlags(d.Arms)))))
	}
}

// snapshotFlags is the arms' " --snapshot NAME" flags for a task validate command.
func snapshotFlags(arms []Arm) string {
	var flags string
	var named []string
	for _, a := range arms {
		if a.Context != BaseContext && !slices.Contains(named, a.Context) {
			named = append(named, a.Context)
			flags += " --snapshot " + a.Context
		}
	}
	return flags
}

func orNone(value string) string {
	if value == "" {
		return "none found"
	}
	return value
}
