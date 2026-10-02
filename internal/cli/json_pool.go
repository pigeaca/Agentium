package cli

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/pigeaca/agentium/internal/mine"
	"github.com/pigeaca/agentium/internal/pool"
	"github.com/pigeaca/agentium/internal/task"
)

// The documents of pool update and pool status (the --json contract: docs/guide.md, "Scripting and automation"). They
// are this package's own structs, so a change in internal/pool cannot change a public key by accident.

// healthDoc is the pool's health: counts of the project's tasks, the last pass's end and the oldest valid base (null
// when there is none).
type healthDoc struct {
	Total           int        `json:"total"`
	Valid           int        `json:"valid"`
	Weak            int        `json:"weak"` // of the valid ones: with a weak-tests warning
	Flaky           int        `json:"flaky"`
	Invalid         int        `json:"invalid"`
	Unchecked       int        `json:"unchecked"`
	Unvalidated     int        `json:"unvalidated"`
	AwaitingReview  int        `json:"awaiting_review"` // active tasks whose instruction needs a review
	Retired         int        `json:"retired"`
	LastPass        *time.Time `json:"last_pass"`
	OldestValidBase *time.Time `json:"oldest_valid_base"`
}

func healthDocOf(h pool.Health) healthDoc {
	doc := healthDoc{Total: h.Total, Valid: h.Valid, Weak: h.Weak, Flaky: h.Flaky, Invalid: h.Invalid, Unchecked: h.Unchecked,
		Unvalidated: h.Unvalidated, AwaitingReview: h.AwaitingReview, Retired: h.Retired}
	if !h.LastPass.IsZero() {
		at := h.LastPass.UTC()
		doc.LastPass = &at
	}
	if !h.OldestValidBase.IsZero() {
		at := h.OldestValidBase.UTC()
		doc.OldestValidBase = &at
	}
	return doc
}

type poolStatusDoc struct {
	header
	Health healthDoc `json:"health"`
}

// staleDoc is a stale task: re-validated (status is its new status, null when the validation was not stored: problem
// says why; or, in a dry run and when re-validations were skipped, null with an empty problem), or kept as it is for
// the experiments that use it.
type staleDoc struct {
	Name        string   `json:"name"`
	Reasons     []string `json:"reasons"`
	Status      *string  `json:"status"`
	Problem     string   `json:"problem"`
	Experiments []string `json:"experiments"` // the locked experiments that use it: kept only
}

type retiredDoc struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

type heldDoc struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// poolUpdateDoc is pool update's document. In a dry run, candidates are those it would try (up to --limit), tasks the
// mined tasks it would validate, revalidated, kept and retired what it would do; imported, accepted and held_back are
// empty and watermark_moved false.
type poolUpdateDoc struct {
	header
	DryRun           bool               `json:"dry_run"`
	Ref              string             `json:"ref"`
	Head             string             `json:"head"`
	CommitsRead      int                `json:"commits_read"`   // since the last pass, inside the window
	OutsideWindow    int                `json:"outside_window"` // read past: older than the window
	Complete         bool               `json:"complete"`       // the scan read every new commit (false: bounded, the next pass reads on)
	UnknownWatermark []string           `json:"unknown_watermark"`
	CandidatesFound  int                `json:"candidates_found"`
	Candidates       []mineCandidateDoc `json:"candidates"`
	// SetAside counts the commits not taken, per reason: the scan's reasons, those outside the window, and candidates
	// whose base is too old or whose change was mined before.
	SetAside    map[string]int `json:"set_aside"`
	Verify      []string       `json:"verify"` // the commands mined tasks verify with
	Imported    []string       `json:"imported"`
	Tasks       []batchRowDoc  `json:"tasks"` // validations of mined tasks, and candidates that failed to import
	Revalidated []staleDoc     `json:"revalidated"`
	// RevalidationsSkipped: an experiment is running, so the stale tasks in revalidated were not re-validated.
	RevalidationsSkipped bool         `json:"revalidations_skipped"`
	Kept                 []staleDoc   `json:"kept"`
	Retired              []retiredDoc `json:"retired"`
	Accepted             []string     `json:"accepted"`
	HeldBack             []heldDoc    `json:"held_back"`
	WatermarkMoved       bool         `json:"watermark_moved"`
	Interrupted          bool         `json:"interrupted"`
	Health               healthDoc    `json:"health"`
	Warnings             []string     `json:"warnings"`
}

func candidateDocs(cands []mine.Candidate) []mineCandidateDoc {
	docs := []mineCandidateDoc{}
	for _, c := range cands {
		parts := make([]string, len(c.Reasons))
		for i, p := range c.Reasons {
			parts[i] = p.String()
		}
		docs = append(docs, mineCandidateDoc{Commit: c.Hash, Score: c.Score, Subject: c.Subject, Tests: len(c.Tests), Code: len(c.Code),
			Lines: c.Lines, Reasons: parts})
	}
	return docs
}

// setAsideDoc is the set-aside counts by reason, leaving out the reasons with none.
func setAsideDoc(rows []setAside) map[string]int {
	doc := map[string]int{}
	for _, r := range rows {
		if r.count > 0 {
			doc[r.reason] = r.count
		}
	}
	return doc
}

