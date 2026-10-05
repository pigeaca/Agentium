package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/claudectx"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/pricing"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/runner"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
	"github.com/pigeaca/agentium/internal/term"
)

// `agentium task draft NAME`: one paid Claude call, without tools, writes a task text from the commit message, the
// reference change and the hidden tests (task.Draft). Money first, in this order:
//  1. Before any spend: the task can be drafted (draftable), the preview names the most it may cost, and the consent
//     stands (a person's own command on a terminal, else --yes). The data folder's run lock is taken, as run once does,
//     and what earlier draft calls left is counted (recoverDrafts).
//  2. The call writes its output to a file in its own folder (<data>/artifacts/drafts/<project>/<call>) before
//     anything reads it; the folder goes only once the call's cost is in the store (store.CountDraftCall, which lists
//     the call so a crash between the count and the removal never counts it twice).
//  3. Only then is the reply read and checked; a draft that passes both checks is stored beside the instruction.

// The files of a draft call's folder.
const (
	draftCallFile = "call.json" // draftCallMeta, written before the call starts
	draftOutFile  = "out.json"  // Claude Code's stdout: its JSON result, with what the call cost
	draftErrFile  = "err.txt"
	draftPGIDFile = "pgid"  // the call's process group, once it runs
	draftStartDir = "start" // the empty folder Claude Code starts in
)

// draftCallMeta is what a draft call's folder says of it: enough to count a stopped call's cost against its task.
type draftCallMeta struct {
	TaskID int64  `json:"task_id"`
	Task   string `json:"task"`
	// TaskCreated, with the id and the name, is the task's identity (store.TaskRef): a removed task's id may be reused.
	TaskCreated time.Time `json:"task_created"`
	Model       string    `json:"model"`
	Started     time.Time `json:"started"`
}

// ref is the task the call was made for.
func (m draftCallMeta) ref() store.TaskRef {
	return store.TaskRef{ID: m.TaskID, Name: m.Task, CreatedAt: m.TaskCreated}
}

// draftMostUSD is the most one draft call may cost: its cap, and what a call can pass it by (Claude Code checks the cap
// after the turn).
func draftMostUSD() float64 {
	return task.DraftCapUSD + claude.CapOvershootUSD(task.DraftCapUSD, task.DraftModel)
}

// draftable says why t cannot be drafted, or nil: a draft is written from a solution with hidden tests, for a task an
// experiment can still take, by a model the price list knows (so the preview's bound is real).
func draftable(t store.Task) error {
	switch {
	case t.Retired():
		return fmt.Errorf("task %s is retired (%s): new experiments leave it out, so a draft of it would not be used", t.Name, t.RetiredReason)
	case t.Grading == task.GradingJudge:
		return fmt.Errorf("task %s is judge-graded: a draft is written from hidden tests, and it has none", t.Name)
	case t.SolutionCommit == "" || len(t.HiddenTests) == 0:
		return fmt.Errorf("task %s has no solution with hidden tests: a draft is written from them", t.Name)
	}
	if _, ok := pricing.Lookup(task.DraftModel); !ok {
		return fmt.Errorf("the price list does not know %s, the drafter's model: the most a call may cost cannot be named", task.DraftModel)
	}
	return nil
}

