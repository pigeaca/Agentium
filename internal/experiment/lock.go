package experiment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/task"
)

// Methods name the rules an experiment runs and is analysed by: the schedule, the caps, retries, what counts, and the
// floors. A lock records its method; a resume refuses an unknown one, and the analysis applies the lock's floors.
//   - phase1-v1: the study's floors, 3 runs per task and arm for every metric.
//   - phase1-v2: the cost floor counts tasks with one run in each arm (FloorsFor); it runs exactly as phase1-v1, so
//     phase1-v1 experiments still resume, and keep their floors.
const (
	MethodV1 = "phase1-v1"
	MethodV2 = "phase1-v2"
)

// MethodVersion is the method new experiments are locked under.
const MethodVersion = MethodV2

// Resumable reports whether an experiment locked under method runs exactly as this Agentium runs experiments.
func Resumable(method string) bool { return method == MethodV1 || method == MethodV2 }

// MaxAttempts is how often a slot is tried when its runs fail for infrastructure reasons.
const MaxAttempts = 3

// Lock is what an experiment fixes before its first run. Resumes check it against the machine, and runs use it, not
// the tasks and snapshots as they are later.
type Lock struct {
	Method        string         `json:"method"`
	Agentium      string         `json:"agentium"`
	LockedAt      time.Time      `json:"locked_at"`
	ClaudeCode    string         `json:"claude_code"` // the version every run must report
	ClaudePath    string         `json:"claude_path"`
	SignIn        string         `json:"sign_in"`
	Host          string         `json:"host"`        // operating system and architecture
	PriceTable    string         `json:"price_table"` // the date of Agentium's price table
	Design        Design         `json:"design"`
	Arms          []LockedArm    `json:"arms"`
	Tasks         []LockedTask   `json:"tasks"`
	Schedule      []Slot         `json:"schedule"`
	MaxAttempts   int            `json:"max_attempts"`
	BudgetChanges []BudgetChange `json:"budget_changes,omitempty"`
}

// LockedArm is an arm's context and the environment its runs must see.
type LockedArm struct {
	Arm
	Files       []FileDigest `json:"files,omitempty"` // the snapshot's context files
	Calibration string       `json:"calibration"`     // the calibration run
	// Model, Tools, Skills and SlashCommands are what the calibration found (Skills and SlashCommands are Claude Code's
	// bundled ones). Skill and command names stay in Agentium's database: reports give counts.
	Model         string   `json:"model"`
	Tools         []string `json:"tools"`
	Skills        []string `json:"skills"`
	SlashCommands []string `json:"slash_commands"`
}

// FileDigest is one context file.
type FileDigest struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// LockedTask is a task as the experiment runs it.
type LockedTask struct {
	Name        string   `json:"name"`
	Instruction string   `json:"instruction"`
	Base        string   `json:"base"`
	Solution    string   `json:"solution,omitempty"`
	HiddenTests []string `json:"hidden_tests,omitempty"`
	Reference   []string `json:"reference,omitempty"`
	Setup       []string `json:"setup,omitempty"`
	Verify      []string `json:"verify"`
	Digest      string   `json:"digest"` // SHA-256 of everything above
}

// NewLockedTask fixes a task and its digest.
func NewLockedTask(name, instruction string, spec task.Spec) LockedTask {
	t := LockedTask{Name: name, Instruction: instruction, Base: spec.Base, Solution: spec.Solution, HiddenTests: spec.HiddenTests,
		Reference: spec.Reference, Setup: spec.Setup, Verify: spec.Verify}
	encoded, _ := json.Marshal(t) // strings and string lists only: cannot fail
	sum := sha256.Sum256(encoded)
	t.Digest = hex.EncodeToString(sum[:])
	return t
}

// Spec is the task's run specification.
func (t LockedTask) Spec() task.Spec {
	return task.Spec{Base: t.Base, Solution: t.Solution, HiddenTests: t.HiddenTests, Reference: t.Reference, Setup: t.Setup, Verify: t.Verify}
}

