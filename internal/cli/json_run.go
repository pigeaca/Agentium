package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pigeaca/agentium/internal/report"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
)

// The --json documents of the run commands. They carry no path: not the records folder, the workspace or Claude Code's.

// runInfo is one run as run once and run show describe it.
type runInfo struct {
	ID      string `json:"id"`
	Task    string `json:"task"`
	Arm     string `json:"arm"`
	Agent   string `json:"agent"` // the coding agent that ran: "claude-code" (also for runs recorded before agents were named)
	Model   string `json:"model"`
	Effort  string `json:"effort"` // as asked for; empty is the CLI's default
	SignIn  string `json:"sign_in"`
	Outcome string `json:"outcome"`
	// Passed is the hidden verification's result; null when it did not run.
	Passed             *bool       `json:"passed"`
	CostUSD            float64     `json:"cost_usd"` // the agent's; JudgeCostUSD is the judge's, when it ran
	JudgeCostUSD       float64     `json:"judge_cost_usd"`
	PairJudgeCostUSD   float64     `json:"pair_judge_cost_usd"` // the pair judge's, on a pair's arm-B run
	CostEstimated      bool        `json:"cost_estimated"`
	Turns              int         `json:"turns"`
	DurationMS         int64       `json:"duration_ms"`
	FirstRequestTokens int64       `json:"first_request_tokens"`
	CLIVersion         string      `json:"cli_version"`
	PermissionMode     string      `json:"permission_mode"`
	Tools              int         `json:"tools"`
	Skills             int         `json:"skills"`
	Behavior           behaviorDoc `json:"behavior"`
	Drift              []string    `json:"drift"`
	Notes              []string    `json:"notes"`
	// Grader is where the run was graded: "host" or the sandbox's version ("sandbox-v1"); "host" for runs recorded
	// before modes. Sandbox is what the grading sandbox reported, as counts (report.SandboxRow); null on the host.
	Grader   string             `json:"grader"`
	Sandbox  *report.SandboxRow `json:"sandbox"`
	Started  time.Time          `json:"started"`
	Finished time.Time          `json:"finished"`
	// GradedBy is what graded the run: "tests", or "judge" for a run of a judge-graded task, whose JudgeGrade is the
	// judge's grading verdict, which Passed follows (unvalidated); JudgeGrade is null for every other run.
	GradedBy   string         `json:"graded_by"`
	JudgeGrade *judgeGradeDoc `json:"judge_grade"`
}

// judgeGradeDoc is a judge-graded run's grade: the majority of the judge's answers ("yes" passes), each answer, how
// many were asked for, and the reason given with the first answer that matches the majority (redacted).
type judgeGradeDoc struct {
	Fixed       string   `json:"fixed"` // yes, partly or no; empty when no repeat answered
	Answers     []string `json:"answers"`
	Requested   int      `json:"requested"`
	Reason      string   `json:"reason"`
	Model       string   `json:"model"`
	Effort      string   `json:"effort"`
	Unvalidated bool     `json:"unvalidated"` // always true: the judge's grading without tests is unvalidated
}

// behaviorDoc is run.Behavior as the public schema has it, owned here so that a storage change cannot change the schema.
type behaviorDoc struct {
	FilesChanged  int      `json:"files_changed"`
	LinesAdded    int      `json:"lines_added"`
	LinesRemoved  int      `json:"lines_removed"`
	TestsChanged  bool     `json:"tests_changed"`
	TestsRemoved  int      `json:"tests_removed"`
	ChecksChanged []string `json:"checks_changed"` // verification scripts and test-runner configuration the agent changed
	ConfigChanged []string `json:"config_changed"` // of those, the configuration the reference solution does not change
	RanTests      bool     `json:"ran_tests"`
	RanChecks     bool     `json:"ran_checks"`
	Commits       int      `json:"commits"`
	BashCommands  int      `json:"bash_commands"`
	Denials       int      `json:"denials"`
	OutsideReads  int      `json:"outside_reads"`
}

func behaviorOf(b run.Behavior) behaviorDoc {
	return behaviorDoc{FilesChanged: b.FilesChanged, LinesAdded: b.LinesAdded, LinesRemoved: b.LinesRemoved, TestsChanged: b.TestsChanged,
		TestsRemoved: b.TestsRemoved, ChecksChanged: list(b.ChecksChanged), ConfigChanged: list(b.ConfigChanged), RanTests: b.RanTests, RanChecks: b.RanChecks,
		Commits: b.Commits, BashCommands: b.BashCommands, Denials: b.Denials, OutsideReads: b.OutsideReads}
}