func taskDraft(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("task draft", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "consent to the paid call, which --json and a command run off a terminal need")
	timeout := fs.Duration("timeout", task.DraftTimeout, "stop the call after this long (hidden: the guide's \"Advanced flags\")")
	rest, code, ok := parseArgs(env, fs, args, taskUsage)
	if !ok {
		return code
	}
	if len(rest) != 1 || *timeout <= 0 {
		fmt.Fprint(env.Stderr, taskUsage)
		return ExitUsage
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	d, code, ok := prepareDraft(ctx, env, w, rest[0], *yes)
	if !ok {
		return code
	}
	release, err := startRuns(ctx, env, w)
	if err != nil {
		return fail(env, err)
	}
	defer release()
	res, err := d.draft(ctx, *timeout)
	switch {
	case err != nil && d.counted && env.JSON: // after the count (a check could not run): the document keeps the call's cost
		d.doc.Outcome, d.doc.Reason, d.doc.NextCommand = "failed", env.redact(err.Error()), "agentium task draft "+d.t.Name
		return env.emitCode(d.doc, ExitError)
	case err != nil:
		if d.counted {
			fmt.Fprintln(env.Stdout, draftSpentText(res, d.doc.DraftingSpendUSD, draftMostUSD()))
		}
		return fail(env, err)
	}
	return d.report(ctx, res)
}

// drafting is one task draft command's state: the task, the call's sign-in, and the document it fills in.
type drafting struct {
	env               Env
	w                 *workspace
	t                 store.Task
	cli, mode, secret string
	doc               taskDraftDoc
	// counted: the call returned and its cost was counted; removed: the task it was made for is gone (or its id is
	// another task's now), so no task's spend counted it and nothing may be stored.
	counted, removed bool
}

// prepareDraft is everything before any spend: the task can be drafted, the preview names the most the call may cost,
// and the consent stands (a person's own command on a terminal, else --yes). A refusal is reported, with its exit code.
func prepareDraft(ctx context.Context, env Env, w *workspace, name string, yes bool) (*drafting, int, bool) {
	d := &drafting{env: env, w: w}
	var err error
	if d.t, err = w.db.TaskByName(ctx, w.project.ID, name); err == nil {
		err = draftable(d.t)
	}
	if err == nil {
		d.cli, err = claudePath(env)
	}
	if err == nil {
		d.mode, d.secret, _, err = signIn(env)
	}
	if err != nil {
		return nil, fail(env, err), false
	}
	most := draftMostUSD()
	d.doc = taskDraftDoc{header: env.hdr(), Task: d.t.Name, RefusedBy: []string{}, Gaps: []gapDoc{}, Giveaways: []string{}, Model: task.DraftModel,
		MaxCostUSD: most, DraftingSpendUSD: d.t.DraftSpendUSD, Notes: []string{}}
	fmt.Fprintf(env.Stdout, "Drafting the text of task %s: one call to %s without tools (sign-in %s), at most $%.2f (its $%.2f cap, which a call can pass by up to $%.2f).\n",
		d.t.Name, task.DraftModel, d.mode, most, task.DraftCapUSD, most-task.DraftCapUSD)
	// Consent: a person's own command on a terminal is theirs; a script's (--json, or no terminal) is its --yes.
	if yes || (!env.JSON && env.Terminal && env.StdinTerminal) {
		return d, ExitOK, true
	}
	d.doc.Outcome, d.doc.RefusedBy = "refused", []string{"consent"}
	d.doc.Reason = "nothing was run or spent: a paid call needs --yes with --json or off a terminal"
	d.doc.NextCommand = "agentium task draft " + d.t.Name + " --yes"
	if env.JSON {
		return nil, env.emitCode(d.doc, ExitError), false
	}
	fmt.Fprintf(env.Stderr, "agentium task draft: nothing was run or spent: off a terminal a paid call needs --yes (%s)\n", d.doc.NextCommand)
	return nil, ExitError, false
}

// draft makes the call, under the run lock the caller holds: first it settles what earlier calls left (recoverDrafts)
// and reads the task again, then the call's cost is counted as soon as it returns, then the checks run (task.Draft).
// The document gets the notes, the call's cost and the task's drafting spend.
func (d *drafting) draft(ctx context.Context, timeout time.Duration) (task.DraftResult, error) {
	env, w := d.env, d.w
	d.doc.Notes = append(d.doc.Notes, w.draftNotes...) // what startRuns settled
	// Read again under the lock: the task may have changed (or gone) since the preview.
	var err error
	if d.t, err = w.db.TaskByName(ctx, w.project.ID, d.t.Name); err != nil {
		return task.DraftResult{}, err
	}
	if err := draftable(d.t); err != nil {
		return task.DraftResult{}, err
	}
	in, err := task.DraftSources(ctx, w.bare, d.t)
	if err != nil {
		return task.DraftResult{}, err
	}
	call, dir, err := w.newDraftCall(env, d.t, d.cli, d.mode, d.secret, timeout)
	if err != nil {
		return task.DraftResult{}, err
	}
	// The count runs once, as soon as the call returns, whatever it brought (task.Draft): the cost goes into the store
	// first, then the call's folder, which until then holds the only record of it, goes.
	count := func(cost float64, reported bool) error {
		_, err := w.db.CountDraftCall(context.WithoutCancel(ctx), filepath.Base(dir), d.t.Ref(), cost, env.Now())
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err // the folder stays: the next start of paid work counts it
		}
		d.counted, d.removed = true, err != nil
		if err := os.RemoveAll(dir); err != nil {
			fmt.Fprintln(env.Stdout, warning(env.style(), fmt.Sprintf("the draft call's folder could not be removed (%v); the next task draft removes it, its cost already counted", err)))
		}
		return nil
	}
	res, err := task.Draft(ctx, task.NewFairness("--git-dir", w.bare), d.t, in, call, count)
	if counted, _ := w.db.TaskByName(context.WithoutCancel(ctx), w.project.ID, d.t.Name); counted.Ref() == d.t.Ref() {
		d.doc.DraftingSpendUSD = counted.DraftSpendUSD
	}
	if res.CostReported {
		d.doc.CostUSD = &res.CostUSD
	}
	for _, c := range res.Cut {
		d.doc.Notes = append(d.doc.Notes, fmt.Sprintf("the prompt cut %s to its first characters", c))
	}
	return res, err
}

