package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/mine"
	"github.com/pigeaca/agentium/internal/stats"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
)

// arms are the experiment's two arms, resolved from the contexts start chose.
func (s *starter) arms(ctx context.Context) ([]experiment.Arm, error) {
	p := s.w.service()
	a, err := p.ResolveArm(ctx, "A", s.a)
	if err != nil {
		return nil, err
	}
	other := s.a // an A/A runs one context in both arms
	if s.b != "" {
		other = s.b
	}
	b, err := p.ResolveArm(ctx, "B", other)
	if err != nil {
		return nil, err
	}
	return []experiment.Arm{a, b}, nil
}

// taskCounts sorts the project's tasks for start: ready ones can be in the experiment, waiting ones are valid but
// their instructions still need a review for solution leaks, pending ones have not been validated in every arm's
// context yet.
type taskCounts struct {
	ready, waiting []string
	pending        []store.Task
}

func (s *starter) countTasks(ctx context.Context, attempted map[string]bool) (taskCounts, error) {
	var c taskCounts
	arms, err := s.arms(ctx)
	if err != nil {
		return c, err
	}
	p := s.w.service()
	if c.ready, _, err = p.EligibleTasks(ctx, arms, defaultGrader(s.env)); err != nil {
		return c, err
	}
	tasks, err := s.w.db.Tasks(ctx, s.w.project.ID)
	if err != nil {
		return c, err
	}
	for _, t := range tasks {
		if t.Grading == task.GradingJudge || t.Retired() { // a retired task is never validated, reviewed or counted
			continue
		}
		v := task.ValidationOf(t)
		cand := experiment.Candidate{Name: t.Name, Grading: t.Grading, Validation: &v}
		switch {
		case t.Validation == nil && !attempted[t.Name]:
			c.pending = append(c.pending, t)
		case t.Validation == nil:
		case v.Status == task.StatusValid && experiment.Ineligible(cand, arms, defaultGrader(s.env)) == "" && t.NeedsReview:
			c.waiting = append(c.waiting, t.Name)
		case v.Status == task.StatusValid && experiment.Ineligible(cand, arms, defaultGrader(s.env)) != "" && !attempted[t.Name]:
			c.pending = append(c.pending, t) // valid in the base only: the arms' contexts are missing
		}
	}
	return c, nil
}

// reachable counts the tasks that are ready or that start's own review gate is waiting for: the ready ones and the
// waiting ones start itself mined (with --accept-mined, only those its checks did not hold back). Tasks the user
// brought in (task import, pull requests, tickets) and left waiting are theirs to review: they never hold start back.
func (s *starter) reachable(c taskCounts) int {
	n := len(c.ready)
	for _, name := range c.waiting {
		if s.mined[name] && (!s.args.acceptMined || s.held[name] == "") {
			n++
		}
	}
	return n
}

