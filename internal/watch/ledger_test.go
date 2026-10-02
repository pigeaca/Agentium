package watch

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/store"
)

// The week is rolling: a run that finished exactly 7 days before now is out, one a second later is in, and moving the
// clock a second drops it. Only the watch's runs count, judge costs included; the user's own run is left out of the
// spend but its absence of readings changes nothing.
func TestLedgerAtTheSevenDayBoundary(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	s, c, app := newService(t, now.Add(-8*24*time.Hour))
	old := startPass(t, s)
	c.now = now.Add(-time.Hour)
	recent := startPass(t, s)
	r1, r2, seven := now.Add(-7*24*time.Hour-time.Hour), now.Add(3*time.Hour), now.Add(2*24*time.Hour)
	saveRuns(t, s, app.ID,
		stored{id: "edge", pass: old, signIn: claude.SignInLogin, agent: 1, finished: now.Add(-Week),
			first: reading(0, r1, 0, seven), last: reading(0.5, r1, 0.1, seven)},
		stored{id: "inside", pass: old, signIn: claude.SignInLogin, agent: 2, judge: 0.25, finished: now.Add(-Week + time.Second),
			first: reading(0.10, r1, 0.20, seven), last: reading(0.16, r1, 0.21, seven)},
		stored{id: "recent", pass: recent, signIn: claude.SignInLogin, agent: 0.5, finished: now.Add(-time.Hour),
			first: reading(0.30, r2, 0.25, seven), last: reading(0.35, r2, 0.26, seven)},
		stored{id: "user", signIn: claude.SignInLogin, agent: 5, finished: now.Add(-2 * time.Hour)},
	)
	c.now = now
	l, err := s.Ledger(ctx, claude.SignInLogin)
	if err != nil {
		t.Fatal(err)
	}
	if !l.Since.Equal(now.Add(-Week)) || l.Week.Runs != 2 || !near(l.Week.AgentUSD, 2.5) || !near(l.Week.JudgeUSD, 0.25) || !near(l.Week.USD(), 2.75) {
		t.Errorf("the week = %+v since %v", l.Week, l.Since)
	}
	if sh := l.Week.Share; sh == nil || !near(sh.FiveHour, 0.11) || !near(sh.SevenDay, 0.02) || sh.Estimates != 0 {
		t.Errorf("the week's share = %+v, want 6%%+5%% five-hour and 1%%+1%% seven-day", sh)
	}
	if w := l.Current; w == nil || !w.FiveHourKnown || w.FiveHour != 0.35 || !w.SevenDayKnown || w.SevenDay != 0.26 {
		t.Errorf("the current windows = %+v", w)
	}
	consent := Consent{Caps: DefaultCaps()}
	if room := l.Room(consent); !near(room.USD, 17.25) || !near(room.SevenDay, 0.13) {
		t.Errorf("the room = %+v", room)
	}

	c.now = now.Add(time.Second)
	l, err = s.Ledger(ctx, claude.SignInLogin)
	if err != nil {
		t.Fatal(err)
	}
	if l.Week.Runs != 1 || !near(l.Week.USD(), 0.5) || !near(l.Week.Share.FiveHour, 0.05) {
		t.Errorf("a second later the week = %+v, %+v; want only the recent run", l.Week, l.Week.Share)
	}
}

