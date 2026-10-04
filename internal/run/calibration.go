package run

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/claudectx"
	"github.com/pigeaca/agentium/internal/codex"
	"github.com/pigeaca/agentium/internal/source"
	"github.com/pigeaca/agentium/internal/task"
	"github.com/pigeaca/agentium/internal/term"
)

// calibrationPrompt asks for what every real task needs (a sandboxed shell command, a large output read back) and for
// the codeword Agentium added to the arm's instruction file. The checks rest on the transcript's tool results; the
// answer only has to agree.
const calibrationPrompt = "This is an environment check: do not change any files and do not search the repository.\n" +
	"1. Run this command with the Bash tool: printf 'agentium-sandbox-ok\\n'\n" +
	"2. Run exactly this command with the Bash tool, without redirecting its output: seq 1 40000\n" +
	"   Its output is too large to show in full: read the full output that Claude Code saved for you, and find its 20000th line.\n" +
	"3. Your project instructions, as loaded at the start, end with a calibration codeword. If you see none, it is NONE.\n" +
	"Then reply with exactly one line: SANDBOX=<what command 1 printed> LINE=<the 20000th line of command 2's output> CODEWORD=<the codeword>"

// codexCalibrationPrompt is calibrationPrompt for Codex: a sandboxed shell command and the codeword. Codex saves no
// large outputs for the agent to read back, so there is no second step (LargeOutput is n/a).
const codexCalibrationPrompt = "This is an environment check: do not change any files and do not search the repository.\n" +
	"1. Run this shell command: printf 'agentium-sandbox-ok\\n'\n" +
	"2. Your project instructions, as loaded at the start, end with a calibration codeword. If you see none, it is NONE.\n" +
	"Then reply with exactly one line: SANDBOX=<what the command printed> CODEWORD=<the codeword>"

// Check states.
const (
	checkOK         = "ok"
	checkFailed     = "FAILED"
	checkUnverified = "unverified" // the run did not show the evidence (for example, it worked around the saved output)
	checkNA         = "n/a"
)

// Calibration is what a calibration run found for one arm.
type Calibration struct {
	// Agent is the agent calibrated (agent.Name: absent for Claude Code, and in calibrations made before agents).
	Agent            string `json:"agent,omitempty"`
	Arm              string `json:"arm"`
	Snapshot         string `json:"snapshot,omitempty"`
	RunID            string `json:"run_id"`
	Outcome          string `json:"outcome"`
	Sandbox          string `json:"sandbox"`      // a sandboxed Bash command returned its output
	LargeOutput      string `json:"large_output"` // the output Claude Code saved was read back
	Instructions     string `json:"instructions"` // the codeword in the arm's instruction file came back without reading it
	FirstRequest     int64  `json:"first_request_tokens"`
	EstimatedContext int    `json:"estimated_context_tokens"` // the resolver's session-start estimate
	CLIVersion       string `json:"cli_version"`
	Model            string `json:"model"`           // as Claude Code reported it
	RequestedModel   string `json:"requested_model"` // as asked for (--model)
	// SignIn is how the calibration signed in: with the user's login, Claude Code keeps large outputs in the user's
	// config, which runs must still read back; with a key or token, in the run's own.
	SignIn string   `json:"sign_in,omitempty"`
	Tools  []string `json:"tools"`
	// Skills and SlashCommands are Claude Code's bundled ones: the arm's own project skills and commands are left out
	// and added back when a run is checked. Stored locally to check later runs; never printed.
	Skills        []string `json:"skills"`
	SlashCommands []string `json:"slash_commands"`
	Drift         []string `json:"drift,omitempty"`
	CostUSD       float64  `json:"cost_usd"`
}

// Healthy reports whether a calibration can be what later runs are checked against.
func (c Calibration) Healthy() bool {
	// LargeOutput is n/a only for an agent that saves no large outputs (Codex).
	return c.Outcome == agent.OutcomeOK && len(c.Drift) == 0 && c.Sandbox == checkOK && (c.LargeOutput == checkOK || c.LargeOutput == checkNA) &&
		(c.Instructions == checkOK || c.Instructions == checkNA)
}

// AllHealthy reports whether every calibration is healthy.
func AllHealthy(results []Calibration) bool {
	return !slices.ContainsFunc(results, func(c Calibration) bool { return !c.Healthy() })
}