// supplyTasks mines and validates until the cost experiment's most tasks (stats.SeqMaxTasks, 16) are ready, or the
// candidates run out. It reports whether enough are ready: the maximum, or, once nothing more can be mined or validated,
// the cost floor (8) or more, which gives fewer looks. Tasks waiting for a review hold it back while fewer than the
// maximum are ready: they become ready once reviewed (or removed), so start says so and stops; with --accept-mined only
// the ones its checks held back stay waiting, and they do not hold it back. When too few are ready, it says why and
// what to do.
func (s *starter) supplyTasks(ctx context.Context) (bool, error) {
	floor, target := experiment.FloorsFor(experiment.MethodVersion).CostTasks, stats.SeqMaxTasks
	out, started := s.env.Stdout, s.env.Now()
	if err := s.loadMined(ctx); err != nil { // only this stage needs it: a corrupt file must not block resuming
		return false, err
	}
	attempted, exhausted, worked := map[string]bool{}, false, false
	for {
		if s.args.acceptMined {
			names, err := s.acceptMined(ctx)
			if err != nil {
				return false, err
			}
			if len(names) > 0 {
				worked = true
				fmt.Fprintf(out, "Accepted %d mined instruction(s) without your review (--accept-mined): %s\n"+
					"  Only solution headings, reference-file names and unstated test requirements were checked; a message that explains the fix is not detected.\n",
					len(names), strings.Join(names, ", "))
			}
		}
		c, err := s.countTasks(ctx, attempted)
		if err != nil {
			return false, err
		}
		s.counts = &c
		took := " (skipped)"
		if worked {
			took = ", in " + s.env.Now().Sub(started).Round(time.Second).String()
		}
		switch {
		case len(c.ready) >= target:
			fmt.Fprintf(out, "Tasks: %d ready (aims for %d)%s\n", len(c.ready), target, took)
			return true, nil
		case ctx.Err() != nil:
			fmt.Fprintf(out, "Interrupted: what was validated is kept; %s resumes\n", s.env.style().Command("agentium start"))
			return false, errReported
		case len(c.pending) > 0:
			worked = true
			if err := s.validate(ctx, c.pending, attempted); err != nil {
				return false, err
			}
		case s.reachable(c) < target && !exhausted && s.stopped == "":
			worked = true
			room := maxMineFactor*target - s.imported
			if room <= 0 {
				s.stopped = fmt.Sprintf("stopped mining after %d imported task(s), %d times the experiment's %d", s.imported, maxMineFactor, target)
				continue
			}
			if exhausted, err = s.mineMore(ctx, min(target-s.reachable(c), room)); err != nil {
				return false, err
			}
		case len(c.ready) >= floor && s.reachable(c) == len(c.ready):
			why := s.stopped
			if why == "" {
				why = "the history has no more candidates"
			}
			fmt.Fprintf(out, "Tasks: %d ready (aims for %d; %s)%s: the experiment takes them all, %s\n", len(c.ready), target, why, took,
				experiment.DescribeLooks(experiment.Design{Tasks: c.ready}))
			return true, nil
		default: // fewer than the floor are ready, or start's own mined tasks wait for a review that would add to them
			s.awaitingReview = len(c.waiting) > 0
			s.explainShortage(c, floor, target, exhausted)
			return false, nil
		}
	}
}

// validate validates tasks in the base context and in each experiment context, a few at a time, and says how it went.
func (s *starter) validate(ctx context.Context, tasks []store.Task, attempted map[string]bool) error {
	var snaps []string
	for _, name := range []string{s.a, s.b} {
		if name != "" && (len(snaps) == 0 || snaps[0] != name) {
			snaps = append(snaps, name)
		}
	}
	arms, err := s.w.validating(s.env, nil, 0, "").Arms(ctx, s.w.project.ID, snaps)
	if err != nil {
		return err
	}
	settings := s.settings()
	jobs := jobsOf(settings)
	fmt.Fprintf(s.env.Stdout, "Validating %d task(s) in %d context(s), %d at a time\n", len(tasks), len(arms), jobs)
	began := s.env.Now()
	quiet := s.env
	quiet.Stdout = io.Discard // the batch's own table is long; the summary below is what start shows
	results, err := validateBatch(ctx, quiet, s.w, tasks, task.ValidateOptions{Arms: arms, Repeat: 1, Timeout: verifyTimeoutOf(settings), Grader: defaultGrader(s.env)}, jobs)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	var bad []string
	mined, minedValid := 0, 0 // of the tasks this run's mining imported
	for _, r := range results {
		attempted[r.Task.Name] = true
		status := "not validated"
		if r.Validated {
			status = task.ValidationOf(r.Task).Status
		}
		counts[status]++
		if s.importedNow[r.Task.Name] {
			mined++
			if status == task.StatusValid {
				minedValid++
			}
		}
		if status != task.StatusValid {
			if p := r.Problem(); p != "" {
				status = p
			}
			bad = append(bad, r.Task.Name+": "+status)
		}
	}
	line := fmt.Sprintf("  %d valid of %d, in %s", counts[task.StatusValid], len(results), s.env.Now().Sub(began).Round(time.Second))
	if len(bad) > 0 {
		line += "; set aside: " + strings.Join(bad, ", ")
	}
	fmt.Fprintln(s.env.Stdout, line)
	// Only tasks this run mined say anything about mining: a broken task someone added by hand does not stop it. A round
	// of one or two tasks says too little on its own (mining then tops up the last few, best candidates first, so an
	// unlucky pick would stop it short), so small rounds add up: mining stops once the tasks mined in this run since its
	// last valid one reach minStopSample, all invalid, in one round or across several. The maxMineFactor cap still
	// bounds the rounds.
	switch {
	case mined == 0:
	case minedValid > 0:
		s.invalidStreak = 0
	default:
		s.invalidStreak += mined
	}
	switch {
	case mined >= minStopSample && minedValid == 0:
		s.stopped = fmt.Sprintf("none of the %d tasks just mined is valid, so mining more would likely repeat that", mined)
	case s.invalidStreak >= minStopSample:
		s.stopped = fmt.Sprintf("the last %d tasks mined are all invalid, so mining more would likely repeat that", s.invalidStreak)
	}
	return nil
}

