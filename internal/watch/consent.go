// Package watch holds the deep watch's state and rules: the user's consent to spend, the ledger of what the watch spent
// in dollars and in window share, and drift panels. The store keeps the rows (migrations/0011_watch.sql); this package
// decides what they allow. The pass itself (`agentium watch --once`) builds on it.
package watch

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/store"
)

// Service reads and writes the watch's state. Now is the clock every time-dependent rule reads.
type Service struct {
	DB  *store.Store
	Now func() time.Time
}

// Caps are what the user allows the watch to use, per project. Shares and thresholds are fractions (0.3 is 30%).
type Caps struct {
	WeeklyUSD float64 // dollars at list price over any 7 days, the screens' included
	RunCapUSD float64 // one run's cap: a pair starts only if two fit the week's remainder
	// PassShare caps a pass's use of the five-hour window, and WeeklyShare the watch's use of the seven-day window over
	// any 7 days. With an API key neither applies.
	PassShare   float64
	WeeklyShare float64
	// StartFiveHour and StartSevenDay leave room for the user: no pair starts when the latest reading is above either.
	StartFiveHour float64
	StartSevenDay float64
}

// DefaultCaps are the user's decisions of 2026-10-02: $20 a week, $3 a run, 30% of a five-hour window per pass and
// 15% of the seven-day window a week, and nothing started above 50% (five-hour) or 60% (seven-day).
func DefaultCaps() Caps {
	return Caps{WeeklyUSD: 20, RunCapUSD: 3, PassShare: 0.30, WeeklyShare: 0.15, StartFiveHour: 0.50, StartSevenDay: 0.60}
}

// Loops are the parts of a pass the user enabled.
type Loops struct {
	Experiments bool // continue enrolled experiments
	Drift       bool // drift checks
	Screens     bool // queued cost screens
}

// Grant is a consent to record.
type Grant struct {
	ProjectID int64
	Caps      Caps
	Loops     Loops
	SignIn    string // claude.SignIn*: the mode the consent holds for
	GrantedBy string // the OS user
	Version   string // Agentium's version
	// Interactive is true only for `agentium watch enable` at a terminal, the one command that may raise a cap,
	// threshold or loop, or change the sign-in mode. Every other writer (agentium.toml, later) may only lower.
	Interactive bool
}

// Consent is the consent in force for a project.
type Consent struct {
	ID          int64
	ProjectID   int64
	Caps        Caps
	Loops       Loops
	SignIn      string
	GrantedBy   string
	Version     string
	Interactive bool
	GrantedAt   time.Time
}

var (
	// ErrNoConsent: the project has no consent in force (none recorded, or revoked), so the watch spends nothing on it.
	ErrNoConsent = errors.New("the watch has no consent for this project: run agentium watch enable at a terminal")
	// ErrSignInChanged: the project now signs in another way than the consent was given for; it needs consent again.
	ErrSignInChanged = errors.New("the sign-in mode changed since the watch's consent: run agentium watch enable again")
	// ErrRaise: a grant that is not interactive would raise the consent in force, or grant one where none is.
	ErrRaise = store.ErrConsentRaise
)

// Consent returns the project's consent in force for the sign-in mode it uses now. Every spend checks it first: no
// consent gives ErrNoConsent, and a consent given under another sign-in mode gives ErrSignInChanged.
func (s Service) Consent(ctx context.Context, projectID int64, signIn string) (Consent, error) {
	row, err := s.DB.WatchConsentOf(ctx, projectID)
	if errors.Is(err, store.ErrNotFound) || err == nil && !row.Enabled {
		return Consent{}, ErrNoConsent
	}
	if err != nil {
		return Consent{}, err
	}
	if row.SignIn != signIn {
		return Consent{}, fmt.Errorf("consent given for %s, the project now uses %s: %w", row.SignIn, signIn, ErrSignInChanged)
	}
	return consentOf(row), nil
}

// Grant records a consent, which then is the consent in force. A grant that is not interactive must keep every cap,
// threshold and loop at or below the consent in force, with the same sign-in mode, or it gives ErrRaise naming what
// it would raise; the store refuses the same inside its insert.
func (s Service) Grant(ctx context.Context, g Grant) (Consent, error) {
	if err := g.validate(); err != nil {
		return Consent{}, err
	}
	if !g.Interactive {
		current, err := s.DB.WatchConsentOf(ctx, g.ProjectID)
		if errors.Is(err, store.ErrNotFound) || err == nil && !current.Enabled {
			return Consent{}, fmt.Errorf("no consent in force to lower: %w", ErrRaise)
		}
		if err != nil {
			return Consent{}, err
		}
		if raised := Raises(consentOf(current), g); len(raised) > 0 {
			return Consent{}, fmt.Errorf("it would raise %s: %w", strings.Join(raised, ", "), ErrRaise)
		}
	}
	row, err := s.DB.AddWatchConsent(ctx, store.WatchConsent{ProjectID: g.ProjectID, Enabled: true, WeeklyUSD: g.Caps.WeeklyUSD,
		RunCapUSD: g.Caps.RunCapUSD, PassShare: g.Caps.PassShare, WeeklyShare: g.Caps.WeeklyShare, StartFiveHour: g.Caps.StartFiveHour,
		StartSevenDay: g.Caps.StartSevenDay, LoopExperiments: g.Loops.Experiments, LoopDrift: g.Loops.Drift, LoopScreens: g.Loops.Screens,
		SignIn: g.SignIn, Interactive: g.Interactive, GrantedBy: g.GrantedBy, AgentiumVersion: g.Version, GrantedAt: s.Now()})
	if err != nil {
		return Consent{}, err
	}
	return consentOf(row), nil
}