// report stores a draft that passed both checks and says how the call ended; only a stored draft exits 0.
func (d *drafting) report(ctx context.Context, res task.DraftResult) int {
	env, t, st := d.env, d.t, d.env.style()
	spent := draftSpentText(res, d.doc.DraftingSpendUSD, draftMostUSD())
	redact := func(s string) string { return string(run.Redact([]byte(s), d.secret)) }
	if d.removed {
		res.Problem = fmt.Sprintf("task %s was removed while the call ran: its cost is listed with the draft calls, no task's spend counts it, and nothing was stored", t.Name)
	}
	if res.Problem != "" {
		d.doc.Outcome, d.doc.Reason, d.doc.NextCommand = "failed", env.redact(redact(res.Problem)), "agentium task draft "+t.Name
		if env.JSON {
			return env.emitCode(d.doc, ExitError)
		}
		fmt.Fprintf(env.Stdout, "The call brought no draft (%s).\n", spent)
		return fail(env, fmt.Errorf("task draft %s: no draft: %s", t.Name, redact(res.Problem)))
	}
	text := redact(res.Text)
	d.doc.Draft, d.doc.Gaps, d.doc.Giveaways = &text, gapDocs(res.Gaps), list(res.Giveaways)
	if !res.Passed() {
		d.doc.Outcome, d.doc.NextCommand = "refused", "agentium task draft "+t.Name
		if len(res.Gaps) > 0 {
			d.doc.RefusedBy = append(d.doc.RefusedBy, "fairness")
		}
		if len(res.Giveaways) > 0 {
			d.doc.RefusedBy = append(d.doc.RefusedBy, "giveaway")
		}
		d.doc.Reason = "the draft did not pass " + strings.Join(d.doc.RefusedBy, " and ") + "; nothing was stored"
		if env.JSON {
			return env.emitCode(d.doc, ExitError)
		}
		printRefusedDraft(env, st, t, res, text, spent)
		return ExitError
	}
	if err := d.w.db.SetTaskDraft(context.WithoutCancel(ctx), t.Ref(), text, task.DraftModel, env.Now()); errors.Is(err, store.ErrNotFound) {
		return fail(env, fmt.Errorf("task %s was removed while the draft was checked: nothing was stored", t.Name))
	} else if err != nil {
		return fail(env, err)
	}
	d.doc.Outcome, d.doc.NextCommand = "stored", "agentium task edit "+t.Name+" --accept-draft"
	if env.JSON {
		return env.emit(d.doc)
	}
	fmt.Fprintf(env.Stdout, "%s (%s): it passed both checks, and the instruction is unchanged:\n", st.Good("Stored the draft beside the instruction"), spent)
	printIndented(env, text)
	fmt.Fprintf(env.Stdout, "Next: %s to read both, then %s to put it in place (the task then awaits your review)\n",
		st.Command("agentium task show "+t.Name), st.Command("agentium task edit "+t.Name+" --accept-draft"))
	return ExitOK
}

