package watch

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
)

// Week is the span of the weekly caps: the ledger sums the watch's runs that finished in the 7 days before now, a
// rolling window rather than calendar weeks, so no boundary resets the budget early.
const Week = 7 * 24 * time.Hour

// Use is what a set of runs used, in both units.
type Use struct {
	Runs int
	// AgentUSD and JudgeUSD are the runs' spend at list price (run.StoredSpend): notional on a subscription, but they
	// still cap a runaway. Estimated: some run's agent cost was priced from its transcript.
	AgentUSD  float64
	JudgeUSD  float64
	Estimated bool
	// Share is the window share the runs used; nil with an API key, whose runs use no subscription.
	Share *Share
}

// USD is everything the runs spent: the agents' runs and their judgements.
func (u Use) USD() float64 { return u.AgentUSD + u.JudgeUSD }

// Share is a use of the subscription's windows, as fractions of one window (0.3 is 30% of one five-hour window).
type Share struct {
	FiveHour float64
	SevenDay float64
	// Estimates counts the runs that added the per-run estimate somewhere, once per run (see Measure).
	Estimates int
}

// Measure is what runs used. Dollars are each run's stored spend, the judge's included. With shares, the window share
// is derived from the runs' usage readings:
//   - Runs are grouped by their watch pass (a run without a pass is its own group), since between passes the user's
//     own use moves the readings too.
//   - In each group, for each window (keyed by its reset time), the use is the highest reading in that window less
//     the lowest first reading of a run in it. A window that began during a run of the group (the run crossed its
//     reset) is counted from 0, whatever later runs read first, so adding a run never hides use already counted.
//     Anything else using the subscription meanwhile, the user included, counts as the watch's.
//   - Where readings cannot tell, the run adds perRun, the five-hour estimate per run (experiment.UsagePerRun), also
//     in the seven-day window: a subscription run with a cost but no readings adds it to both, a run that crossed a
//     reset adds it for the part before the reset, and a run whose readings lack the seven-day window adds it there.
//
// It errs high where readings cannot tell, but two uses are not seen: a run's first reading may come after its first
// request, and use after a group's last reading (a run still going, or its last requests) shows only in a later
// reading. The five-hour use of runs that span several windows is their sum, in units of one window.
func Measure(runs []store.Run, shares bool, perRun float64) Use {
	use := Use{Runs: len(runs)}
	if shares {
		use.Share = &Share{}
	}
	groups := map[string][]readings{}
	var order []string
	for i, r := range runs {
		spend := run.StoredSpend(r.CostUSD, r.Record)
		use.AgentUSD += spend.AgentUSD
		use.JudgeUSD += spend.JudgeUSD
		use.Estimated = use.Estimated || spend.AgentEstimated
		if !shares {
			continue
		}
		rec := decodeReadings(r.Record)
		if rec.Metrics.UsageFirst == nil || rec.Metrics.UsageLast == nil {
			if subscription(rec.SignIn) && spend.TotalUSD() > 0 {
				use.Share.FiveHour += perRun
				use.Share.SevenDay += perRun
				use.Share.Estimates++
			}
			continue
		}
		key := "run " + r.ID
		if r.WatchPassID != 0 {
			key = "pass " + strconv.FormatInt(r.WatchPassID, 10)
		}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], readings{run: i, first: *rec.Metrics.UsageFirst, last: *rec.Metrics.UsageLast})
	}
	estimated := map[int]bool{}
	for _, key := range order {
		for _, d := range []struct {
			dim dimension
			to  *float64
		}{{fiveHour, &use.Share.FiveHour}, {sevenDay, &use.Share.SevenDay}} {
			rise, unknown := d.dim.rise(groups[key])
			*d.to += rise + perRun*float64(len(unknown))
			for _, i := range unknown {
				estimated[i] = true
			}
		}
	}
	if shares {
		use.Share.Estimates += len(estimated)
	}
	return use
}

// readings are a run's first and last usage readings; run is its index in Measure's runs.
type readings struct {
	run         int
	first, last claude.UsageReading
}

// dimension is one kind of window: the five-hour or the seven-day.
type dimension int

const (
	fiveHour dimension = iota
	sevenDay
)

func (d dimension) resets(u claude.UsageReading) time.Time {
	if d == sevenDay {
		return u.SevenDayResets
	}
	return u.FiveHourResets
}

func (d dimension) value(u claude.UsageReading) float64 {
	if d == sevenDay {
		return u.SevenDay
	}
	return u.FiveHour
}

// rise is a group's use of one kind of window (see Measure), and the runs it could not measure there: those that
// crossed a reset, or whose readings lack this window.
func (d dimension) rise(group []readings) (total float64, unknown []int) {
	type window struct {
		base, high float64
		hasBase    bool
	}
	windows := map[time.Time]*window{}
	at := func(t time.Time) *window {
		w := windows[t]
		if w == nil {
			w = &window{}
			windows[t] = w
		}
		return w
	}
	lower := func(w *window, v float64) {
		if !w.hasBase || v < w.base {
			w.base, w.hasBase = v, true
		}
	}
	for _, g := range group {
		from, to := d.resets(g.first), d.resets(g.last)
		if from.IsZero() || to.IsZero() {
			unknown = append(unknown, g.run)
			continue
		}
		w := at(from)
		lower(w, d.value(g.first))
		w.high = max(w.high, d.value(g.first))
		w = at(to)
		w.high = max(w.high, d.value(g.last))
		if !from.Equal(to) {
			lower(w, 0) // the window began during this run: all of its use so far counts
			unknown = append(unknown, g.run)
		}
	}
	for _, w := range windows {
		base := 0.0
		if w.hasBase {
			base = w.base
		}
		total += max(w.high-base, 0)
	}
	return total, unknown
}

