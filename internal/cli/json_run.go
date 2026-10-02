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
	Passed             *bool        `json:"passed"`
	CostUSD            float64      `json:"cost_usd"` // the agent's; JudgeCostUSD is the judge's, when it ran
	JudgeCostUSD       float64      `json:"judge_cost_usd"`
	CostEstimated      bool         `json:"cost_estimated"`
	Turns              int          `json:"turns"`
	DurationMS         int64        `json:"duration_ms"`
	FirstRequestTokens int64        `json:"first_request_tokens"`
	CLIVersion         string       `json:"cli_version"`
	PermissionMode     string       `json:"permission_mode"`
	Tools              int          `json:"tools"`
	Skills             int          `json:"skills"`
	Behavior           run.Behavior `json:"behavior"`
	Drift              []string     `json:"drift"`
	Notes              []string     `json:"notes"`
	Started            time.Time    `json:"started"`
	Finished           time.Time    `json:"finished"`
}

func runInfoOf(rec run.Record) runInfo {
	spend := rec.Spend()
	return runInfo{ID: rec.ID, Task: rec.Task, Arm: rec.Arm, Model: rec.Model, Effort: rec.Effort, SignIn: rec.SignIn, Outcome: rec.Outcome,
		Passed: rec.Passed, CostUSD: spend.AgentUSD, JudgeCostUSD: spend.JudgeUSD, CostEstimated: rec.CostEstimated, Turns: rec.Metrics.Turns,
		DurationMS: rec.Metrics.DurationMS, FirstRequestTokens: rec.Metrics.FirstRequest, CLIVersion: rec.Metrics.CLIVersion,
		PermissionMode: rec.Metrics.PermissionMode, Tools: len(rec.Metrics.Tools), Skills: rec.Metrics.SkillCount, Behavior: rec.Behavior,
		Drift: list(rec.Drift), Notes: list(rec.Notes), Started: rec.Started, Finished: rec.Finished}
}

type runOnceDoc struct {
	header
	Run runInfo `json:"run"`
}

type runShowDoc struct {
	header
	Run        runInfo `json:"run"`
	Kind       string  `json:"kind"` // task | calibration
	Experiment *struct {
		Name    string `json:"name"`
		Slot    int    `json:"slot"` // from 0
		Attempt int    `json:"attempt"`
	} `json:"experiment"`
	Files []string `json:"files"` // the record's file names
	// Diff, SetupLog and VerifyLog are set by --diff and --log: the file's text, or null when the file does not exist.
	Diff      *string `json:"diff,omitempty"`
	SetupLog  *string `json:"setup_log,omitempty"`
	VerifyLog *string `json:"verify_log,omitempty"`
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
func runShowDocument(ctx context.Context, w *workspace, stored store.Run, rec run.Record, diff, logs bool) runShowDoc {
	doc := runShowDoc{header: hdr("run show"), Run: runInfoOf(rec), Kind: stored.Kind, Files: []string{}}
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
		doc.Experiment = &struct {
			Name    string `json:"name"`
			Slot    int    `json:"slot"`
			Attempt int    `json:"attempt"`
		}{name, stored.Slot, stored.Attempt}
	}
	if entries, err := os.ReadDir(rec.RecordsDir); err == nil {
		for _, e := range entries {
			doc.Files = append(doc.Files, e.Name())
		}
	}
	read := func(name string) *string {
		data, err := os.ReadFile(filepath.Join(rec.RecordsDir, name))
		if err != nil {
			return nil
		}
		text := string(data)
		return &text
	}
	if diff {
		doc.Diff = read("agent.diff")
	}
	if logs {
		doc.SetupLog, doc.VerifyLog = read("setup.log"), read("verify.log")
	}
	return doc
}
