package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/mine"
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
	if c.ready, _, err = p.EligibleTasks(ctx, arms); err != nil {
		return c, err
	}
	tasks, err := s.w.db.Tasks(ctx, s.w.project.ID)
	if err != nil {
		return c, err
	}
	for _, t := range tasks {
		if t.Grading == task.GradingJudge {
			continue
		}
		v := task.ValidationOf(t)
		cand := experiment.Candidate{Name: t.Name, Grading: t.Grading, Validation: &v}
		switch {
		case t.Validation == nil && !attempted[t.Name]:
			c.pending = append(c.pending, t)
		case t.Validation == nil:
		case v.Status == task.StatusValid && experiment.Ineligible(cand, arms) == "" && t.NeedsReview:
			c.waiting = append(c.waiting, t.Name)
		case v.Status == task.StatusValid && experiment.Ineligible(cand, arms) != "" && !attempted[t.Name]:
			c.pending = append(c.pending, t) // valid in the base only: the arms' contexts are missing
		}
	}
	return c, nil
}

// supplyTasks mines and validates until the cost floor of tasks can be in the experiment, or the candidates run out.
// It reports whether the floor is met; when it is not, it says why and what to do.
func (s *starter) supplyTasks(ctx context.Context) (bool, error) {
	floor := experiment.FloorsFor(experiment.MethodVersion).CostTasks
	out, started := s.env.Stdout, s.env.Now()
	attempted, exhausted, worked := map[string]bool{}, false, false
	for {
		if s.args.reviewed {
			n, err := s.acceptReviews(ctx)
			if err != nil {
				return false, err
			}
			if n > 0 {
				worked = true
				fmt.Fprintf(out, "Reviewed: accepted %d instruction(s) that show no solution sections and state every requirement of their tests (--reviewed)\n", n)
			}
		}
		c, err := s.countTasks(ctx, attempted)
		if err != nil {
			return false, err
		}
		switch {
		case len(c.ready) >= floor:
			if worked {
				fmt.Fprintf(out, "Tasks: %d ready (needs %d), in %s\n", len(c.ready), floor, s.env.Now().Sub(started).Round(time.Second))
			} else {
				fmt.Fprintf(out, "Tasks: %d ready (needs %d) (skipped)\n", len(c.ready), floor)
			}
			return true, nil
		case ctx.Err() != nil:
			fmt.Fprintf(out, "Interrupted: what was validated is kept; %s resumes\n", s.env.style().Command("agentium start"))
			return false, errReported
		case len(c.pending) > 0:
			worked = true
			if err := s.validate(ctx, c.pending, attempted); err != nil {
				return false, err
			}
		case len(c.ready)+len(c.waiting) < floor && !exhausted:
			worked = true
			if exhausted, err = s.mineMore(ctx, floor-len(c.ready)-len(c.waiting)); err != nil {
				return false, err
			}
		default:
			s.explainShortage(c, floor, exhausted)
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
	arms, err := s.w.validating(nil, s.env.Now).Arms(ctx, s.w.project.ID, snaps)
	if err != nil {
		return err
	}
	fmt.Fprintf(s.env.Stdout, "Validating %d task(s) in %d context(s), %d at a time\n", len(tasks), len(arms), defaultJobs)
	began := s.env.Now()
	quiet := s.env
	quiet.Stdout = io.Discard // the batch's own table is long; the summary below is what start shows
	results, err := validateBatch(ctx, quiet, s.w, tasks, task.ValidateOptions{Arms: arms, Repeat: 1, Timeout: 10 * time.Minute}, defaultJobs)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	var bad []string
	for _, r := range results {
		attempted[r.task.Name] = true
		status := "not validated"
		if r.validated {
			status = task.ValidationOf(r.task).Status
		}
		counts[status]++
		if status != task.StatusValid {
			bad = append(bad, r.task.Name+": "+status)
		}
	}
	line := fmt.Sprintf("  %d valid of %d, in %s", counts[task.StatusValid], len(results), s.env.Now().Sub(began).Round(time.Second))
	if len(bad) > 0 {
		line += "; set aside: " + strings.Join(bad, ", ")
	}
	fmt.Fprintln(s.env.Stdout, line)
	return nil
}

// mineMore imports up to want more candidates from the history as tasks (unvalidated); exhausted is whether the
// history has no more to give.
func (s *starter) mineMore(ctx context.Context, want int) (exhausted bool, err error) {
	w, out := s.w, s.env.Stdout
	prep, err := mine.Prepare(ctx, mine.PrepareInput{DB: w.db, ProjectID: w.project.ID, Root: w.root,
		Options: mine.Options{MaxFiles: mine.DefaultMaxFiles, MaxLines: mine.DefaultMaxLines}, DefaultVerify: w.defaultVerify()})
	if err != nil {
		return false, err
	}
	found := len(prep.Result.Candidates)
	if found == 0 {
		fmt.Fprintf(out, "Mining: no more candidates in %d commit(s) read\n", prep.Result.Scanned)
		return true, nil
	}
	imp := mine.Import(ctx, mine.ImportInput{Importer: w.importer(prep.Names), Candidates: prep.Result.Candidates, Limit: want,
		NewTask: func() store.Task {
			return store.Task{ProjectID: w.project.ID, Verify: prep.Verify, CreatedAt: s.env.Now()}
		}})
	fmt.Fprintf(out, "Mining: %d candidate(s) in %d commit(s) read; imported %d of %d tried (verify: %s)\n", found, prep.Result.Scanned,
		len(imp.Tasks), imp.Tried, strings.Join(prep.Verify, "; "))
	return len(imp.Tasks) < want, nil // fewer imported than asked: every candidate was tried
}

// acceptReviews marks waiting tasks reviewed when the automatic checks find nothing to review: no section that may
// give the solution away, and no requirement of the hidden tests that nothing states. The rest stay for a person.
func (s *starter) acceptReviews(ctx context.Context) (accepted int, err error) {
	tasks, err := s.w.db.Tasks(ctx, s.w.project.ID)
	if err != nil {
		return 0, err
	}
	quiet := s.env
	quiet.Stderr = io.Discard
	for _, t := range tasks {
		if !t.NeedsReview || t.SolutionCommit == "" || t.Grading == task.GradingJudge || len(task.SolutionSections(t.Instruction)) > 0 {
			continue
		}
		if ok, err := gapGate(ctx, quiet, s.w, t, false); err != nil {
			return accepted, err
		} else if !ok {
			continue
		}
		t.NeedsReview = false
		if err := s.w.db.UpdateTask(ctx, t, s.env.Now()); err != nil {
			return accepted, err
		}
		accepted++
	}
	return accepted, nil
}

// explainShortage says how many tasks start found and what to do next.
func (s *starter) explainShortage(c taskCounts, floor int, exhausted bool) {
	out, st := s.env.Stdout, s.env.style()
	if len(c.waiting) > 0 {
		fmt.Fprintf(out, "Tasks: %s\n", st.Warn(fmt.Sprintf("%d valid, %d ready of the %d an experiment needs: the others wait for your review", len(c.ready)+len(c.waiting), len(c.ready), floor)))
		fmt.Fprintf(out, "  Read each instruction for solution leaks: %s, then %s\n"+
			"  (or %s accepts those whose instructions pass the leak and gap checks)\n", st.Command("agentium task show NAME"),
			st.Command("agentium task edit NAME --reviewed"), st.Command("agentium start --reviewed"))
		fmt.Fprintf(out, "  waiting: %s\n", strings.Join(c.waiting, ", "))
	} else {
		fmt.Fprintf(out, "Tasks: %s\n", st.Bad(fmt.Sprintf("only %d of the %d an experiment needs are ready", len(c.ready), floor)))
	}
	if exhausted && len(c.ready)+len(c.waiting) < floor {
		fmt.Fprintf(out, "  the history has no more candidates: %s shows why commits were set aside; add tasks with %s or %s, then run %s again\n",
			st.Command("agentium task mine --dry-run"), st.Command("agentium task add"), st.Command("agentium task import --commit REF"), st.Command("agentium start"))
	}
}
