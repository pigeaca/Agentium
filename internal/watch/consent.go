// Package watch holds the deep watch's state and rules: the user's consent to spend (one budget for the whole data
// folder, since the subscription's windows belong to one login), each project's loops, the ledger of what the watch
// spent in dollars and in window share, and drift panels. The store keeps the rows (migrations/0011_watch.sql); this
// package decides what they allow. The pass itself (`agentium watch --once`) builds on it.
package watch

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	// openTTY opens the controlling terminal for ConfirmAtTerminal; nil opens /dev/tty. Tests set it.
	openTTY func() (io.ReadWriteCloser, error)
}

// Caps are what the user allows the watch to use, for all projects together. Shares and thresholds are fractions
// (0.3 is 30%).
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

// Loops are what the watch may do in one project.
type Loops struct {
	Experiments bool // continue enrolled experiments
	Drift       bool // drift checks
	Screens     bool // queued cost screens
}

// Grant is a consent to record.
type Grant struct {
	Caps      Caps
	SignIn    SignIn // what the consent holds for
	GrantedBy string // the OS user
	Version   string // Agentium's version
}

// Consent is the consent in force.
type Consent struct {
	ID        int64
	Caps      Caps
	SignIn    SignIn
	GrantedBy string
	Version   string
	Confirmed bool // the user confirmed it at a terminal (a lowering is not confirmed)
	GrantedAt time.Time
}

var (
	// ErrNoConsent: no consent is in force (none recorded, or revoked), so the watch spends nothing.
	ErrNoConsent = errors.New("the watch has no consent: run agentium watch enable at a terminal")
	// ErrSignInChanged: Agentium now signs in another way, or with another token file, than the consent was given
	// for; it needs consent again.
	ErrSignInChanged = errors.New("the sign-in changed since the watch's consent: run agentium watch enable again")
	// ErrRaise: an unconfirmed grant would raise the consent in force, grant one where none is, or turn a loop on.
	ErrRaise = store.ErrConsentRaise
)

// Consent returns the consent in force for the sign-in Agentium uses now. Every spend checks it first: no consent gives
// ErrNoConsent, and a consent given for another sign-in mode or identity gives ErrSignInChanged.
func (s Service) Consent(ctx context.Context, current SignIn) (Consent, error) {
	row, err := s.DB.WatchConsentInForce(ctx)
	if errors.Is(err, store.ErrNotFound) || err == nil && !row.Enabled {
		return Consent{}, ErrNoConsent
	}
	if err != nil {
		return Consent{}, err
	}
	c := consentOf(row)
	if change := c.SignIn.changeTo(current); change != "" {
		return Consent{}, fmt.Errorf("consent given for %s, the sign-in changed %s: %w", c.SignIn.Mode, change, ErrSignInChanged)
	}
	return c, nil
}