// judgeChecks reads the checks from the run's tool calls and results.
func judgeChecks(calls []claude.ToolCall, answer, codeword, probeFile string) (sandbox, large, instructions string) {
	sandbox, large, instructions = checkFailed, checkUnverified, checkFailed
	saved := ""
	for i, c := range calls {
		command, _ := c.Input["command"].(string)
		switch {
		case c.Name == "Bash" && strings.Contains(command, "agentium-sandbox-ok"):
			if !c.IsError && strings.Contains(c.Result, "agentium-sandbox-ok") {
				sandbox = checkOK
			}
		case c.Name == "Bash" && strings.Contains(command, "seq 1 40000") && strings.Contains(c.Result, "<persisted-output>"):
			if _, rest, ok := strings.Cut(c.Result, "saved to: "); ok && len(strings.Fields(rest)) > 0 {
				saved = strings.Fields(rest)[0]
				large = checkFailed // there is a saved output: now it must be read back
				for _, later := range calls[i+1:] {
					file, _ := later.Input["file_path"].(string)
					cmd, _ := later.Input["command"].(string)
					if (file == saved || strings.Contains(cmd, saved)) && !later.IsError && strings.Contains(later.Result, "20000") {
						large = checkOK
						break
					}
				}
			}
		}
	}
	if large == checkOK && !strings.Contains(answer, "LINE=20000") {
		large = checkFailed
	}
	switch {
	case probeFile == "":
		instructions = checkNA
	case strings.Contains(answer, "CODEWORD="+codeword):
		instructions = checkOK
		for _, c := range calls { // finding the codeword with a tool (reading, grepping, a symlink) is not loading it
			if input, _ := json.Marshal(c.Input); strings.Contains(c.Result, codeword) || strings.Contains(string(input), path.Base(probeFile)) {
				instructions = checkUnverified
			}
		}
	}
	return sandbox, large, instructions
}

// codexChecks reads a Codex calibration's checks from the shell commands its stream shows: the sandboxed command's
// output, and the codeword in the answer, which a command that read the probe file or printed the codeword makes
// unverified. There is no large-output check (n/a).
func codexChecks(commands []codex.CommandResult, answer, codeword, probeFile string) (sandboxCheck, large, instructions string) {
	sandboxCheck, large, instructions = checkFailed, checkNA, checkFailed
	for _, c := range commands {
		if strings.Contains(c.Command, "agentium-sandbox-ok") && c.ExitCode != nil && *c.ExitCode == 0 && strings.Contains(c.Output, "agentium-sandbox-ok") {
			sandboxCheck = checkOK
		}
	}
	switch {
	case probeFile == "":
		instructions = checkNA
	case strings.Contains(answer, "CODEWORD="+codeword):
		instructions = checkOK
		for _, c := range commands { // finding the codeword with a command (reading, grepping) is not loading it
			if strings.Contains(c.Output, codeword) || strings.Contains(c.Command, path.Base(probeFile)) {
				instructions = checkUnverified
			}
		}
	}
	return sandboxCheck, large, instructions
}

// without lists the names in list that none of the others hold.
func without(list []string, others ...[]string) []string {
	out := []string{}
	for _, x := range list {
		found := false
		for _, o := range others {
			found = found || slices.Contains(o, x)
		}
		if !found {
			out = append(out, x)
		}
	}
	return out
}

// CalibrationArm is one arm to calibrate and the context it loads.
type CalibrationArm struct {
	Arm    task.Arm
	Source source.Source
}

// ArmSources opens the context of the base commit and of each snapshot in Agentium's bare repository, so that every
// arm of a calibration is read at its own commit. The first arm is the base's own context.
func ArmSources(ctx context.Context, bare, head string, snapshots []task.Arm) ([]CalibrationArm, error) {
	baseSource, err := source.Commit(ctx, head, "--git-dir", bare)
	if err != nil {
		return nil, err
	}
	arms := []CalibrationArm{{task.Arm{Name: "base"}, baseSource}}
	for _, s := range snapshots {
		src, err := source.Commit(ctx, s.Snapshot, "--git-dir", bare)
		if err != nil {
			return nil, err
		}
		arms = append(arms, CalibrationArm{s, src})
	}
	return arms, nil
}

// Calibrator runs one short real run per arm and keeps what each found. It starts agents: the caller holds the run
// lock and has prepared Execute (the run's environment and its storage).
type Calibrator struct {
	Head    string // the commit the base arm is calibrated at
	Arms    []CalibrationArm
	Model   string
	Effort  string // empty: the CLI's default (Claude Code's calibrations always use it: they are of a context on a model)
	Budget  float64
	Timeout time.Duration
	SignIn  string // recorded with each calibration
	// Agent is the agent calibrated (codex.Name, or "" for Claude Code): it decides the prompt and how the checks are
	// read. Execute must run that agent.
	Agent string
	Now   func() time.Time
	// Execute runs and stores one calibration run of the arm; it reports progress for the arm as it likes.
	Execute func(ctx context.Context, arm task.Arm, spec Spec) (Record, error)
	// Save stores a healthy calibration; unhealthy ones are never saved. It gets a context that outlives
	// cancellation, so an interrupt does not lose a finished arm.
	Save func(ctx context.Context, c Calibration) error
}