// baseDocument is the part of both documents that the scan and the plan give.
func (p *poolPass) baseDocument(health pool.Health) poolUpdateDoc {
	scan := p.scan
	return poolUpdateDoc{header: p.env.hdr(), Ref: p.ref, Head: p.head, CommitsRead: scan.Result.Scanned, OutsideWindow: scan.Old,
		Complete: scan.Scanned.Complete, UnknownWatermark: list(scan.Scanned.Unknown), Candidates: []mineCandidateDoc{}, SetAside: map[string]int{},
		Verify: list(p.verify), Imported: []string{},
		Tasks: []batchRowDoc{}, Revalidated: []staleDoc{}, Kept: []staleDoc{}, Retired: []retiredDoc{}, Accepted: []string{}, HeldBack: []heldDoc{},
		Health: healthDocOf(health), Warnings: []string{}}
}

func keptDocs(plan pool.Plan) []staleDoc {
	docs := []staleDoc{}
	for _, k := range plan.Kept {
		docs = append(docs, staleDoc{Name: k.Task.Name, Reasons: list(k.Stale.Reasons), Experiments: list(k.Experiments)})
	}
	return docs
}

func (p *poolPass) dryRunDocument(ctx context.Context, prev pool.Preview[mine.Candidate], top []mine.Candidate, plan pool.Plan, health pool.Health, busy bool) poolUpdateDoc {
	doc := p.baseDocument(health)
	doc.DryRun, doc.Head, doc.CandidatesFound, doc.Candidates = true, prev.Head, len(prev.Candidates), candidateDocs(top)
	doc.SetAside = setAsideDoc(p.setAside(len(prev.Candidates)))
	doc.UnknownWatermark = list(prev.Unknown)
	var rows []batchRow
	for i := range prev.Unvalidated {
		rows = append(rows, batchRow{task: &prev.Unvalidated[i]})
	}
	doc.Tasks = batchRowDocs(ctx, p.env, p.w, rows)
	for _, r := range plan.Revalidate {
		doc.Revalidated = append(doc.Revalidated, staleDoc{Name: r.Task.Name, Reasons: list(r.Stale.Reasons), Experiments: []string{}})
	}
	doc.RevalidationsSkipped, doc.Kept = busy, keptDocs(plan)
	for _, r := range plan.Retire {
		doc.Retired = append(doc.Retired, retiredDoc{Name: r.Task.Name, Reason: r.Reason})
	}
	if prev.Unreadable != "" {
		doc.Warnings = append(doc.Warnings, "the pool's state file is unreadable: a pass sets it aside and starts over")
	}
	if p.noMining {
		doc.Warnings = append(doc.Warnings, noMiningNote)
	}
	return doc
}

// document is a finished (or interrupted) pass's document.
func (p *poolPass) document(ctx context.Context, res pool.PassResult, accepted []string, held map[string]string, health pool.Health, interrupted bool) poolUpdateDoc {
	doc := p.baseDocument(health)
	doc.Head, doc.CandidatesFound, doc.WatermarkMoved, doc.Interrupted = res.Head, res.Candidates, res.Moved, interrupted
	doc.UnknownWatermark = list(res.Unknown)
	doc.Candidates = candidateDocs(p.tried)
	doc.SetAside = setAsideDoc(p.setAside(res.Candidates))
	for _, t := range res.Imported {
		doc.Imported = append(doc.Imported, t.Name)
	}
	rows := batchRows(p.validated)
	for _, f := range p.imp.Failed {
		rows = append(rows, batchRow{name: taskName(f.Candidate.Subject, f.Candidate.Hash), commit: f.Candidate.Hash, problem: "not imported: " + f.Err.Error()})
	}
	doc.Tasks = batchRowDocs(ctx, p.env, p.w, rows)
	m := p.maint
	for i, r := range m.plan.Revalidate {
		d := staleDoc{Name: r.Task.Name, Reasons: list(r.Stale.Reasons), Experiments: []string{}}
		if i < len(m.revalidated) && (i >= len(m.notRun) || !m.notRun[i]) {
			if br := m.revalidated[i]; br.Validated {
				status := task.ValidationOf(br.Task).Status
				d.Status = &status
			} else {
				d.Problem = p.env.redact(br.Problem())
			}
		}
		doc.Revalidated = append(doc.Revalidated, d)
	}
	doc.RevalidationsSkipped, doc.Kept = m.skipped, keptDocs(m.plan)
	for _, r := range m.retired {
		doc.Retired = append(doc.Retired, retiredDoc{Name: r.Task.Name, Reason: r.Reason})
	}
	doc.Accepted = list(accepted)
	for _, name := range slices.Sorted(maps.Keys(held)) {
		doc.HeldBack = append(doc.HeldBack, heldDoc{Name: name, Reason: held[name]})
	}
	if res.Unreadable != "" {
		doc.Warnings = append(doc.Warnings, "the pool's state file was unreadable and was set aside")
	}
	if m.skipped {
		doc.Warnings = append(doc.Warnings, "an experiment is running: some stale tasks were not re-validated (status null, no problem); the next pass re-validates them")
	}
	if p.acceptRefused {
		doc.Warnings = append(doc.Warnings, "--accept-mined accepted nothing: the pool's state file was unreadable")
	}
	if p.noMining {
		doc.Warnings = append(doc.Warnings, noMiningNote)
	}
	return doc
}