func runInfoOf(env Env, rec run.Record) runInfo {
	spend := rec.Spend()
	info := runInfo{ID: rec.ID, Task: rec.Task, Arm: rec.Arm, Agent: rec.AgentName(), Model: rec.Model, Effort: rec.Effort, SignIn: rec.SignIn, Outcome: rec.Outcome,
		Passed: rec.Passed, CostUSD: spend.AgentUSD, JudgeCostUSD: spend.JudgeUSD, PairJudgeCostUSD: spend.PairJudgeUSD, CostEstimated: rec.CostEstimated, Turns: rec.Metrics.Turns,
		DurationMS: rec.Metrics.DurationMS, FirstRequestTokens: rec.Metrics.FirstRequest, CLIVersion: rec.Metrics.CLIVersion,
		PermissionMode: rec.Metrics.PermissionMode, Tools: len(rec.Metrics.Tools), Skills: rec.Metrics.SkillCount, Behavior: behaviorOf(rec.Behavior),
		Drift: env.redactAll(rec.Drift), Notes: env.redactAll(rec.Notes), Grader: task.GraderOf(rec.Grader), Sandbox: report.SandboxOf(rec.Sandbox),
		Started: rec.Started, Finished: rec.Finished, GradedBy: task.GradingOf(rec.GradedBy), JudgeGrade: judgeGradeOf(env, rec)}
	if info.GradedBy == "" {
		info.GradedBy = task.GradingTests
	}
	return info
}

// judgeGradeOf is a judge-graded run's grade for the JSON documents; nil for any other run.
func judgeGradeOf(env Env, rec run.Record) *judgeGradeDoc {
	if rec.GradedBy != task.GradingJudge || rec.Judge == nil {
		return nil
	}
	v := rec.Judge
	return &judgeGradeDoc{Fixed: v.Fixed, Answers: list(v.Answers), Requested: v.Requested, Reason: env.redact(v.Reason), Model: v.Model, Effort: v.Effort,
		Unvalidated: true}
}

type runOnceDoc struct {
	header
	Run runInfo `json:"run"`
}

type runShowDoc struct {
	header
	Run        runInfo           `json:"run"`
	Kind       string            `json:"kind"`       // task | calibration
	Experiment *runExperimentDoc `json:"experiment"` // null for a run that belongs to none
	Files      []string          `json:"files"`      // the record's file names
	// Diff, SetupLog and VerifyLog are set by --diff and --log: the file's text. They are null when not asked for, and
	// when the file does not exist.
	Diff      *string `json:"diff"`
	SetupLog  *string `json:"setup_log"`
	VerifyLog *string `json:"verify_log"`
}

type runExperimentDoc struct {
	Name    string `json:"name"`
	Slot    int    `json:"slot"` // from 0
	Attempt int    `json:"attempt"`
}

type runListEntry struct {
	ID      string    `json:"id"`
	Task    string    `json:"task"` // empty for a calibration
	Kind    string    `json:"kind"`
	Arm     string    `json:"arm"`
	Outcome string    `json:"outcome"`
	Passed  *bool     `json:"passed"`
	CostUSD float64   `json:"cost_usd"`
	Started time.Time `json:"started"`
	// GradedBy is "judge" for a run of a judge-graded task (Passed is the judge's unvalidated grade); absent for a run
	// graded by tests.
	GradedBy string `json:"graded_by,omitempty"`
}

type runListDoc struct {
	header
	Runs []runListEntry `json:"runs"`
}

// runShowDocument describes a stored run; with diff or logs it adds those files' text.
func runShowDocument(ctx context.Context, env Env, w *workspace, stored store.Run, rec run.Record, diff, logs bool) runShowDoc {
	doc := runShowDoc{header: hdr("run show"), Run: runInfoOf(env, rec), Kind: stored.Kind, Files: []string{}}
	if doc.Kind == "" {
		doc.Kind = "task"
	}
	if stored.ExperimentID != 0 {
		name := fmt.Sprintf("#%d", stored.ExperimentID)
		if all, err := w.db.Experiments(ctx, w.project.ID); err == nil {
			for _, e := range all {
				if e.ID == stored.ExperimentID {
					name = e.Name
				}
			}
		}
		doc.Experiment = &runExperimentDoc{Name: name, Slot: stored.Slot, Attempt: stored.Attempt}
	}
	if entries, err := os.ReadDir(rec.RecordsDir); err == nil {
		for _, e := range entries {
			doc.Files = append(doc.Files, e.Name())
		}
	}
	read := func(name string, ours bool) *string {
		data, err := os.ReadFile(filepath.Join(rec.RecordsDir, name))
		if err != nil {
			return nil
		}
		text := string(data)
		if ours { // setup and verification logs hold Agentium's own output, which can name the workspace and data folders
			text = env.redact(text)
		}
		return &text // the diff is the agent's work: as it is
	}
	if diff {
		doc.Diff = read("agent.diff", false)
	}
	if logs {
		doc.SetupLog, doc.VerifyLog = read("setup.log", true), read("verify.log", true)
	}
	return doc
}