// recordReadings is what Measure reads from a stored run's record.
type recordReadings struct {
	SignIn  string `json:"sign_in"`
	Metrics struct {
		UsageFirst *claude.UsageReading `json:"usage_first"`
		UsageLast  *claude.UsageReading `json:"usage_last"`
	} `json:"metrics"`
}

// decodeReadings reads a record's sign-in and readings; an unreadable record has neither.
func decodeReadings(record []byte) recordReadings {
	var r recordReadings
	if json.Unmarshal(record, &r) != nil {
		return recordReadings{}
	}
	return r
}

func subscription(signIn string) bool {
	return signIn == claude.SignInLogin || signIn == claude.SignInTokenFile
}

// Window is where the subscription's windows stand, from the latest readings. A reading whose window has reset is
// unknown (Known false): readings arrive only with runs, so the next run is the probe.
type Window struct {
	FiveHour       float64
	FiveHourResets time.Time
	FiveHourKnown  bool
	SevenDay       float64
	SevenDayResets time.Time
	SevenDayKnown  bool
}

// Ledger is what the watch spent over the last Week, in every project together (the consent is one budget), derived
// from the runs its passes made (agent and judge spend, and usage readings), never from a counter of its own, so a
// crash cannot double-count or drop a stored run.
type Ledger struct {
	Now, Since time.Time
	// Shares is false with an API key: Week.Share and Current are then nil, and only dollars bind.
	Shares bool
	// PerRun is the five-hour share one run is expected to use (experiment.UsagePerRun over the week's runs of every
	// project), which Measure adds where readings cannot tell.
	PerRun float64
	// Week is the watch's runs that finished in (Since, Now], in every project: runs of every pass, scheduled or
	// started at the terminal, and only those.
	Week Use
	// Current is the latest reading of any run in the week, in any project, the user's own runs included.
	Current *Window
}

// Ledger reads the ledger at the service's clock, for the sign-in mode Agentium uses now.
func (s Service) Ledger(ctx context.Context, mode string) (Ledger, error) {
	now := s.Now()
	l := Ledger{Now: now, Since: now.Add(-Week), Shares: mode != claude.SignInAPIKey}
	runs, err := s.DB.RunsFinishedSince(ctx, l.Since)
	if err != nil {
		return Ledger{}, err
	}
	var watched []store.Run
	for _, r := range runs {
		if r.WatchPassID != 0 {
			watched = append(watched, r)
		}
	}
	if l.Shares {
		samples := experiment.UsageSamples(runs)
		l.PerRun, _ = experiment.UsagePerRun(samples)
		l.Current = current(samples, now)
	}
	l.Week = Measure(watched, l.Shares, l.PerRun)
	return l, nil
}

// current is where the windows stand at now, from the newest reading of each window.
func current(samples []experiment.UsageSample, now time.Time) *Window {
	var w Window
	for _, s := range samples {
		for _, u := range []claude.UsageReading{s.First, s.Last} {
			if !u.FiveHourResets.IsZero() && (u.FiveHourResets.After(w.FiveHourResets) ||
				u.FiveHourResets.Equal(w.FiveHourResets) && u.FiveHour > w.FiveHour) {
				w.FiveHour, w.FiveHourResets = u.FiveHour, u.FiveHourResets
			}
			if !u.SevenDayResets.IsZero() && (u.SevenDayResets.After(w.SevenDayResets) ||
				u.SevenDayResets.Equal(w.SevenDayResets) && u.SevenDay > w.SevenDay) {
				w.SevenDay, w.SevenDayResets = u.SevenDay, u.SevenDayResets
			}
		}
	}
	w.FiveHourKnown = now.Before(w.FiveHourResets)
	w.SevenDayKnown = now.Before(w.SevenDayResets)
	return &w
}

// Room is what is left of the consent's weekly caps: dollars, and, with shares, the seven-day share. Either is
// negative when an estimate overshot.
type Room struct {
	USD      float64
	SevenDay float64 // zero without shares
}

// Room is what the consent's weekly caps leave after the ledger's week.
func (l Ledger) Room(c Consent) Room {
	r := Room{USD: c.Caps.WeeklyUSD - l.Week.USD()}
	if l.Week.Share != nil {
		r.SevenDay = c.Caps.WeeklyShare - l.Week.Share.SevenDay
	}
	return r
}

// PassUse is what one pass used, across projects. With shares, API-key runs add dollars only: they have no readings
// and are not subscription runs.
func (s Service) PassUse(ctx context.Context, passID int64, shares bool, perRun float64) (Use, error) {
	runs, err := s.DB.PassRuns(ctx, passID)
	if err != nil {
		return Use{}, err
	}
	return Measure(runs, shares, perRun), nil
}

// CheckUse is what one drift check used, over the passes it spanned.
func (s Service) CheckUse(ctx context.Context, checkID int64, shares bool, perRun float64) (Use, error) {
	runs, err := s.DB.DriftCheckRuns(ctx, checkID)
	if err != nil {
		return Use{}, err
	}
	return Measure(runs, shares, perRun), nil
}