// A five-hour reset inside a pass: each window counts its own rise, the new window from 0 because a run crossed into
// it (r4's later first reading of 0.04 does not hide r3's use after the reset), and the crossing run adds the per-run
// estimate for its part before the reset. Once the latest window resets, its reading is unknown (the next run probes),
// while the week's use stays.
func TestLedgerAcrossAFiveHourReset(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 5, 0, 0, 0, time.UTC)
	s, c, app := newService(t, now.Add(-4*time.Hour))
	pass := startPass(t, s)
	a, b, seven := now.Add(-time.Hour), now.Add(4*time.Hour), now.Add(3*24*time.Hour)
	saveRuns(t, s, app.ID,
		stored{id: "r1", pass: pass, signIn: claude.SignInLogin, agent: 1, finished: now.Add(-3 * time.Hour),
			first: reading(0.10, a, 0.40, seven), last: reading(0.16, a, 0.41, seven)},
		stored{id: "r2", pass: pass, signIn: claude.SignInLogin, agent: 1, finished: now.Add(-2 * time.Hour),
			first: reading(0.16, a, 0.41, seven), last: reading(0.22, a, 0.42, seven)},
		stored{id: "r3", pass: pass, signIn: claude.SignInLogin, agent: 1, finished: now.Add(-50 * time.Minute),
			first: reading(0.22, a, 0.42, seven), last: reading(0.04, b, 0.43, seven)},
		stored{id: "r4", pass: pass, signIn: claude.SignInLogin, agent: 1, finished: now.Add(-10 * time.Minute),
			first: reading(0.04, b, 0.43, seven), last: reading(0.10, b, 0.44, seven)},
	)
	c.now = now
	l, err := s.Ledger(ctx, claude.SignInLogin)
	if err != nil {
		t.Fatal(err)
	}
	if l.PerRun != experiment.DefaultUsagePerRun {
		t.Fatalf("per run = %v, want the default (no window has %d runs)", l.PerRun, experiment.MinUsageRuns)
	}
	// 0.22-0.10 in the first window, 0.10-0 in the second, and the estimate for r3's part before the reset.
	if sh := l.Week.Share; !near(sh.FiveHour, 0.12+0.10+0.06) || !near(sh.SevenDay, 0.04) || sh.Estimates != 1 {
		t.Errorf("the share across a five-hour reset = %+v", sh)
	}
	if w := l.Current; !w.FiveHourKnown || w.FiveHour != 0.10 || !w.FiveHourResets.Equal(b) {
		t.Errorf("the current five-hour window = %+v", w)
	}
	c.now = b.Add(time.Minute)
	l, err = s.Ledger(ctx, claude.SignInLogin)
	if err != nil {
		t.Fatal(err)
	}
	if w := l.Current; w.FiveHourKnown || !w.SevenDayKnown || w.SevenDay != 0.44 {
		t.Errorf("after the window reset, the current windows = %+v; want the five-hour unknown", w)
	}
	if !near(l.Week.Share.FiveHour, 0.28) || l.Week.Runs != 4 {
		t.Errorf("after the reset the week = %+v, %+v; want unchanged", l.Week, l.Week.Share)
	}
}

// A seven-day reset across passes: each pass counts its own rise, so the user's use between passes (0.34 to 0.50)
// is not the watch's; a run that crossed the reset counts the new window from 0 plus the estimate. When the first
// pass falls out of the week, its rise does.
func TestLedgerAcrossASevenDayReset(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	s, c, app := newService(t, now.Add(-3*day))
	s1, s2 := now.Add(-day), now.Add(6*day)
	five := func(at time.Time) time.Time { return at.Add(3 * time.Hour) }
	var passes []int64
	for _, at := range []time.Time{now.Add(-3 * day), now.Add(-2 * day), now.Add(-day), now.Add(-2 * time.Hour)} {
		c.now = at.Add(-time.Hour)
		passes = append(passes, startPass(t, s))
	}
	saveRuns(t, s, app.ID,
		stored{id: "p1a", pass: passes[0], signIn: claude.SignInLogin, agent: 1, finished: now.Add(-3*day - 30*time.Minute),
			first: reading(0.05, five(now.Add(-3*day)), 0.30, s1), last: reading(0.10, five(now.Add(-3*day)), 0.32, s1)},
		stored{id: "p1b", pass: passes[0], signIn: claude.SignInLogin, agent: 1, finished: now.Add(-3 * day),
			first: reading(0.10, five(now.Add(-3*day)), 0.32, s1), last: reading(0.15, five(now.Add(-3*day)), 0.34, s1)},
		stored{id: "p2", pass: passes[1], signIn: claude.SignInLogin, agent: 1, finished: now.Add(-2 * day),
			first: reading(0.02, five(now.Add(-2*day)), 0.50, s1), last: reading(0.08, five(now.Add(-2*day)), 0.52, s1)},
		stored{id: "p3", pass: passes[2], signIn: claude.SignInLogin, agent: 1, finished: now.Add(-day + 5*time.Minute),
			first: reading(0.30, five(now.Add(-day)), 0.61, s1), last: reading(0.33, five(now.Add(-day)), 0.01, s2)},
		stored{id: "p4", pass: passes[3], signIn: claude.SignInLogin, agent: 1, finished: now.Add(-2 * time.Hour),
			first: reading(0.20, five(now.Add(-2*time.Hour)), 0.05, s2), last: reading(0.26, five(now.Add(-2*time.Hour)), 0.07, s2)},
	)
	c.now = now
	l, err := s.Ledger(ctx, claude.SignInLogin)
	if err != nil {
		t.Fatal(err)
	}
	perRun := experiment.DefaultUsagePerRun
	// Seven-day: 0.04 (p1) + 0.02 (p2) + 0.01 and the estimate (p3 crossed the reset) + 0.02 (p4).
	if sh := l.Week.Share; !near(sh.SevenDay, 0.04+0.02+0.01+perRun+0.02) || !near(sh.FiveHour, 0.10+0.06+0.03+0.06) || sh.Estimates != 1 {
		t.Errorf("the share across a seven-day reset = %+v", sh)
	}
	if room := l.Room(Consent{Caps: DefaultCaps()}); !near(room.SevenDay, 0) || !near(room.USD, 15) {
		t.Errorf("the room = %+v; want the weekly share used up", room)
	}
	if w := l.Current; !w.SevenDayKnown || w.SevenDay != 0.07 || !w.SevenDayResets.Equal(s2) {
		t.Errorf("the current seven-day window = %+v", w)
	}
	c.now = now.Add(4*day + time.Hour)
	l, err = s.Ledger(ctx, claude.SignInLogin)
	if err != nil {
		t.Fatal(err)
	}
	if sh := l.Week.Share; l.Week.Runs != 3 || !near(sh.SevenDay, 0.02+0.01+perRun+0.02) {
		t.Errorf("once the first pass left the week: %+v, %+v", l.Week, sh)
	}
}