// Run calibrates each arm in turn and returns what it found, healthy or not. Only a healthy calibration is saved: it
// becomes what later runs are checked against.
func (k Calibrator) Run(ctx context.Context) ([]Calibration, error) {
	var results []Calibration
	for _, a := range k.Arms {
		resolved, err := claudectx.Resolve(a.Source)
		if err != nil {
			return results, err
		}
		suffix, err := NewID(k.Now()) // random enough to be unguessable
		if err != nil {
			return results, err
		}
		codeword := "AGENTIUM-" + strings.ToUpper(suffix[len(suffix)-6:])
		prompt := calibrationPrompt
		if k.Agent == codex.Name {
			prompt = codexCalibrationPrompt
		}
		rec, err := k.Execute(ctx, a.Arm, Spec{TaskName: "calibration", Instruction: prompt,
			PlainPrompt: true, Probe: "Calibration codeword: " + codeword, Task: task.Spec{Base: k.Head, Verify: []string{"true"}},
			Arm: a.Arm, Model: k.Model, Effort: k.Effort, BudgetUSD: k.Budget, Timeout: k.Timeout})
		if err != nil {
			return results, err
		}
		m := rec.Metrics
		var sandboxCheck, large, instructions string
		if k.Agent == codex.Name {
			commands, err := codex.Commands(filepath.Join(rec.RecordsDir, agent.Transcript))
			if err != nil {
				return results, err
			}
			sandboxCheck, large, instructions = codexChecks(commands, m.ResultExcerpt, codeword, rec.ProbeFile)
		} else {
			transcript, err := os.Open(filepath.Join(rec.RecordsDir, "stream.jsonl"))
			if err != nil {
				return results, fmt.Errorf("calibration transcript: %w", err)
			}
			calls, err := claude.ToolCalls(transcript)
			transcript.Close()
			if err != nil {
				return results, err
			}
			sandboxCheck, large, instructions = judgeChecks(calls, m.ResultExcerpt, codeword, rec.ProbeFile)
		}
		agentName := ""
		if k.Agent == codex.Name {
			agentName = codex.Name
		}
		c := Calibration{Agent: agentName, Arm: a.Arm.Name, Snapshot: a.Arm.Snapshot, RunID: rec.ID, Outcome: rec.Outcome, FirstRequest: m.FirstRequest,
			EstimatedContext: claudectx.EstimateTokens(resolved.StartupBytes()), CLIVersion: m.CLIVersion, Model: m.Model,
			RequestedModel: k.Model, SignIn: k.SignIn, Tools: m.Tools, Skills: without(m.Skills, rec.ProjectSkills),
			SlashCommands: without(m.SlashCommands, rec.ProjectSkills, rec.ProjectCommands), Drift: rec.Drift, CostUSD: rec.Spend().AgentUSD}
		c.Sandbox, c.LargeOutput, c.Instructions = sandboxCheck, large, instructions
		results = append(results, c)
		if !c.Healthy() {
			continue
		}
		if err := k.Save(context.WithoutCancel(ctx), c); err != nil {
			return results, err
		}
	}
	return results, nil
}

// WriteCalibrations reports each arm and compares the measured context sizes with Agentium's estimates.
func WriteCalibrations(out io.Writer, st term.Style, results []Calibration) error {
	table := term.NewTable(st, term.Left("ARM"), term.Left("OUTCOME"), term.Left("SANDBOX"), term.Left("LARGE OUTPUT"), term.Left("INSTRUCTIONS"),
		term.Right("FIRST REQUEST"), term.Right("ESTIMATED CTX"), term.Right("TOOLS"), term.Right("SKILLS"), term.Right("COST"))
	for _, c := range results {
		table.Row(c.Arm, st.Status(c.Outcome), st.Status(c.Sandbox), st.Status(c.LargeOutput), st.Status(c.Instructions),
			strconv.FormatInt(c.FirstRequest, 10), strconv.Itoa(c.EstimatedContext), strconv.Itoa(len(c.Tools)), strconv.Itoa(len(c.Skills)),
			fmt.Sprintf("$%.3f", c.CostUSD))
		for _, d := range c.Drift {
			table.Line(fmt.Sprintf("  %s %s", st.Warn("unfair:"), d))
		}
	}
	if err := table.Write(out); err != nil {
		return err
	}
	fmt.Fprintln(out, st.Note("Checks rest on the transcript: SANDBOX, the Bash output; LARGE OUTPUT, a read of the output Claude Code saved;"))
	fmt.Fprintln(out, st.Note("INSTRUCTIONS, the codeword Agentium added to the arm's instruction file, repeated without reading that file."))
	if len(results) > 0 {
		label := "Claude Code"
		if results[0].Agent == codex.Name {
			label = "Codex"
		}
		fmt.Fprintf(out, "%s %s, %s. The first request also holds %s's own system prompt and tools; between arms:\n",
			label, term.OrNone(results[0].CLIVersion), term.OrNone(results[0].Model), label)
	}
	base := results[0]
	for _, c := range results[1:] {
		// Estimates count about four bytes per token; real counts run higher for text dense with paths and links, and
		// Claude Code wraps each file. Experiments report the measured size; this ratio says how far off estimates are.
		measured, estimated := c.FirstRequest-base.FirstRequest, int64(c.EstimatedContext-base.EstimatedContext)
		ratio := "n/a"
		if estimated != 0 {
			ratio = fmt.Sprintf("%.2f", float64(measured)/float64(estimated))
		}
		fmt.Fprintf(out, "  %s: measured %+d tokens, estimated %+d (measured/estimated %s)\n", c.Arm, measured, estimated, ratio)
	}
	return nil
}
