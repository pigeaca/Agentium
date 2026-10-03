package cli

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/term"
)

// The steps of a run as the dashboard shows them, in order: a fresh copy of the repository, Claude Code at work, the
// hidden tests, and the result.
const (
	stepCopy = iota
	stepAgent
	stepTests
	stepResult
	stepCount
)

// dotTime is how long the dot takes to travel from a finished step's box to the next one's.
const dotTime = time.Second

// logRows is the dashboard's log: the last 4 runs' results, in a fixed area. The state keeps every line of the
// execution for the scrollback at the end.
const logRows = 4

// stateRun is one run as the screens follow it: its task and how far it got.
type stateRun struct {
	pos, arm, attempt int
	task              string // sanitized
	step              int    // the step in progress: stepResult once finished
	reached           int    // the last step that began, once finished (a run that was not graded never reached the tests)
	began             [stepCount]time.Time
	took              [stepCount]time.Duration
	moved             time.Time // when the last step ended: the dot travels from step-1 to step until dotTime after it
	started           time.Time
	judging, finished bool
	requeued          bool
	result            experiment.Result
}

// runState is what the designed views know of a running experiment: the facts from its lock, and what the scheduler's
// events told so far. The scheduler reports from several goroutines (one event at a time, but the dashboard's frames
// read it from the display's writer goroutine), so it has its own lock; frames draw from copies (view).
type runState struct {
	mu       sync.Mutex
	facts    runFacts
	now      func() time.Time
	runs     map[int]*stateRun // in flight, by schedule position
	last     [2]*stateRun      // each arm's run that finished last
	settled  map[int]bool
	spent    float64
	usage    claude.UsageReading
	hasUsage bool
	until    time.Time // waiting for the plan's usage to reset
	answer   answerState
	log      []logEntry
	// note is the dashboard's status line: the latest retry, pause, warning or comparison, in words, until it no longer
	// applies (a retry's slot or a paused execution starts again) or, for the rest, noteTime after it came. notePos is a
	// retry's slot, else -1; noteAt when it came.
	note     string
	noteRole term.Role
	notePos  int
	noteAt   time.Time
}

// noteTime is how long a note that nothing else ends (a comparison, a warning) stays in the status line.
const noteTime = 10 * time.Second

func newRunState(f runFacts, s experiment.Standing, now func() time.Time) *runState {
	st := &runState{facts: f, now: now, runs: map[int]*stateRun{}, settled: map[int]bool{}, spent: s.Spent, usage: s.Usage, hasUsage: s.HasUsage,
		notePos: -1}
	for pos := range s.Settled {
		st.settled[pos] = true
	}
	st.answer = answerState{Metric: experiment.MetricCost, Margin: f.margin, All: f.tasks, Seq: f.looks != nil}
	if f.goal == experiment.GoalBetter && f.looks == nil {
		st.answer.Metric = experiment.MetricSuccess
	}
	if len(f.looks) > 0 {
		st.answer.Next = f.looks[0]
	}
	return st
}

// apply takes in a scheduler event and returns the log entries it adds (none for a step), and whether the answer
// changed (a check of a seq-v1 experiment).
func (s *runState) apply(e experiment.Event) (added []logEntry, checked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	arm := s.armOf(e.Slot)
	label := s.facts.labels[arm]
	taskName := term.Sanitize(e.Slot.Task)
	sentence := func(role term.Role, text string) {
		added = append(added, logEntry{at: now, arm: -1, words: text, role: role, sentence: true})
		s.note, s.noteRole, s.notePos, s.noteAt = text, role, -1, now
	}
	switch e.Kind {
	case "start":
		if !s.until.IsZero() || s.notePos == e.Slot.Position {
			s.note = ""
		}
		s.until = time.Time{}
		s.spent = max(s.spent, e.SpentUSD)
		r := &stateRun{pos: e.Slot.Position, arm: arm, attempt: e.Attempt, task: taskName, started: now}
		r.began[stepCopy] = now
		s.runs[r.pos] = r
	case "step":
		r := s.runs[e.Slot.Position]
		if r == nil || r.attempt != e.Attempt || r.finished {
			break
		}
		switch e.Step {
		case run.StepAgent:
			r.advance(stepAgent, now)
		case run.StepGrading:
			r.advance(stepTests, now)
		case run.StepJudging:
			r.advance(stepResult, now)
			r.judging = true
		}
	case "finish":
		r := s.runs[e.Slot.Position]
		if r == nil || r.attempt != e.Attempt {
			r = &stateRun{pos: e.Slot.Position, arm: arm, attempt: e.Attempt, task: taskName, started: now}
		}
		delete(s.runs, r.pos)
		r.finish(e.Result, e.Requeued, now)
		s.last[arm] = r
		s.spent = e.SpentUSD
		if experiment.Settles(e.Result.Outcome) {
			s.settled[e.Slot.Position] = true
		}
		if u := e.Result.Usage; u != nil {
			s.read(*u)
		}
		entry := logEntry{at: now, arm: arm, task: taskName, result: e.Result, requeued: e.Requeued, took: now.Sub(r.started)}
		if e.Result.Judge != "" {
			entry.judge = term.Sanitize(e.Result.Judge)
		}
		added = append(added, entry)
		if e.Result.Overshoot != "" {
			sentence(term.LevelCaution, "warning: "+label+" · "+taskName+": "+term.Sanitize(e.Result.Overshoot))
		}
	case "retry":
		s.spent = max(s.spent, e.SpentUSD)
		sentence(term.OutcomeInfra, fmt.Sprintf("retrying %s for %s in %s: it failed, not by Claude's doing", taskName, label, term.Elapsed(e.RetryIn)))
		s.notePos = e.Slot.Position
	case "wait":
		s.until = e.Until
		s.read(claude.UsageReading{FiveHour: e.Usage, FiveHourResets: e.Until})
		sentence(term.LevelCaution, fmt.Sprintf("waiting for your Claude plan's usage to reset at %s (%.0f%% used); Ctrl-C stops, and running it again goes on",
			experiment.Clock(e.Until, now), 100*e.Usage))
	case "look":
		if e.Look == nil {
			break
		}
		prev := s.answer
		s.answer = answerOfLook(*e.Look, s.facts.looks, s.facts.tasks)
		s.answer.Margin, s.answer.Ended = s.facts.margin, prev.Ended
		if !e.Look.Analysed && prev.Verdict != "" { // no task counted since: the last analysed check's answer stands, as in the report
			s.answer.Verdict, s.answer.Estimate, s.answer.HasEstimate = prev.Verdict, prev.Estimate, prev.HasEstimate
		}
		headline, _ := answerWords(s.answer, s.facts.labels, s.facts.aa)
		entry := logEntry{at: now, arm: -1, role: term.OutcomeOK, sentence: true, check: true,
			words: fmt.Sprintf("checked the answer after %d tasks: %s", e.Look.Planned, headline)}
		added, checked = append(added, entry), true
	case "pair":
		s.spent += e.Result.JudgeUSD
		sentence(term.Muted, fmt.Sprintf("compared the two passing runs of %s: %s, $%.2f (a second opinion)", taskName,
			pairWords(term.Sanitize(e.Result.Judge), s.facts.labels), e.Result.JudgeUSD))
	}
	s.log = append(s.log, added...)
	return added, checked
}