// draftSpentText says what the call cost and what drafting the task has cost so far.
func draftSpentText(res task.DraftResult, total, most float64) string {
	if !res.CostReported {
		return fmt.Sprintf("the call reported no cost: its spend is unknown, at most $%.2f; drafting this task has cost $%.2f counted", most, total)
	}
	return fmt.Sprintf("$%.2f; drafting this task has cost $%.2f", res.CostUSD, total)
}

// printRefusedDraft says which check refused the draft and what it found, and shows the refused text.
func printRefusedDraft(env Env, st term.Style, t store.Task, res task.DraftResult, text, spent string) {
	out := env.Stdout
	kept := ""
	if t.Draft != "" {
		kept = "; the earlier draft stays"
	}
	fmt.Fprintf(out, "%s (%s): nothing was stored%s.\n", st.Warn("Refused the draft"), spent, kept)
	if len(res.Gaps) > 0 {
		fmt.Fprintln(out, st.Warn(fmt.Sprintf("Fairness: it leaves unstated %d requirement(s) of the hidden tests:", len(res.Gaps))))
		for _, g := range res.Gaps {
			fmt.Fprintf(out, "  %s\n", g)
		}
	}
	if len(res.Giveaways) > 0 {
		fmt.Fprintln(out, st.Warn(fmt.Sprintf("Giveaway: it names %d name(s) only the reference solution has, which the hidden tests do not use:", len(res.Giveaways))))
		for _, n := range res.Giveaways {
			fmt.Fprintf(out, "  %s\n", n)
		}
	}
	fmt.Fprintln(out, st.Heading("The refused text:"))
	printIndented(env, text)
	fmt.Fprintf(out, "Run %s for another draft, or write the text yourself: %s\n", st.Command("agentium task draft "+t.Name),
		st.Command("agentium task edit "+t.Name+" --instruction @FILE"))
}

func printIndented(env Env, text string) {
	for _, line := range strings.Split(text, "\n") {
		fmt.Fprintf(env.Stdout, "  %s\n", line)
	}
}