// An API-key project's ledger has dollars and no share: no window, no estimate, and its room is dollars only, even
// when other runs of the project read a subscription's windows.
func TestAPIKeyLedgerHasDollarsAndNoShare(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 5, 0, 0, 0, time.UTC)
	s, c, app := newService(t, now.Add(-time.Hour))
	pass := startPass(t, s)
	saveRuns(t, s, app.ID,
		stored{id: "k1", pass: pass, signIn: claude.SignInAPIKey, agent: 1.2, judge: 0.3, finished: now.Add(-30 * time.Minute)},
		stored{id: "k2", pass: pass, signIn: claude.SignInAPIKey, agent: 0.5, finished: now.Add(-10 * time.Minute)},
		stored{id: "old-login", signIn: claude.SignInLogin, agent: 1, finished: now.Add(-20 * time.Minute),
			first: reading(0.1, now.Add(time.Hour), 0.1, now.Add(time.Hour)), last: reading(0.2, now.Add(time.Hour), 0.1, now.Add(time.Hour))},
	)
	c.now = now
	l, err := s.Ledger(ctx, claude.SignInAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	if l.Shares || l.Week.Share != nil || l.Current != nil || l.PerRun != 0 {
		t.Errorf("an API-key ledger = %+v; want no share", l)
	}
	if l.Week.Runs != 2 || !near(l.Week.USD(), 2.0) {
		t.Errorf("an API-key week = %+v", l.Week)
	}
	if room := l.Room(Consent{Caps: DefaultCaps()}); !near(room.USD, 18) || room.SevenDay != 0 {
		t.Errorf("an API-key room = %+v", room)
	}
}

