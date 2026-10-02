package watch

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
)

// clock is a settable fake clock.
type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func newService(t *testing.T, now time.Time) (Service, *clock, store.Project) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app, err := db.SaveProject(context.Background(), "/work/app", "app", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{now: now}
	return Service{DB: db, Now: c.Now}, c, app
}

// stored describes a run to store: its pass (0: not the watch's), spend, sign-in and readings.
type stored struct {
	id          string
	pass        int64
	signIn      string
	agent       float64
	judge       float64
	finished    time.Time
	first, last *claude.UsageReading
}

func saveRuns(t *testing.T, s Service, projectID int64, runs ...stored) {
	t.Helper()
	for _, r := range runs {
		rec := run.Record{ID: r.id, Task: "t", Arm: "A", SignIn: r.signIn, Outcome: "ok",
			Metrics: claude.Metrics{CostUSD: r.agent, UsageFirst: r.first, UsageLast: r.last}}
		if r.judge > 0 {
			rec.Judge = &judge.Verdict{CostUSD: r.judge}
		}
		encoded, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.DB.SaveRun(context.Background(), store.Run{ID: r.id, ProjectID: projectID, TaskName: "t", Arm: "A", Outcome: "ok",
			CostUSD: rec.Spend().AgentUSD, Record: encoded, Started: r.finished.Add(-10 * time.Minute), Finished: r.finished,
			WatchPassID: r.pass}); err != nil {
			t.Fatal(err)
		}
	}
}

// reading is a usage reading: the five-hour share and reset, and the seven-day share and reset.
func reading(fiveHour float64, fiveResets time.Time, sevenDay float64, sevenResets time.Time) *claude.UsageReading {
	return &claude.UsageReading{FiveHour: fiveHour, FiveHourResets: fiveResets, SevenDay: sevenDay, SevenDayResets: sevenResets, Status: "allowed"}
}

func startPass(t *testing.T, s Service) int64 {
	t.Helper()
	pass, _, err := s.DB.StartWatchPass(context.Background(), "schedule", "dev", s.Now())
	if err != nil {
		t.Fatal(err)
	}
	return pass.ID
}

func near(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}