// pairWords names the arms a pair's comparison prefers by their labels: "prefers A" is "prefers baseline".
func pairWords(words string, labels [2]string) string {
	for i, arm := range []string{"A", "B"} {
		if rest, ok := strings.CutPrefix(words, "prefers "+arm); ok && (rest == "" || rest[0] == ';') {
			return "prefers " + labels[i] + rest
		}
	}
	return words
}

// armOf is the arm of a slot: 0 or 1.
func (s *runState) armOf(slot experiment.Slot) int {
	if i, ok := s.facts.arms[slot.Arm]; ok {
		return i
	}
	if slot.Position >= 0 && slot.Position < len(s.facts.slotArm) {
		return s.facts.slotArm[slot.Position]
	}
	return 0
}

// read keeps u if it is later than the reading kept so far; s.mu is held.
func (s *runState) read(u claude.UsageReading) {
	if !s.hasUsage || u.Newer(s.usage) {
		s.usage, s.hasUsage = u, true
	}
}

// end records how the execution ended, and for a fixed design that is done, its answer (nil when it could not be
// read). It returns the answer.
func (s *runState) end(sum experiment.Summary, final *answerState) answerState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if final != nil {
		s.answer = *final
		s.answer.Margin = s.facts.margin
	}
	s.answer.Ended = sum.Status
	return s.answer
}

// entries is every log line so far.
func (s *runState) entries() []logEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]logEntry(nil), s.log...)
}

// advance ends the step in progress and begins step to at now: the dot then travels to it.
func (r *stateRun) advance(to int, now time.Time) {
	if to <= r.step {
		return
	}
	r.took[r.step] = now.Sub(r.began[r.step])
	r.step, r.moved = to, now
	r.began[to] = now
}

// finish ends the run with its result: the step in progress ends, and the steps it never reached are skipped.
func (r *stateRun) finish(res experiment.Result, requeued bool, now time.Time) {
	r.reached = r.step
	if r.step < stepResult {
		r.took[r.step] = now.Sub(r.began[r.step])
		r.step, r.moved = stepResult, now
	} else {
		r.reached = stepTests
	}
	r.finished, r.judging, r.result, r.requeued = true, false, res, requeued
}

// view copies what a frame draws, at now.
func (s *runState) view() stateView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := stateView{facts: s.facts, spent: s.spent, usage: s.usage, hasUsage: s.hasUsage, until: s.until, answer: s.answer,
		note: s.note, noteRole: s.noteRole, noteFades: s.notePos < 0, noteAt: s.noteAt}
	for pos := range s.settled {
		if pos >= 0 && pos < len(s.facts.slotArm) {
			v.settled[s.facts.slotArm[pos]]++
		}
	}
	for arm := range 2 {
		var shown *stateRun
		for _, r := range s.runs { // the latest run of the arm in flight (the concurrency may run several)
			if r.arm == arm && (shown == nil || r.started.After(shown.started) || r.started.Equal(shown.started) && r.pos > shown.pos) {
				shown = r
			}
		}
		if shown == nil {
			shown = s.last[arm]
		}
		if shown != nil {
			v.shown[arm], v.have[arm] = *shown, true
		}
	}
	for i := len(s.log) - 1; i >= 0 && len(v.log) < logRows; i-- { // the latest runs' results, oldest first
		if !s.log[i].sentence {
			v.log = append([]logEntry{s.log[i]}, v.log...)
		}
	}
	return v
}

// stateView is a copy of the state for one frame.
type stateView struct {
	facts     runFacts
	shown     [2]stateRun
	have      [2]bool
	settled   [2]int
	spent     float64
	usage     claude.UsageReading
	hasUsage  bool
	until     time.Time
	answer    answerState
	log       []logEntry // the last logRows runs' results
	note      string
	noteRole  term.Role
	noteFades bool // the note goes noteTime after noteAt (a retry's lasts until its run starts again)
	noteAt    time.Time
}

// noteNow is the status line's note at now: empty once it has faded.
func (v stateView) noteNow(now time.Time) string {
	if v.noteFades && now.Sub(v.noteAt) >= noteTime {
		return ""
	}
	return v.note
}
