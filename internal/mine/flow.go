package mine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/store"
)

// The flow of `task mine`: Prepare scans the history for candidates, Import turns the best into tasks. Both keep no
// state and take what they need as parameters; the command prints.

// PrepareInput is what a scan needs: where the project is and what it already has.
type PrepareInput struct {
	DB        *store.Store
	ProjectID int64
	Root      string  // the user's repository
	Options   Options // Since, MaxFiles and MaxLines; Prepare adds the exclusions and the test languages
	// Verify is the verification commands asked for (empty: the detected build tools' test commands, else
	// DefaultVerify).
	Verify        []string
	DefaultVerify []string // the project's detected test commands
	DryRun        bool     // nothing is imported, so no verification command is needed
}

// Prepared is a scan and what the import needs from it.
type Prepared struct {
	Result  Result
	Options Options         // as scanned
	Verify  []string        // the commands mined tasks verify with
	Names   map[string]bool // the project's task names, which unique names avoid
}

// Prepare reads the project's tasks (their commits are excluded from the scan), works out the tests mining picks
// commits by, and scans the history.
func Prepare(ctx context.Context, in PrepareInput) (Prepared, error) {
	opts := in.Options
	opts.Exclude = map[string]bool{}
	existing, err := in.DB.Tasks(ctx, in.ProjectID)
	if err != nil {
		return Prepared{}, err
	}
	names := map[string]bool{}
	for _, t := range existing {
		names[t.Name] = true
		if t.SolutionCommit != "" {
			opts.Exclude[t.SolutionCommit] = true
		}
	}
	var toolCommands []string
	opts.Languages, toolCommands = TestLanguages(in.Root)
	opts.TestCommand = strings.Join(toolCommands, ", ")
	// Mined tasks verify with the build tools' own test commands: the tests mining picked commits by. Other commands the
	// project runs (linters, documentation checks) would fail at old commits for reasons no agent can fix, and are saved
	// with the task for every later grading. Without a detected tool, the project's default commands are all there is.
	verify := in.Verify
	if len(verify) == 0 {
		if verify = toolCommands; len(verify) == 0 {
			verify = in.DefaultVerify
		}
	}
	if !in.DryRun && len(verify) == 0 {
		return Prepared{}, errors.New("no test commands were detected for this project: pass --verify CMD")
	}
	res, err := Scan(ctx, in.Root, opts)
	if err != nil {
		return Prepared{}, err
	}
	return Prepared{Result: res, Options: opts, Verify: verify, Names: names}, nil
}

// TestLanguages names the languages of the tests the project's detected build tools run (Options.Languages), and those
// tools' test commands.
func TestLanguages(root string) (languages, commands []string) {
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(root, name))
		return err == nil
	}
	for _, p := range buildtool.Detected(has) {
		languages = append(languages, p.Languages...)
		if c := p.TestCommand(has); c != "" {
			commands = append(commands, c)
		}
	}
	return languages, commands
}

// ErrAlreadyTask means a candidate became a task (in another process) while it was being imported.
var ErrAlreadyTask = errors.New("already a task")

// Importer turns candidates into stored tasks. Commit and Complete are the steps `task import --commit` takes (keep the
// commit, take its instruction, split its solution), which the command line owns.
type Importer struct {
	DB        *store.Store
	ProjectID int64
	Names     map[string]bool // the project's task names; One adds what it saves
	// Commit sets t's commits, source, instruction (c's) and a name from c's commit.
	Commit func(ctx context.Context, c Candidate, t store.Task) (store.Task, error)
	// Complete fills in t's verify commands, hidden tests, reference and grading, refusing what cannot be a task.
	Complete func(ctx context.Context, t store.Task) (store.Task, error)
}

// One imports candidate c as the task t describes, with a name not in Names, which it then adds there. When another
// process made a task of the same commit meanwhile (a name clash shows it), it returns ErrAlreadyTask, so no commit is
// imported twice.
func (im Importer) One(ctx context.Context, c Candidate, t store.Task) (store.Task, error) {
	t, err := im.Commit(ctx, c, t)
	if err != nil {
		return t, err
	}
	base := t.Name
	unique := func() {
		t.Name = base
		for n := 2; im.Names[t.Name]; n++ {
			t.Name = fmt.Sprintf("%s-%d", base, n)
		}
	}
	unique()
	if t, err = im.Complete(ctx, t); err != nil {
		return t, err
	}
	for {
		saved, err := im.DB.SaveTask(ctx, t)
		if !errors.Is(err, store.ErrExists) {
			if err != nil {
				return t, err
			}
			im.Names[saved.Name] = true
			return saved, nil
		}
		// Another process saved a task since the list was read: read it again.
		tasks, err := im.DB.Tasks(ctx, im.ProjectID)
		if err != nil {
			return t, err
		}
		for _, other := range tasks {
			im.Names[other.Name] = true
			if other.SolutionCommit == t.SolutionCommit {
				return t, ErrAlreadyTask
			}
		}
		unique()
	}
}

// ImportInput is what an import of the best candidates needs.
type ImportInput struct {
	Importer
	Candidates []Candidate // best first
	Limit      int         // how many to import
	// NewTask is a fresh task to fill in for each candidate.
	NewTask func() store.Task
	// Progress is told before each candidate: how many are imported so far, and the candidate.
	Progress func(imported int, c Candidate)
}

// Failure is a candidate that did not become a task, and why.
type Failure struct {
	Candidate Candidate
	Err       error
}

// Imported is what an import did: the tasks saved, the candidates that failed, how many were tried, and whether an
// interrupt stopped it (what was imported is kept).
type Imported struct {
	Tasks       []store.Task
	Failed      []Failure
	Tried       int
	Interrupted bool
}

// Import imports candidates, best first, until Limit of them succeed; those that fail are reported, not retried. A
// candidate that another process imported meanwhile is skipped quietly.
func Import(ctx context.Context, in ImportInput) Imported {
	var res Imported
	for _, c := range in.Candidates {
		if len(res.Tasks) == in.Limit {
			break
		}
		res.Tried++
		if in.Progress != nil {
			in.Progress(len(res.Tasks), c)
		}
		t, err := in.One(ctx, c, in.NewTask())
		switch {
		case err == nil:
			res.Tasks = append(res.Tasks, t)
		case errors.Is(err, ErrAlreadyTask):
		case ctx.Err() == nil:
			res.Failed = append(res.Failed, Failure{c, err})
		}
		if ctx.Err() != nil {
			res.Interrupted = true
			return res
		}
	}
	return res
}