// mineMore imports up to want more candidates from the history as tasks (unvalidated); exhausted is whether the
// history has no more to give. With the project's module set it mines that module's commits (mine.Options.Module), into
// tasks that record it.
func (s *starter) mineMore(ctx context.Context, want int) (exhausted bool, err error) {
	w, out, settings := s.w, s.env.Stdout, s.settings()
	prep, err := mine.Prepare(ctx, mine.PrepareInput{DB: w.db, ProjectID: w.project.ID, Root: w.root,
		Options: mine.Options{MaxFiles: mine.DefaultMaxFiles, MaxLines: mine.DefaultMaxLines, RequireLock: settings.RequireLock, Module: settings.Module},
		Verify:  settings.Verify, DefaultVerify: w.defaultVerify()}) // the module's detected commands, with a module
	if err != nil {
		return false, err
	}
	s.lastScan = &prep.Result
	candidates := slices.DeleteFunc(slices.Clone(prep.Result.Candidates), func(c mine.Candidate) bool { return s.dismissed[c.Hash] })
	found := len(candidates)
	if found == 0 {
		fmt.Fprintf(out, "Mining: no more candidates in %d commit(s) read%s", prep.Result.Scanned, inModule(settings.Module))
		if n := prep.Result.Counts()[mine.ReasonUnlocked]; settings.RequireLock && n > 0 {
			// The setting alone can empty a Python project's history: say so, not just "no more".
			fmt.Fprintf(out, "; the require-lock setting set aside %d Python commit(s) whose base pins no dependencies "+
				"(agentium init --require-lock=false mines them)", n)
		}
		fmt.Fprintln(out)
		return true, nil
	}
	imp := mine.Import(ctx, mine.ImportInput{Importer: w.importer(prep.Names), Candidates: candidates, Limit: want,
		NewTask: func() store.Task {
			return store.Task{ProjectID: w.project.ID, Verify: prep.Verify, Setup: append([]string{}, settings.Setup...), Module: settings.Module, CreatedAt: s.env.Now()}
		}})
	fmt.Fprintf(out, "Mining: %d candidate(s) in %d commit(s) read%s; imported %d of %d tried (verify: %s)\n", found, prep.Result.Scanned, inModule(settings.Module),
		len(imp.Tasks), imp.Tried, strings.Join(prep.Verify, "; "))
	for _, f := range imp.Failed {
		fmt.Fprintf(out, "  not imported: %s (%s): %v\n", cut(f.Candidate.Subject, maxSubject), experiment.ShortCommit(f.Candidate.Hash), f.Err)
	}
	s.imported += len(imp.Tasks)
	// What was imported is recorded even after Ctrl-C: the records are what keeps a task the user removes from being
	// mined again and accepted unread.
	if err := s.recordMined(context.WithoutCancel(ctx), imp.Tasks); err != nil {
		return false, err
	}
	if imp.Interrupted {
		fmt.Fprintf(out, "Interrupted: %d task(s) imported are kept; %s resumes\n", len(imp.Tasks), s.env.style().Command("agentium start"))
		return false, errReported
	}
	if len(imp.Failed) > 0 && len(imp.Tasks) < want {
		// Every candidate was tried, but some failed to import: that is not an exhausted history, and the user should see why.
		s.stopped = fmt.Sprintf("%d candidate(s) could not be imported (listed above)", len(imp.Failed))
		return false, nil
	}
	return len(imp.Tasks) < want, nil // fewer imported than asked: every candidate was tried
}