// Where readings cannot tell, Measure adds the estimate: a subscription run with a cost and no readings adds the per-run
// estimate in both units, as does a reading without the seven-day window; an API-key run in the same pass, a run that
// spent nothing and an unreadable record add none. Dollars still come from every run.
func TestMeasureEstimatesWhatReadingsCannotTell(t *testing.T) {
	five, seven := time.Date(2026, 10, 2, 5, 0, 0, 0, time.UTC), time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	record := func(signIn string, first, last *claude.UsageReading) []byte {
		return []byte(`{"sign_in":"` + signIn + `","metrics":{"usage_first":` + jsonOf(first) + `,"usage_last":` + jsonOf(last) + `}}`)
	}
	runs := []store.Run{
		{ID: "unread", WatchPassID: 1, CostUSD: 1, Record: record(claude.SignInLogin, nil, nil)},
		{ID: "token", WatchPassID: 1, CostUSD: 1, Record: record(claude.SignInTokenFile, nil, nil)},
		{ID: "api", WatchPassID: 1, CostUSD: 1, Record: record(claude.SignInAPIKey, nil, nil)},
		{ID: "free", WatchPassID: 1, CostUSD: 0, Record: record(claude.SignInLogin, nil, nil)},
		{ID: "corrupt", WatchPassID: 1, CostUSD: 1, Record: []byte(`{not json`)},
		{ID: "no-seven", WatchPassID: 2, CostUSD: 1, Record: record(claude.SignInLogin, reading(0.1, five, 0, time.Time{}), reading(0.2, five, 0, time.Time{}))},
		{ID: "full", WatchPassID: 3, CostUSD: 1, Record: record(claude.SignInLogin, reading(0.3, five, 0.1, seven), reading(0.35, five, 0.11, seven))},
		{ID: "both", WatchPassID: 4, CostUSD: 1, Record: record(claude.SignInLogin, reading(0.3, five, 0.1, seven),
			reading(0.02, five.Add(5*time.Hour), 0.01, seven.Add(Week)))},
	}
	use := Measure(runs, true, 0.05)
	if use.Runs != 8 || !near(use.USD(), 7) {
		t.Errorf("dollars = %+v", use)
	}
	// unread and token: 0.05 each in both; no-seven: 0.1 five-hour and 0.05 seven-day; full: 0.05 and 0.01; both
	// (crossed both resets): 0.02 and 0.01 in the new windows, and 0.05 in each for the parts before. Four runs were
	// estimated: "both" counts once though it was estimated in both windows.
	if sh := use.Share; !near(sh.FiveHour, 0.05+0.05+0.1+0.05+0.02+0.05) || !near(sh.SevenDay, 0.05+0.05+0.05+0.01+0.01+0.05) || sh.Estimates != 4 {
		t.Errorf("the share = %+v", sh)
	}
	if none := Measure(runs, false, 0.05); none.Share != nil || !near(none.USD(), 7) {
		t.Errorf("without shares = %+v", none)
	}
}