// Expect is the environment the arm's runs are checked against.
func (a LockedArm) Expect(cliVersion string) claude.Expect {
	return claude.Expect{CLIVersion: cliVersion, Tools: a.Tools, Skills: a.Skills, SlashCommands: a.SlashCommands}
}

// BudgetChange records a budget raised on resume.
type BudgetChange struct {
	At   time.Time `json:"at"`
	From float64   `json:"from_usd"`
	To   float64   `json:"to_usd"`
}

// Slot is one run of the schedule: a task's repeat in one arm. The two arms of a (task, repeat) pair are adjacent.
type Slot struct {
	Position int    `json:"position"`
	Pair     int    `json:"pair"`
	Task     string `json:"task"`
	Arm      string `json:"arm"`
	Repeat   int    `json:"repeat"` // from 1
}

// Schedule orders the runs: repeat by repeat, so a stopped experiment still has every task at similar depth; within a
// repeat, the tasks in a random order; and each pair's arms next to each other, in a random order, so time of day and
// the API's condition fall on both arms alike. Seeded, so the same design gives the same schedule.
func Schedule(d Design) []Slot {
	r := rand.New(rand.NewPCG(d.Seed, 1)) // stream 1: the task sample uses stream 0
	var slots []Slot
	for repeat := 1; repeat <= d.Repeats; repeat++ {
		tasks := append([]string(nil), d.Tasks...)
		r.Shuffle(len(tasks), func(i, j int) { tasks[i], tasks[j] = tasks[j], tasks[i] })
		for _, t := range tasks {
			arms := []string{d.Arms[0].Name, d.Arms[1].Name}
			if r.IntN(2) == 1 {
				arms[0], arms[1] = arms[1], arms[0]
			}
			pair := len(slots) / 2
			for _, a := range arms {
				slots = append(slots, Slot{Position: len(slots), Pair: pair, Task: t, Arm: a, Repeat: repeat})
			}
		}
	}
	return slots
}

// Check compares a lock with the machine a resume runs on: everything that would make later runs incomparable with
// earlier ones.
func (l Lock) Check(cliVersion, signIn string) error {
	switch {
	case !Resumable(l.Method):
		return fmt.Errorf("the experiment was locked under method %s; this Agentium runs %s and %s", l.Method, MethodV1, MethodV2)
	case cliVersion != l.ClaudeCode:
		return fmt.Errorf("Claude Code is %s now, but the experiment's runs used %s: install %s again to continue, or start a new experiment", cliVersion, l.ClaudeCode, l.ClaudeCode)
	case signIn != l.SignIn:
		return fmt.Errorf("runs would sign in with %s, but the experiment's used %s", signIn, l.SignIn)
	}
	return nil
}

// Arm returns the locked arm of that name.
func (l Lock) Arm(name string) (LockedArm, bool) {
	for _, a := range l.Arms {
		if a.Name == name {
			return a, true
		}
	}
	return LockedArm{}, false
}

// Task returns the locked task of that name.
func (l Lock) Task(name string) (LockedTask, bool) {
	for _, t := range l.Tasks {
		if t.Name == name {
			return t, true
		}
	}
	return LockedTask{}, false
}

// Fair reports whether an outcome is the agent's own attempt, counted in results: ok, capped at the budget or turn
// limit, or stopped at the timeout.
func Fair(outcome string) bool {
	switch outcome {
	case claude.OutcomeOK, claude.OutcomeCapped, claude.OutcomeTimeout:
		return true
	}
	return false
}

// Settles reports whether a run settles its slot. Fair runs do; so do unfair ones (their environment drifted: they
// are excluded, not retried, since a retry would drift the same way). Infrastructure failures are retried, and
// cancelled runs are run again.
func Settles(outcome string) bool { return Fair(outcome) || outcome == claude.OutcomeUnfair }

// Success reports whether a run counts as a success: a fair run that passed the verification with the hidden tests,
// graded without test-runner configuration the agent changed beyond what the task's reference changes.
func Success(outcome string, passed *bool, configChanged []string) bool {
	return Fair(outcome) && passed != nil && *passed && len(configChanged) == 0
}