// maxMineFactor caps how many tasks start imports, as a multiple of the experiment's most tasks, when many fail
// validation.
const maxMineFactor = 3

// minStopSample is how many tasks mined in a row, in one round or across several, all invalid, stop start from mining
// more.
const minStopSample = 3

// acceptMined marks waiting tasks that start itself mined (now or in an earlier run: minedFile) as reviewed when the
// automatic checks find nothing: no section that may give the solution away, no reference-file name in the
// instruction, and no requirement of the hidden tests that nothing states. A task from a pull request, a ticket or
// `task import` is never accepted, nor is one validation set aside (invalid, flaky, unchecked). It returns the accepted
// names and leaves the reasons it held back others in s.held; a mined instruction that explains the fix in plain words
// passes these checks, which is why the flag is opt-in.
func (s *starter) acceptMined(ctx context.Context) ([]string, error) {
	tasks, err := s.w.db.Tasks(ctx, s.w.project.ID)
	if err != nil {
		return nil, err
	}
	fair := task.NewFairness("--git-dir", s.w.bare)
	s.held = map[string]string{}
	var accepted []string
	for _, t := range tasks {
		if !s.mined[t.Name] || !t.NeedsReview || t.SolutionCommit == "" || t.Grading == task.GradingJudge || t.Validation == nil {
			continue
		}
		if task.ValidationOf(t).Status != task.StatusValid {
			continue // set aside by validation (listed by validate's "set aside:" line): never accepted
		}
		if reason, err := heldBack(ctx, fair, t); err != nil {
			return accepted, err
		} else if reason != "" {
			s.held[t.Name] = reason
			continue
		}
		t.NeedsReview = false
		if err := s.w.db.UpdateTask(ctx, t, s.env.Now()); err != nil {
			return accepted, err
		}
		accepted = append(accepted, t.Name)
	}
	return accepted, nil
}

// heldBack is why a mined task's instruction is not accepted without a person: "" when the checks find nothing.
func heldBack(ctx context.Context, fair *task.Fairness, t store.Task) (string, error) {
	if sections := task.SolutionSections(t.Instruction); len(sections) > 0 {
		return "the instruction has sections that may give the solution away: " + strings.Join(sections, ", "), nil
	}
	if p := namedReferenceFile(t); p != "" {
		return "the instruction names reference file " + p, nil
	}
	gaps, err := task.Gaps(ctx, fair, t)
	if err != nil {
		return "", err
	}
	if len(gaps) > 0 {
		texts := make([]string, len(gaps))
		for i, g := range gaps {
			texts[i] = g.String()
		}
		return fmt.Sprintf("%d requirement(s) of the hidden tests that nothing states: %s", len(gaps), strings.Join(texts, "; ")), nil
	}
	return "", nil
}

// namedReferenceFile is a file of the reference solution that the instruction names, which tells the agent where the
// fix goes (the check `task show` makes); "" when it names none.
func namedReferenceFile(t store.Task) string {
	for _, p := range t.Reference {
		if strings.Contains(t.Instruction, p) || strings.Contains(t.Instruction, filepath.Base(p)) {
			return p
		}
	}
	return ""
}