// newDraftCall makes the folder of one draft call for t and returns the call (task.DraftCall) and its folder. The folder
// is <drafts>/<project>/<call id>, owner-only, and holds what the call is (draftCallMeta) before it starts; Claude Code
// starts in its empty start folder, with a fresh config folder beside it for an API key or a token, and writes its
// output to files there, never pipes. timeout bounds the call. The caller removes the folder once the call's cost is
// counted; until then it is what recoverDrafts reads.
func (w *workspace) newDraftCall(env Env, t store.Task, cli, mode, secret string, timeout time.Duration) (task.DraftCall, string, error) {
	id, err := run.NewID(env.Now())
	if err != nil {
		return nil, "", err
	}
	dir := filepath.Join(w.layout.Drafts(), strconv.FormatInt(w.project.ID, 10), id)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, "", fmt.Errorf("draft folder: %w", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, "", fmt.Errorf("draft folder: %w", err)
	}
	failed := func(err error) (task.DraftCall, string, error) {
		os.RemoveAll(dir) // nothing ran yet
		return nil, "", err
	}
	meta, err := json.Marshal(draftCallMeta{TaskID: t.ID, Task: t.Name, TaskCreated: t.CreatedAt, Model: task.DraftModel, Started: env.Now().UTC()})
	if err != nil {
		return failed(fmt.Errorf("draft call: %w", err))
	}
	if err := os.WriteFile(filepath.Join(dir, draftCallFile), meta, 0o600); err != nil {
		return failed(fmt.Errorf("draft call: %w", err))
	}
	start := filepath.Join(dir, draftStartDir)
	if err := os.Mkdir(start, 0o700); err != nil {
		return failed(fmt.Errorf("draft folder: %w", err))
	}
	if above := claudectx.InstructionFilesAbove(start); len(above) > 0 {
		return failed(fmt.Errorf("%s: Claude Code would load it into the draft call from above its folder; move it, or set AGENTIUM_HOME elsewhere", strings.Join(above, ", ")))
	}
	j := claude.Judgement{CLI: cli, Dir: start, Model: task.DraftModel, SystemPrompt: task.DraftSystemPrompt, Schema: task.DraftSchema,
		BudgetUSD: task.DraftCapUSD, SignIn: mode, Secret: secret, Home: env.Getenv("HOME")} // Effort: the CLI's default (decision 4)
	if mode != claude.SignInLogin {
		j.ConfigDir = filepath.Join(dir, "config")
		if err := os.Mkdir(j.ConfigDir, 0o700); err != nil {
			return failed(fmt.Errorf("draft folder: %w", err))
		}
	}
	var environ []string
	if env.Environ != nil {
		environ = env.Environ()
	}
	args, cmdEnv, err := j.Command(environ)
	if err != nil {
		return failed(err)
	}
	call := func(ctx context.Context, prompt string) (task.DraftReply, error) {
		files := make([]*os.File, 2) // stdout, stderr: files, not pipes (claude.RunJudgement)
		for i, name := range []string{draftOutFile, draftErrFile} {
			var err error
			if files[i], err = os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600); err != nil {
				return task.DraftReply{}, fmt.Errorf("draft output: %w", err)
			}
			defer files[i].Close()
		}
		// runner.Run, not claude.RunJudgement: the process group is written down as it starts, so a later task draft can
		// tell a call that a stopped Agentium left running from one that ended.
		result, runErr := runner.Run(ctx, runner.Spec{Dir: start, Args: append([]string{cli}, args...), Environ: cmdEnv, Stdin: strings.NewReader(prompt),
			Timeout: timeout, Grace: task.DraftGrace, Output: files[0], Stderr: files[1],
			Started: func(pid int) {
				_ = os.WriteFile(filepath.Join(dir, draftPGIDFile), []byte(strconv.Itoa(pid)+"\n"), 0o600)
			}})
		stdout, _ := os.ReadFile(filepath.Join(dir, draftOutFile))
		stderr, _ := os.ReadFile(filepath.Join(dir, draftErrFile))
		return task.DraftReply{Stdout: stdout, Stderr: strings.TrimSpace(string(stderr)), ExitCode: result.ExitCode, TimedOut: result.TimedOut}, runErr
	}
	return call, dir, nil
}

