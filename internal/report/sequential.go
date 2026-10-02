package report

import (
	"fmt"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/stats"
)

// A seq-v1 experiment's report: the analysis is its reported look's (experiment.SequentialStatus), so the headlines
// and the metrics are that look's, and a Looks section lists every look made. Other methods' reports are unchanged.

// seqLine is the sequential status in one sentence ("Method seq-v1: stopped at look 1 of 3 (8 tasks): cost
// improved."), or "" for other methods.
func (r Report) seqLine() string {
	s := r.Analysis.Sequential
	if s == nil {
		return ""
	}
	return fmt.Sprintf("Method %s: %s.", experiment.MethodSeq, s.Describe())
}

// lookColumns are the Looks section's columns, shared by the Markdown and terminal renderings.
var lookColumns = []string{"Look", "Tasks counted", "Cost B vs A", "Interval", "Level", "Verdict", "Conditional power", "Decision"}

// lookRows are the Looks section's rows: each look made, the cost interval its verdict reads at its own level (a
// repeated confidence interval: the efficacy level, or the equivalence level for "equivalent") and its decision.
func (r Report) lookRows() [][]string {
	s := r.Analysis.Sequential
	var rows [][]string
	for _, l := range s.Looks {
		row := []string{fmt.Sprintf("%d of %d", l.Look, len(s.Planned)), fmt.Sprintf("%d of %d", l.Counted, l.Planned)}
		if !l.Analysed {
			rows = append(rows, append(row, "-", "-", "-", "no verdict: "+l.Note, "-", l.Decision))
			continue
		}
		cp := "-"
		if l.ConditionalPower != nil {
			cp = pct(*l.ConditionalPower)
		}
		estimate, interval := "-", "-"
		if i := l.IntervalOfVerdict(); i != nil {
			estimate, interval = change(i.Estimate), fmt.Sprintf("[%s, %s]", change(i.Low), change(i.High))
		}
		rows = append(rows, append(row, estimate, interval, fmt.Sprintf("%.2f%%", 100*l.LevelOfVerdict()), l.Verdict, cp, l.Decision))
	}
	return rows
}

// looksIntro introduces the Looks section.
func (r Report) looksIntro() string {
	futility := "an interim look without a verdict stops for futility when the chance of one by the last look (conditional power) is below " +
		pct(r.Lock.Sequential.Futility)
	if r.Lock.Sequential.Futility == 0 {
		futility = "futility stops were off"
	}
	return fmt.Sprintf("Method %s looks once each stage's runs are settled, at exactly the tasks of the stages up to it, and stops at the first look "+
		"whose cost verdict is decisive; %s. Each look's cost interval is at its own level, from O'Brien–Fleming-type spending of a two-sided %.1f%% "+
		"over all looks: the wider of the bootstrap and the t-interval on each side.", experiment.MethodSeq, futility, 100*r.Lock.Sequential.Alpha)
}

// seqNotes are a seq-v1 report's honesty notes: what the verdict covers and how it was checked, the early stop's bias,
// and the runs since the reported look.
func seqNotes(rep Report, in Input) []string {
	s := rep.Analysis.Sequential
	if s == nil || rep.Lock.Sequential == nil {
		return nil
	}
	var out []string
	l := s.ReportedLook()
	if l == nil {
		out = append(out, fmt.Sprintf("No look was analysed yet, so cost has no verdict: the results cover every run so far, at look 1's levels. %s.",
			upper(s.Describe())))
	} else {
		out = append(out, fmt.Sprintf("The results are look %d's: the runs of the tasks up to it (%d of them counted), with cost's intervals at %.2f%% "+
			"(and %.2f%% for \"equivalent\") and the other metrics' at 95%% (90%%).", l.Look, l.Counted, 100*l.EffLevel, 100*l.EqLevel))
		if s.Ended == experiment.LookStop && l.Look < len(s.Planned) {
			out = append(out, "It stopped at an early look. An early stop overstates the effect's size on average: the true change is likely "+
				"smaller than the estimate. The interval is valid at the look it stopped at (a repeated confidence interval).")
		}
		if after := runsAfter(rep.Lock, in.Runs, l.Look); after > 0 {
			last := s.Looks[len(s.Looks)-1]
			next := "the next look counts them once its stage is settled"
			if last.Look > l.Look { // a later look was made on them, but gave no verdict
				next = fmt.Sprintf("look %d was made on them but gave no verdict (%s), so the results stay look %d's", last.Look, last.Note, l.Look)
			}
			out = append(out, fmt.Sprintf("%d run(s) of stages after look %d are not in its results (their spend is in the total): %s.", after, l.Look, next))
		}
	}
	out = append(out, fmt.Sprintf("A seeded simulation of method %s (up to %d tasks × 1 run, the same looks and levels; normal, skewed, heavy-tailed, "+
		"arm-specific and recorded noise) gave false differences in at most 5%% of experiments per group of scenarios without a true difference "+
		"(the wave-3 statistics note, §7). One extreme scenario, both arms strongly and oppositely skewed, gave 6.5%%.", experiment.MethodSeq, stats.SeqMaxTasks))
	return out
}

// runsAfter counts the runs in stages after look k.
func runsAfter(l experiment.Lock, runs []Run, k int) int {
	if l.Sequential == nil || k < 1 || k > len(l.Sequential.Looks) {
		return 0
	}
	end, n := l.Sequential.StageEnd(k), 0
	for _, r := range runs {
		if r.Slot >= end {
			n++
		}
	}
	return n
}

// upper capitalizes the first letter of an ASCII sentence.
func upper(s string) string {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}

// metricsIntro introduces the metrics table; a seq-v1 report says at which levels its intervals are.
func (r Report) metricsIntro() string {
	intro := "A and B: the success rate, or the geometric mean per run. B vs A is paired by task: a difference for success, a ratio of geometric means for the others."
	for _, res := range r.Analysis.Results {
		if res.Level > 0 {
			intro += fmt.Sprintf(" Intervals are at 95%%, cost's at %.2f%%, its look's level.", 100*res.Level)
		}
	}
	return intro
}

// intervalHeads are the metrics table's interval columns: at 95%, unless a seq-v1 look puts cost's at its own level.
func (r Report) intervalHeads() (boot, t string) {
	if r.Analysis.Sequential != nil {
		return "Bootstrap", "t"
	}
	return "95% bootstrap", "95% t"
}