// Revoke ends the project's consent: the watch spends nothing on it until an interactive grant. It reports false when
// no consent was in force. Revoking needs no terminal.
func (s Service) Revoke(ctx context.Context, projectID int64, by, version string) (bool, error) {
	current, err := s.DB.WatchConsentOf(ctx, projectID)
	if errors.Is(err, store.ErrNotFound) || err == nil && !current.Enabled {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := s.DB.AddWatchConsent(ctx, store.WatchConsent{ProjectID: projectID, SignIn: current.SignIn, GrantedBy: by,
		AgentiumVersion: version, GrantedAt: s.Now()}); err != nil {
		return false, err
	}
	return true, nil
}

// Raises names what g would raise over the consent c: a cap, a threshold, a loop, or the sign-in mode.
func Raises(c Consent, g Grant) []string {
	var raised []string
	money := func(name string, from, to float64) {
		if to > from {
			raised = append(raised, fmt.Sprintf("%s ($%.2f to $%.2f)", name, from, to))
		}
	}
	share := func(name string, from, to float64) {
		if to > from {
			raised = append(raised, fmt.Sprintf("%s (%.0f%% to %.0f%%)", name, 100*from, 100*to))
		}
	}
	loop := func(name string, from, to bool) {
		if to && !from {
			raised = append(raised, "the "+name+" loop")
		}
	}
	if g.SignIn != c.SignIn {
		raised = append(raised, fmt.Sprintf("the sign-in mode (%s to %s)", c.SignIn, g.SignIn))
	}
	money("the weekly budget", c.Caps.WeeklyUSD, g.Caps.WeeklyUSD)
	money("the run cap", c.Caps.RunCapUSD, g.Caps.RunCapUSD)
	share("the pass share", c.Caps.PassShare, g.Caps.PassShare)
	share("the weekly share", c.Caps.WeeklyShare, g.Caps.WeeklyShare)
	share("the five-hour start threshold", c.Caps.StartFiveHour, g.Caps.StartFiveHour)
	share("the seven-day start threshold", c.Caps.StartSevenDay, g.Caps.StartSevenDay)
	loop("experiments", c.Loops.Experiments, g.Loops.Experiments)
	loop("drift", c.Loops.Drift, g.Loops.Drift)
	loop("screens", c.Loops.Screens, g.Loops.Screens)
	return raised
}

func (g Grant) validate() error {
	c := g.Caps
	var problems []string
	for _, f := range []struct {
		name  string
		value float64
		max   float64
	}{
		{"the weekly budget", c.WeeklyUSD, math.Inf(1)}, {"the run cap", c.RunCapUSD, math.Inf(1)},
		{"the pass share", c.PassShare, 1}, {"the weekly share", c.WeeklyShare, 1},
		{"the five-hour start threshold", c.StartFiveHour, 1}, {"the seven-day start threshold", c.StartSevenDay, 1},
	} {
		if math.IsNaN(f.value) || math.IsInf(f.value, 0) || f.value <= 0 || f.value > f.max {
			bound := ""
			if f.max == 1 {
				bound = " and at most 100%"
			}
			problems = append(problems, fmt.Sprintf("%s must be above 0%s (got %v)", f.name, bound, f.value))
		}
	}
	if c.RunCapUSD > c.WeeklyUSD {
		problems = append(problems, fmt.Sprintf("the run cap ($%.2f) is above the weekly budget ($%.2f)", c.RunCapUSD, c.WeeklyUSD))
	}
	switch g.SignIn {
	case claude.SignInLogin, claude.SignInTokenFile, claude.SignInAPIKey:
	default:
		problems = append(problems, fmt.Sprintf("unknown sign-in mode %q", g.SignIn))
	}
	if !g.Loops.Experiments && !g.Loops.Drift && !g.Loops.Screens {
		problems = append(problems, "no loop enabled")
	}
	if strings.TrimSpace(g.GrantedBy) == "" {
		problems = append(problems, "no user named as granting it")
	}
	if len(problems) > 0 {
		return fmt.Errorf("the watch's consent: %s", strings.Join(problems, "; "))
	}
	return nil
}

func consentOf(row store.WatchConsent) Consent {
	return Consent{ID: row.ID, ProjectID: row.ProjectID, SignIn: row.SignIn, GrantedBy: row.GrantedBy, Version: row.AgentiumVersion,
		Interactive: row.Interactive, GrantedAt: row.GrantedAt,
		Caps: Caps{WeeklyUSD: row.WeeklyUSD, RunCapUSD: row.RunCapUSD, PassShare: row.PassShare, WeeklyShare: row.WeeklyShare,
			StartFiveHour: row.StartFiveHour, StartSevenDay: row.StartSevenDay},
		Loops: Loops{Experiments: row.LoopExperiments, Drift: row.LoopDrift, Screens: row.LoopScreens}}
}