func jsonOf(u *claude.UsageReading) string {
	encoded, err := json.Marshal(u) // nil encodes as null
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// Crash and recovery: the ledger is derived from the stored runs, so it neither drops nor double-counts. A pass whose
// process died still counts its runs, a run recovered from its start file later counts once under its pass, and
// reading the ledger again gives the same figures.
func TestLedgerIsDerivedFromRuns(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC)
	s, c, app := newService(t, now)
	dead := startPass(t, s)
	saveRuns(t, s, app.ID, stored{id: "before-crash", pass: dead, signIn: claude.SignInAPIKey, agent: 1, finished: now.Add(20 * time.Minute)})
	c.now = now.Add(24 * time.Hour)
	if _, interrupted, err := s.DB.StartWatchPass(ctx, "schedule", "dev", c.now); err != nil || len(interrupted) != 1 {
		t.Fatalf("the next pass: %v, %v", interrupted, err)
	}
	// The dead pass's second run, stored by recovery from its start file during the next pass.
	saveRuns(t, s, app.ID, stored{id: "recovered", pass: dead, signIn: claude.SignInAPIKey, agent: 0.75, finished: now.Add(30 * time.Minute)})
	first, err := s.Ledger(ctx, claude.SignInAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Ledger(ctx, claude.SignInAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	if first.Week.Runs != 2 || !near(first.Week.USD(), 1.75) || first.Week != second.Week {
		t.Errorf("the ledger = %+v, then %+v", first.Week, second.Week)
	}
	use, err := s.PassUse(ctx, dead, false, 0)
	if err != nil || use.Runs != 2 || !near(use.USD(), 1.75) {
		t.Errorf("the dead pass's use = %+v, %v", use, err)
	}
}

// Adding a run never hides use already counted: over the five-hour reset scenario, in either order, every longer
// prefix of the runs measures at least as much in both units. The crossing run alone counts 0.04 after the reset
// (plus its estimate), and a later run that first reads 0.04 in the new window keeps it.
func TestAddingARunNeverHidesUse(t *testing.T) {
	now := time.Date(2026, 10, 2, 5, 0, 0, 0, time.UTC)
	a, b, seven := now.Add(-time.Hour), now.Add(4*time.Hour), now.Add(3*24*time.Hour)
	run := func(id string, first, last *claude.UsageReading) store.Run {
		return store.Run{ID: id, WatchPassID: 1, CostUSD: 1, Record: []byte(`{"sign_in":"login","metrics":{"usage_first":` + jsonOf(first) +
			`,"usage_last":` + jsonOf(last) + `}}`)}
	}
	r1 := run("r1", reading(0.10, a, 0.40, seven), reading(0.16, a, 0.41, seven))
	r2 := run("r2", reading(0.16, a, 0.41, seven), reading(0.22, a, 0.42, seven))
	r3 := run("r3", reading(0.22, a, 0.42, seven), reading(0.04, b, 0.43, seven))
	r4 := run("r4", reading(0.04, b, 0.43, seven), reading(0.10, b, 0.44, seven))
	const perRun = 0.06
	if got := Measure([]store.Run{r3}, true, perRun).Share.FiveHour; !near(got, 0.04+perRun) {
		t.Errorf("the crossing run alone = %v", got)
	}
	if got := Measure([]store.Run{r3, r4}, true, perRun).Share.FiveHour; !near(got, 0.10+perRun) {
		t.Errorf("the crossing run and the next = %v; want 0.10 after the reset, not 0.06", got)
	}
	for _, order := range [][]store.Run{{r1, r2, r3, r4}, {r4, r3, r2, r1}, {r3, r1, r4, r2}} {
		var last Share
		for k := 1; k <= len(order); k++ {
			sh := *Measure(order[:k], true, perRun).Share
			if sh.FiveHour < last.FiveHour-1e-12 || sh.SevenDay < last.SevenDay-1e-12 {
				t.Errorf("adding %s lowered the share: %+v to %+v", order[k-1].ID, last, sh)
			}
			last = sh
		}
		if !near(last.FiveHour, 0.28) {
			t.Errorf("all four runs = %v, want 0.28 in any order", last.FiveHour)
		}
	}
}

// One budget for all projects: the ledger sums the watch's runs in every project, so two projects together cannot
// spend more than one cap; the current windows come from any project's latest run, the user's own included.
func TestOneBudgetForAllProjects(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 5, 0, 0, 0, time.UTC)
	s, c, app := newService(t, now.Add(-3*time.Hour))
	other, err := s.DB.SaveProject(ctx, "/work/other", "other", []byte(`{}`), c.now)
	if err != nil {
		t.Fatal(err)
	}
	third, err := s.DB.SaveProject(ctx, "/work/third", "third", []byte(`{}`), c.now)
	if err != nil {
		t.Fatal(err)
	}
	first := startPass(t, s)
	c.now = now.Add(-time.Hour)
	second := startPass(t, s)
	five, seven := now.Add(2*time.Hour), now.Add(3*24*time.Hour)
	saveRuns(t, s, app.ID, stored{id: "a1", pass: first, signIn: claude.SignInLogin, agent: 8, finished: now.Add(-2 * time.Hour),
		first: reading(0.10, five, 0.01, seven), last: reading(0.20, five, 0.05, seven)})
	saveRuns(t, s, other.ID, stored{id: "b1", pass: second, signIn: claude.SignInLogin, agent: 9, finished: now.Add(-30 * time.Minute),
		first: reading(0.25, five, 0.06, seven), last: reading(0.35, five, 0.11, seven)})
	saveRuns(t, s, third.ID, stored{id: "user", signIn: claude.SignInLogin, agent: 2, finished: now.Add(-10 * time.Minute),
		first: reading(0.35, five, 0.11, seven), last: reading(0.45, five, 0.13, seven)})
	c.now = now
	l, err := s.Ledger(ctx, claude.SignInLogin)
	if err != nil {
		t.Fatal(err)
	}
	if l.Week.Runs != 2 || !near(l.Week.USD(), 17) || !near(l.Week.Share.FiveHour, 0.20) || !near(l.Week.Share.SevenDay, 0.09) {
		t.Errorf("the week across projects = %+v, %+v", l.Week, l.Week.Share)
	}
	room := l.Room(Consent{Caps: DefaultCaps()})
	if !near(room.USD, 3) || room.USD >= 2*DefaultCaps().RunCapUSD || !near(room.SevenDay, 0.06) {
		t.Errorf("the room = %+v; want $3 left, less than a pair's two run caps", room)
	}
	if w := l.Current; !w.FiveHourKnown || w.FiveHour != 0.45 || w.SevenDay != 0.13 {
		t.Errorf("the current windows = %+v; want the third project's latest reading", w)
	}
}
