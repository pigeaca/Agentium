package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
)

// The --json documents of the run commands. They carry no path: not the records folder, the workspace or Claude Code's.

// runInfo is one run as run once and run show describe it.
type runInfo struct {
	ID      string `json:"id"`
	Task    string `json:"task"`
	Arm     string `json:"arm"`
	Model   string `json:"model"`
	Effort  string `json:"effort"` // as asked for; empty is the CLI's default
	SignIn  string `json:"sign_in"`
	Outcome string `json:"outcome"`
	// Passed is the hidden verification's result; null when it did not run.
	Passed             *bool       `json:"passed"`
	CostUSD            float64     `json:"cost_usd"` // the agent's; JudgeCostUSD is the judge's, when it ran
	JudgeCostUSD       float64     `json:"judge_cost_usd"`
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
	Started            time.Time   `json:"started"`
	Finished           time.Time   `json:"finished"`
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
	return runInfo{ID: rec.ID, Task: rec.Task, Arm: rec.Arm, Model: rec.Model, Effort: rec.Effort, SignIn: rec.SignIn, Outcome: rec.Outcome,
		Passed: rec.Passed, CostUSD: spend.AgentUSD, JudgeCostUSD: spend.JudgeUSD, CostEstimated: rec.CostEstimated, Turns: rec.Metrics.Turns,
		DurationMS: rec.Metrics.DurationMS, FirstRequestTokens: rec.Metrics.FirstRequest, CLIVersion: rec.Metrics.CLIVersion,
		PermissionMode: rec.Metrics.PermissionMode, Tools: len(rec.Metrics.Tools), Skills: rec.Metrics.SkillCount, Behavior: behaviorOf(rec.Behavior),
		Drift: env.redactAll(rec.Drift), Notes: env.redactAll(rec.Notes), Started: rec.Started, Finished: rec.Finished}
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