// settleDraftCalls settles the draft calls that a stopped Agentium left in the data folder, of every project (their
// folders, newDraftCall), and returns one line for each that it says something about, also printed to out. Every
// command that starts paid work calls it under the run lock (startRuns), so no agent or call starts beside a call that
// still runs:
//   - while any call may still run (its process group exists, and its leader is not clearly another program: one that
//     started more than draftStartSlack away from the call), it settles nothing and fails, saying where the call's
//     folder is and what removing it means;
//   - a call whose output reports its cost has that cost counted against the task it was made for, once
//     (store.CountDraftCall: a call counted before its folder went says nothing; a task removed since is not charged);
//   - a call whose output holds no cost (it never finished) is said to have an unknown spend, at most its bound.
//
// Then its folder goes. Its draft is never taken from a leftover reply: the owner runs task draft again.
func settleDraftCalls(ctx context.Context, env Env, db *store.Store, layout home.Layout, out io.Writer) ([]string, error) {
	projects, err := os.ReadDir(layout.Drafts())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("draft calls: %w", err)
	}
	var calls []string // the call folders, newDraftCall's only (folders, never links)
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(layout.Drafts(), p.Name()))
		if err != nil {
			return nil, fmt.Errorf("draft calls: %w", err)
		}
		for _, e := range entries {
			if e.IsDir() {
				calls = append(calls, filepath.Join(layout.Drafts(), p.Name(), e.Name()))
			}
		}
	}
	metas := make([]draftCallMeta, len(calls))
	metaOK := make([]bool, len(calls))
	var notes []string
	say := func(line string) {
		notes = append(notes, env.redact(line))
		fmt.Fprintln(out, env.style().Warn(line))
	}
	// First, whether any may still run: then nothing is settled, so a refusal changes nothing.
	for i, path := range calls {
		data, err := os.ReadFile(filepath.Join(path, draftCallFile))
		metaOK[i] = err == nil && json.Unmarshal(data, &metas[i]) == nil && metas[i].TaskID != 0
		data, err = os.ReadFile(filepath.Join(path, draftPGIDFile))
		pgid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || convErr != nil || pgid <= 1 || !draftGroupExists(pgid) {
			continue
		}
		if leader, ok := processStarted(ctx, pgid); ok && metaOK[i] && !metas[i].Started.IsZero() && leader.Sub(metas[i].Started).Abs() > draftStartSlack {
			say(fmt.Sprintf("Process group %d of a draft call left by a stopped Agentium (%s) now belongs to another program, started %s; the call is settled as ended",
				pgid, filepath.Base(path), leader.UTC().Format(time.RFC3339)))
			continue
		}
		return nil, fmt.Errorf("a draft call that a stopped Agentium left may still be running (process group %d): wait for it to finish, or stop it, then try again "+
			"(ps -o pid,command -g %d shows what it is). If that group is another program's (after a restart the number may be reused), remove the call's folder %s: "+
			"its spend is then unknown, at most $%.2f, and not counted", pgid, pgid, env.redact(path), draftMostUSD())
	}
	for i, path := range calls {
		meta := metas[i]
		stdout, _ := os.ReadFile(filepath.Join(path, draftOutFile))
		a := task.ReadDraftReply(task.DraftReply{Stdout: stdout})
		switch {
		case metaOK[i] && a.CostReported:
			counted, err := db.CountDraftCall(ctx, filepath.Base(path), meta.ref(), a.CostUSD, env.Now())
			switch {
			case errors.Is(err, store.ErrNotFound):
				say(fmt.Sprintf("A draft call of task %s, left by a stopped Agentium, spent $%.2f; that task was removed since, so no task's spend counts it", meta.Task, a.CostUSD))
			case err != nil:
				return notes, err
			case counted:
				say(fmt.Sprintf("Counted $%.2f that a draft call of task %s spent before Agentium stopped; its draft was not kept (run agentium task draft %s again)",
					a.CostUSD, meta.Task, meta.Task))
			}
		case metaOK[i]:
			listed, err := db.DraftCallCounted(ctx, filepath.Base(path))
			if err != nil {
				return notes, err
			}
			if !listed {
				say(fmt.Sprintf("A draft call of task %s, left by a stopped Agentium, reported no cost: its spend is unknown, at most $%.2f", meta.Task, draftMostUSD()))
			}
		default:
			say(fmt.Sprintf("A draft call left by a stopped Agentium (%s) cannot be read: its spend is unknown, at most $%.2f", filepath.Base(path), draftMostUSD()))
		}
		if err := os.RemoveAll(path); err != nil {
			return notes, fmt.Errorf("remove the folder of draft call %s: %w", filepath.Base(path), err)
		}
	}
	return notes, nil
}

// draftStartSlack is how far apart a draft call's start (draftCallMeta.Started) and its process group leader's start
// may be for the group to be the call's: the call starts within moments of its folder, and ps reports whole seconds.
const draftStartSlack = 2 * time.Minute

// processStarted is when process pid started, from ps; false when it cannot be told (no such process, no ps).
func processStarted(ctx context.Context, pid int) (time.Time, bool) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	cmd.Env = []string{"LC_ALL=C"} // English names; the same zone as time.Local, which TZ sets for both
	if tz, ok := os.LookupEnv("TZ"); ok {
		cmd.Env = append(cmd.Env, "TZ="+tz)
	}
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}, false
	}
	at, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(strings.Fields(string(out)), " "), time.Local)
	return at, err == nil
}

// draftGroupExists reports whether a process group exists (possibly another user's: EPERM).
func draftGroupExists(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