// minedRecord is one task start mined, with enough to tell it from a task someone made later under the same name:
// the commit it solves and when it was created.
type minedRecord struct {
	Name           string    `json:"name"`
	SolutionCommit string    `json:"solution_commit"`
	CreatedAt      time.Time `json:"created_at"`
}

// minedState is the file start keeps beside the project's repository (not task data, so no migration).
//
// Rule for removed tasks: a record whose task is gone, or whose name now belongs to a different task (another commit or
// creation time), is dropped, and its commit is dismissed for good: start never mines it again, so it can neither
// return to be accepted unread after the user removed it, nor be accepted when the user imported it by hand.
type minedState struct {
	Mined     []minedRecord `json:"mined"`
	Dismissed []string      `json:"dismissed"`
}

func (s *starter) minedFile() string {
	return filepath.Join(filepath.Dir(s.w.bare), "start-mined.json")
}

// loadMined reads the state and checks every record against the project's tasks. An unreadable file costs only the
// --accept-mined shortcut: it is an error with that flag (nothing is accepted on a guess), and a warning without it.
func (s *starter) loadMined(ctx context.Context) error {
	s.mined, s.dismissed, s.records = map[string]bool{}, map[string]bool{}, nil
	state, err := s.readMined()
	switch {
	case err != nil && s.args.acceptMined:
		return fmt.Errorf("%s is unreadable, so --accept-mined cannot tell which tasks start mined: %w (delete it to start over)", s.minedFile(), err)
	case err != nil:
		fmt.Fprintf(s.env.Stderr, "agentium: %s is unreadable and ignored: %v\n", s.minedFile(), err)
		return nil
	}
	tasks, err := s.w.db.Tasks(ctx, s.w.project.ID)
	if err != nil {
		return err
	}
	for _, c := range state.Dismissed {
		s.dismissed[c] = true
	}
	for _, rec := range state.Mined {
		i := slices.IndexFunc(tasks, func(t store.Task) bool {
			return t.Name == rec.Name && t.SolutionCommit == rec.SolutionCommit && t.CreatedAt.Equal(rec.CreatedAt)
		})
		if i < 0 {
			s.dismissed[rec.SolutionCommit] = true
			continue
		}
		s.mined[rec.Name] = true
		s.records = append(s.records, rec)
	}
	return nil
}

// readMined reads the state file; a missing file is an empty state.
func (s *starter) readMined() (minedState, error) {
	var state minedState
	data, err := os.ReadFile(s.minedFile())
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	} else if err != nil {
		return state, err
	}
	return state, json.Unmarshal(data, &state)
}

// recordMined adds the tasks just imported (as stored, so their creation time matches) and writes the state.
func (s *starter) recordMined(ctx context.Context, imported []store.Task) error {
	tasks, err := s.w.db.Tasks(ctx, s.w.project.ID)
	if err != nil {
		return err
	}
	for _, imp := range imported {
		if i := slices.IndexFunc(tasks, func(t store.Task) bool { return t.Name == imp.Name }); i >= 0 {
			s.records = append(s.records, minedRecord{Name: imp.Name, SolutionCommit: tasks[i].SolutionCommit, CreatedAt: tasks[i].CreatedAt.UTC()})
			s.mined[imp.Name] = true
			s.importedNow[imp.Name] = true
		}
	}
	return s.saveMined()
}