// Grant records a consent, which then is the consent in force. With a confirmation (ConfirmAtTerminal) it may set
// any caps; the confirmation must be for these caps and this sign-in. Without one (nil) it may only lower: every cap
// and threshold at or below the consent in force, with the same sign-in, or it gives ErrRaise naming what it would
// raise. The store refuses an unconfirmed raise on its own as well.
func (s Service) Grant(ctx context.Context, g Grant, confirm *TerminalConfirmation) (Consent, error) {
	if err := g.validate(); err != nil {
		return Consent{}, err
	}
	if confirm != nil {
		if err := confirm.covers(g); err != nil {
			return Consent{}, err
		}
	} else {
		current, err := s.DB.WatchConsentInForce(ctx)
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
	row, err := s.DB.AddWatchConsent(ctx, store.WatchConsent{Enabled: true, WeeklyUSD: g.Caps.WeeklyUSD, RunCapUSD: g.Caps.RunCapUSD,
		PassShare: g.Caps.PassShare, WeeklyShare: g.Caps.WeeklyShare, StartFiveHour: g.Caps.StartFiveHour, StartSevenDay: g.Caps.StartSevenDay,
		SignIn: g.SignIn.Mode, SignInIdentity: g.SignIn.Identity, Confirmed: confirm != nil, GrantedBy: g.GrantedBy, AgentiumVersion: g.Version,
		GrantedAt: s.Now()})
	if err != nil {
		return Consent{}, err
	}
	return consentOf(row), nil
}

// Revoke ends the consent: the watch spends nothing until a confirmed grant. It reports false when none was in force.
// Revoking needs no terminal.
func (s Service) Revoke(ctx context.Context, by, version string) (bool, error) {
	current, err := s.DB.WatchConsentInForce(ctx)
	if errors.Is(err, store.ErrNotFound) || err == nil && !current.Enabled {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := s.DB.AddWatchConsent(ctx, store.WatchConsent{SignIn: current.SignIn, SignInIdentity: current.SignInIdentity, GrantedBy: by,
		AgentiumVersion: version, GrantedAt: s.Now()}); err != nil {
		return false, err
	}
	return true, nil
}

// SetLoops records what the watch may do in a project. Turning a loop on needs a confirmation for this project that
// names that loop (ConfirmAtTerminal with ProjectLoops), as raising a cap does; turning loops off does not (nil). The
// store refuses an unconfirmed loop on its own as well.
func (s Service) SetLoops(ctx context.Context, projectID int64, loops Loops, by string, confirm *TerminalConfirmation) error {
	if strings.TrimSpace(by) == "" {
		return errors.New("the watch's loops: no user named as setting them")
	}
	current, err := s.Loops(ctx, projectID)
	if err != nil {
		return err
	}
	if confirm != nil {
		if err := confirm.coversLoops(projectID, current, loops); err != nil {
			return err
		}
	} else if raised := loopRaises(current, loops); len(raised) > 0 {
		return fmt.Errorf("it would turn on %s: %w", strings.Join(raised, ", "), ErrRaise)
	}
	return s.DB.SetWatchLoops(ctx, store.WatchLoops{ProjectID: projectID, Experiments: loops.Experiments, Drift: loops.Drift,
		Screens: loops.Screens, Confirmed: confirm != nil, SetBy: by, SetAt: s.Now()})
}

// Loops returns what the watch may do in a project: nothing until set.
func (s Service) Loops(ctx context.Context, projectID int64) (Loops, error) {
	row, err := s.DB.WatchLoopsOf(ctx, projectID)
	if errors.Is(err, store.ErrNotFound) {
		return Loops{}, nil
	}
	if err != nil {
		return Loops{}, err
	}
	return Loops{Experiments: row.Experiments, Drift: row.Drift, Screens: row.Screens}, nil
}

// Raises names what g would raise over the consent c: a cap, a threshold, or the sign-in.
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
	if change := c.SignIn.changeTo(g.SignIn); change != "" {
		raised = append(raised, "the sign-in ("+change+")")
	}
	money("the weekly budget", c.Caps.WeeklyUSD, g.Caps.WeeklyUSD)
	money("the run cap", c.Caps.RunCapUSD, g.Caps.RunCapUSD)
	share("the pass share", c.Caps.PassShare, g.Caps.PassShare)
	share("the weekly share", c.Caps.WeeklyShare, g.Caps.WeeklyShare)
	share("the five-hour start threshold", c.Caps.StartFiveHour, g.Caps.StartFiveHour)
	share("the seven-day start threshold", c.Caps.StartSevenDay, g.Caps.StartSevenDay)
	return raised
}

func loopRaises(from, to Loops) []string {
	var raised []string
	for _, l := range []struct {
		name     string
		from, to bool
	}{{"the experiments loop", from.Experiments, to.Experiments}, {"the drift loop", from.Drift, to.Drift}, {"the screens loop", from.Screens, to.Screens}} {
		if l.to && !l.from {
			raised = append(raised, l.name)
		}
	}
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
	switch g.SignIn.Mode {
	case claude.SignInLogin, claude.SignInTokenFile, claude.SignInAPIKey:
	default:
		problems = append(problems, fmt.Sprintf("unknown sign-in mode %q", g.SignIn.Mode))
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
	return Consent{ID: row.ID, SignIn: SignIn{Mode: row.SignIn, Identity: row.SignInIdentity}, GrantedBy: row.GrantedBy,
		Version: row.AgentiumVersion, Confirmed: row.Confirmed, GrantedAt: row.GrantedAt,
		Caps: Caps{WeeklyUSD: row.WeeklyUSD, RunCapUSD: row.RunCapUSD, PassShare: row.PassShare, WeeklyShare: row.WeeklyShare,
			StartFiveHour: row.StartFiveHour, StartSevenDay: row.StartSevenDay}}
}