// saveMined merges this run's records into the file under a lock, so two starts at once keep each other's records,
// then writes it through a temporary file of its own in the same folder and renames it into place. A file that cannot
// be read is replaced by this run's state.
func (s *starter) saveMined() error {
	lock, err := os.OpenFile(s.minedFile()+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close() // closing releases the flock
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s: %w", s.minedFile(), err)
	}
	state := minedState{Mined: s.records, Dismissed: slices.Sorted(maps.Keys(s.dismissed))}
	if disk, err := s.readMined(); err == nil {
		for _, rec := range disk.Mined {
			if !slices.Contains(state.Mined, rec) {
				state.Mined = append(state.Mined, rec)
			}
		}
		state.Dismissed = slices.Compact(slices.Sorted(slices.Values(append(state.Dismissed, disk.Dismissed...))))
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.minedFile()), "start-mined-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // after a successful rename it is gone already
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.minedFile())
}

// explainShortage says how many tasks start found and what to do next: fewer than the floor are ready, or tasks wait
// for a review that would bring the experiment closer to its target.
func (s *starter) explainShortage(c taskCounts, floor, target int, exhausted bool) {
	out, st := s.env.Stdout, s.env.style()
	if len(c.waiting) > 0 {
		fmt.Fprintf(out, "Tasks: %s\n", st.Warn(fmt.Sprintf("%d valid, %d ready: the others wait for your review (an experiment aims for %d tasks and needs at least %d)",
			len(c.ready)+len(c.waiting), len(c.ready), target, floor)))
		fmt.Fprintf(out, "  Read each instruction for solution leaks: %s, then %s (or remove one you will not accept: %s)\n", st.Command("agentium task show NAME"),
			st.Command("agentium task edit NAME --reviewed"), st.Command("agentium task rm NAME"))
		if !s.args.acceptMined {
			fmt.Fprintf(out, "  (or %s accepts the ones start mined without your review, after automatic checks that miss an instruction explaining the fix)\n", st.Command("agentium start --accept-mined"))
			fmt.Fprintf(out, "  waiting: %s\n", strings.Join(c.waiting, ", "))
		}
		for _, name := range c.waiting {
			switch {
			case !s.args.acceptMined:
			case s.held[name] != "":
				fmt.Fprintf(out, "  held back from --accept-mined: %s: %s\n", name, s.held[name])
			default:
				fmt.Fprintf(out, "  not accepted by --accept-mined, which takes only what start mined: %s\n", name)
			}
		}
	} else {
		fmt.Fprintf(out, "Tasks: %s\n", st.Bad(fmt.Sprintf("only %d of the %d an experiment needs are ready", len(c.ready), floor)))
	}
	if s.stopped != "" && s.reachable(c) < floor {
		fmt.Fprintf(out, "  %s: look at the tasks set aside above (%s shows each task's status), or add tasks with %s\n", s.stopped,
			st.Command("agentium task list"), st.Command("agentium task import --commit REF"))
	}
	if exhausted && s.reachable(c) < floor {
		fmt.Fprintf(out, "  the history has no more candidates%s; add tasks with %s or %s, then run %s again "+
			"(%s lists the pool's candidates: commits since its last pass, within 270 days)\n", s.setAsideSummary(),
			st.Command("agentium task add"), st.Command("agentium task import --commit REF"), st.Command("agentium start"),
			st.Command("agentium pool update --dry-run"))
	}
}

// inModule names a module for a mining line: " in svc/billing", or "" for the whole repository.
func inModule(module string) string {
	if module == "" {
		return ""
	}
	return " in " + module
}

// setAsideSummary says why start's last scan, over the whole history, set commits aside, per reason in the order the
// scan checks them: " (12 commit(s) read; set aside: merge commit 3, tests only 2)"; "" without a scan.
func (s *starter) setAsideSummary() string {
	if s.lastScan == nil {
		return ""
	}
	var parts []string
	for _, r := range rejections(*s.lastScan) {
		parts = append(parts, fmt.Sprintf("%s %d", r.reason, r.count))
	}
	summary := fmt.Sprintf(" (%d commit(s) read", s.lastScan.Scanned)
	if len(parts) > 0 {
		summary += "; set aside: " + strings.Join(parts, ", ")
	}
	return summary + ")"
}
