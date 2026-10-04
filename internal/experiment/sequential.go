package experiment

import (
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/stats"
)

// Sequential is a seq-v1 experiment's group-sequential design, fixed in its lock (the wave-3 statistics note, §3).
// Stage k holds the tasks after look k−1's count up to Looks[k−1] of the seeded task order; the analysis looks once a
// stage is settled, and the experiment stops at the first look whose cost verdict is decisive. Every field is recorded
// so a resume can refuse a design this Agentium would not run the same way (Check).
type Sequential struct {
	Looks            []int   `json:"looks"`             // tasks counted by each look as planned; the last is the maximum
	Alpha            float64 `json:"alpha"`             // two-sided, for improved and regressed
	EquivalenceAlpha float64 `json:"equivalence_alpha"` // one-sided, for each of equivalence's two tests
	Spending         string  `json:"spending"`          // stats.SpendingOBF, for both
	// Futility is the conditional power below which an interim look without a verdict stops; 0 turns it off. It is
	// non-binding: the boundaries ignore it.
	Futility float64 `json:"futility"`
	// EfficacyLevels and EquivalenceLevels are the two-sided nominal levels of each planned look's intervals. A look
	// that counts fewer tasks than planned (runs failed) gets levels computed from the tasks it counts.
	EfficacyLevels    []float64 `json:"efficacy_levels"`
	EquivalenceLevels []float64 `json:"equivalence_levels"`
}

// NewSequential is the seq-v1 design of an experiment of tasks tasks, with or without futility stops.
func NewSequential(tasks int, futility bool) (Sequential, error) {
	if tasks < 1 || tasks > stats.SeqMaxTasks {
		return Sequential{}, fmt.Errorf("method %s takes 1 to %d tasks, not %d", MethodSeq, stats.SeqMaxTasks, tasks)
	}
	s := Sequential{Looks: stats.SeqLooks(tasks), Alpha: stats.SeqAlpha, EquivalenceAlpha: stats.SeqEquivalenceAlpha, Spending: stats.SpendingOBF}
	if futility {
		s.Futility = stats.SeqFutility
	}
	looks, err := s.planned()
	if err != nil {
		return Sequential{}, err
	}
	for _, l := range looks {
		s.EfficacyLevels = append(s.EfficacyLevels, l.EffLevel)
		s.EquivalenceLevels = append(s.EquivalenceLevels, l.EqLevel)
	}
	return s, nil
}

// planned computes the looks as planned, from the recorded looks, alphas and spending.
func (s Sequential) planned() ([]stats.SeqLook, error) {
	if len(s.Looks) == 0 {
		return nil, errors.New("a sequential design without looks")
	}
	return stats.SequentialLooks(s.Looks, s.Looks[len(s.Looks)-1], true, s.Alpha, s.EquivalenceAlpha)
}

// FinalBound is the planned final look's efficacy boundary, which conditional power aims at.
func (s Sequential) FinalBound() float64 {
	return stats.NormalQuantile((1 + s.EfficacyLevels[len(s.EfficacyLevels)-1]) / 2)
}

// StageEnd is the schedule position where stage k (from 1) ends, exclusive: one run per task and arm, so two slots a
// task.
func (s Sequential) StageEnd(k int) int { return 2 * s.Looks[k-1] }

// StageOf is the stage (from 1) of the slot at position pos.
func (s Sequential) StageOf(pos int) int {
	for k := range s.Looks {
		if pos < s.StageEnd(k+1) {
			return k + 1
		}
	}
	return len(s.Looks)
}

// stage assigns each slot its stage. The schedule is today's, of one repeat: the seeded task order, each pair's arms
// adjacent in a seeded order, so stage k is the pairs of the tasks after look k−1's count up to look k's.
func (s Sequential) stage(slots []Slot) []Slot {
	for i := range slots {
		slots[i].Stage = s.StageOf(slots[i].Position)
	}
	return slots
}

// levelTolerance is how far a lock's recorded levels may be from what this build computes for a resume to go on.
const levelTolerance = 1e-6

// checkSequential refuses a seq-v1 lock this Agentium would not run and analyse as it was locked: a design that is not
// seq-v1's (looks, alphas, spending, futility, one run per task and arm), levels this Agentium computes differently, or
// a schedule whose stages do not match the looks.
func (l Lock) checkSequential() error {
	s := l.Sequential
	refuse := func(why string) error {
		return fmt.Errorf("the experiment's %s lock cannot continue: %s", MethodSeq, why)
	}
	switch {
	case s == nil:
		return refuse("it records no sequential design")
	case l.Design.Method != MethodSeq || l.Design.Repeats != 1:
		return refuse("its design is not a sequential one")
	case !slices.Equal(s.Looks, stats.SeqLooks(len(l.Tasks))):
		return refuse(fmt.Sprintf("its looks %v are not %s's for %d tasks (%v)", s.Looks, MethodSeq, len(l.Tasks), stats.SeqLooks(len(l.Tasks))))
	case s.Alpha != stats.SeqAlpha || s.EquivalenceAlpha != stats.SeqEquivalenceAlpha || s.Spending != stats.SpendingOBF:
		return refuse(fmt.Sprintf("its alpha %g, equivalence alpha %g and spending %q are not %s's", s.Alpha, s.EquivalenceAlpha, s.Spending, MethodSeq))
	case s.Futility != 0 && s.Futility != stats.SeqFutility:
		return refuse(fmt.Sprintf("its futility threshold %g is not %s's", s.Futility, MethodSeq))
	case len(l.Schedule) != 2*s.Looks[len(s.Looks)-1]:
		return refuse("its schedule does not hold one run per task and arm")
	}
	want, err := NewSequential(len(l.Tasks), s.Futility > 0)
	if err != nil {
		return refuse(err.Error())
	}
	// The analysis uses the recorded levels for planned looks, so a harmless numerical change (a finer grid, another
	// quadrature) must not refuse every resume: levels within levelTolerance pass. A change that moves them further is
	// a new method (seq-v2), and stats' golden test (TestSeqLevelsGolden) fails before it ships.
	if len(s.EfficacyLevels) != len(want.EfficacyLevels) || len(s.EquivalenceLevels) != len(want.EquivalenceLevels) {
		return refuse("its looks' levels are not one per look")
	}
	for k := range want.EfficacyLevels {
		if math.Abs(s.EfficacyLevels[k]-want.EfficacyLevels[k]) > levelTolerance || math.Abs(s.EquivalenceLevels[k]-want.EquivalenceLevels[k]) > levelTolerance {
			return refuse("its looks' levels are not the ones this Agentium computes")
		}
	}
	for _, slot := range l.Schedule {
		if slot.Stage != s.StageOf(slot.Position) {
			return refuse(fmt.Sprintf("slot %d is in stage %d, not %d", slot.Position, slot.Stage, s.StageOf(slot.Position)))
		}
	}
	return nil
}

// Look decisions.
const (
	LookContinue = "continue" // no verdict: the next stage runs
	LookStop     = "stop"     // a decisive cost verdict: the experiment ends here
	LookFutility = "futility" // no verdict, and too little chance of one by the last look: the experiment ends here
	LookFinal    = "final"    // the last look: the experiment ends with its verdict, decisive or not
)

// Look is one look of a seq-v1 experiment: made once its stage settled, on exactly the runs of the stages up to it.
type Look struct {
	Look    int `json:"look"`    // from 1
	Planned int `json:"planned"` // tasks in the stages up to it
	Counted int `json:"counted"` // tasks with cost in both arms among them
	// Analysed is false when the look gave no verdict for want of tasks: fewer than the cost floor counted, or none
	// since the last analysed look. Its levels are then zero.
	Analysed bool    `json:"analysed"`
	Fraction float64 `json:"fraction,omitempty"` // counted over the planned maximum
	EffLevel float64 `json:"efficacy_level,omitempty"`
	EqLevel  float64 `json:"equivalence_level,omitempty"`
	Verdict  string  `json:"verdict,omitempty"` // cost's
	// Interval is cost's ratio (B / A) at the look's efficacy level, the wider of the bootstrap and the t-interval on
	// each side, as the verdict reads it: a repeated confidence interval, valid at this look whatever came before.
	// EqInterval is the same at the look's equivalence level, which an "equivalent" verdict reads.
	Interval   *stats.Interval `json:"interval,omitempty"`
	EqInterval *stats.Interval `json:"equivalence_interval,omitempty"`
	Z          float64         `json:"z,omitempty"` // the per-task log differences' t-statistic
	// ConditionalPower is the chance, under the trend so far, of crossing the final boundary: an analysed interim look
	// without a verdict, with futility on.
	ConditionalPower *float64 `json:"conditional_power,omitempty"`
	Decision         string   `json:"decision"`
	Note             string   `json:"note,omitempty"` // why a look was not analysed
}

// SeqStatus is where a seq-v1 experiment stands: the looks made so far, in order, and whether it has ended.
type SeqStatus struct {
	Planned []int  `json:"planned"` // each look's planned task count
	Looks   []Look `json:"looks"`   // made so far: their stages are settled
	// Ended is the last look's decision when it ends the experiment (LookStop, LookFutility or LookFinal); empty while
	// stages remain to run.
	Ended string `json:"ended,omitempty"`
	// Reported is the look whose analysis the results show (the last analysed one), 0 when none was analysed yet: the
	// results then cover every run so far, and the primary metric has no verdict.
	Reported int `json:"reported"`
	// NextStage is the stage to run next (from 1) while the experiment has not ended.
	NextStage int `json:"next_stage,omitempty"`
}

// ReportedLook is the look the results come from, or nil.
func (s SeqStatus) ReportedLook() *Look {
	for i := range s.Looks {
		if s.Looks[i].Look == s.Reported {
			return &s.Looks[i]
		}
	}
	return nil
}

// Describe is the status in words, as the experiment's status note and the report say it: "stopped at look 1 of 3 (8
// tasks): cost improved".
func (s SeqStatus) Describe() string {
	of, made := len(s.Planned), len(s.Looks)
	l := s.ReportedLook()
	switch {
	case s.Ended == LookStop && l != nil:
		return fmt.Sprintf("stopped at look %d of %d (%d tasks): cost %s", l.Look, of, l.Counted, l.Verdict)
	case s.Ended == LookFutility && l != nil:
		cp := 0.0
		if l.ConditionalPower != nil {
			cp = *l.ConditionalPower
		}
		return fmt.Sprintf("stopped at look %d of %d (%d tasks) for futility: cost is unlikely to reach a verdict by the last look (conditional power %.0f%%)",
			l.Look, of, l.Counted, 100*cp)
	case s.Ended != "" && l != nil && l.Look == made:
		return fmt.Sprintf("ended at its last look, %d of %d (%d tasks): cost %s", made, of, l.Counted, l.Verdict)
	case s.Ended != "":
		return fmt.Sprintf("ended at its last look, %d of %d, without a verdict", made, of)
	case made == 0:
		return fmt.Sprintf("no look yet: look 1 of %d comes once the first %d tasks are settled", of, s.Planned[0])
	}
	return fmt.Sprintf("look %d of %d made (%s); look %d comes once the first %d tasks are settled", made, of, s.Looks[made-1].Decision, made+1, s.Planned[made])
}

// analyzeSequential analyses a seq-v1 experiment look by look (SequentialStatus) and returns the reported look's
// analysis with the looks attached.
func analyzeSequential(l Lock, runs []RunData) (Analysis, error) {
	status, out, err := SequentialStatus(l, runs)
	if err != nil {
		return Analysis{}, err
	}
	out.Sequential = &status
	return out, nil
}

// SequentialStatus makes a seq-v1 experiment's looks from its stored runs, as the executor does between stages: for
// each stage in order whose every slot is settled or out of attempts, the look analyses exactly the runs of the stages
// up to it: at the lock's recorded levels while every look so far counted its planned tasks, so `experiment run` and
// `experiment report` always read the same levels; else at levels from the tasks counted (stats.SequentialLooks: earlier
// looks keep theirs, the last spends the remainder). The first look with a decisive cost verdict, a futility stop or the last look ends it; the first stage
// not yet settled is the one to run next. It is a function of the lock and the runs alone, so a resume after a crash
// makes the same looks: none is repeated on other data, and none is skipped.
//
// It also returns the analysis the results show: the last analysed look's, or, before any, every run's with no verdict
// on the primary metric. So an experiment stopped between looks (by its budget, the usage limit or the user) keeps its
// last look's verdict.
func SequentialStatus(l Lock, runs []RunData) (SeqStatus, Analysis, error) {
	s := l.Sequential
	if s == nil || len(s.Looks) == 0 || len(s.EfficacyLevels) != len(s.Looks) || len(s.EquivalenceLevels) != len(s.Looks) || len(l.Design.Arms) != 2 {
		return SeqStatus{}, Analysis{}, errors.New("analyze: a seq-v1 lock without its sequential design")
	}
	status := SeqStatus{Planned: slices.Clone(s.Looks)}
	done := slotsDone(l, runs)
	a, b := l.Design.Arms[0].Name, l.Design.Arms[1].Name
	maximum := s.Looks[len(s.Looks)-1]
	var counts []int // the analysed looks' counted tasks
	var reported Analysis
	for k := 1; k <= len(s.Looks); k++ {
		end := min(s.StageEnd(k), len(done))
		if slices.Contains(done[:end], false) {
			status.NextStage = k
			break
		}
		var prefix []RunData
		for _, r := range runs {
			if r.Slot < end {
				prefix = append(prefix, r)
			}
		}
		look := Look{Look: k, Planned: s.Looks[k-1], Counted: costPairs(prefix, a, b)}
		final := k == len(s.Looks)
		switch {
		case look.Counted < MinTasksCost:
			look.Note = fmt.Sprintf("%d task(s) have cost in both arms, below the floor of %d", look.Counted, MinTasksCost)
		case len(counts) > 0 && look.Counted <= counts[len(counts)-1]:
			look.Note = "no task was counted since the last analysed look"
		default:
			counts = append(counts, look.Counted)
			cur := stats.SeqLook{Tasks: look.Counted, Fraction: float64(look.Counted) / float64(maximum),
				EffLevel: s.EfficacyLevels[k-1], EqLevel: s.EquivalenceLevels[k-1]}
			if !slices.Equal(counts, s.Looks[:k]) { // tasks lost: levels the lock did not plan, from the counts
				looks, err := stats.SequentialLooks(counts, maximum, final, s.Alpha, s.EquivalenceAlpha)
				if err != nil {
					return SeqStatus{}, Analysis{}, err
				}
				cur = looks[len(looks)-1]
			}
			an, z, err := analyze(l, prefix, lookLevels{eff: cur.EffLevel, eq: cur.EqLevel, look: k})
			if err != nil {
				return SeqStatus{}, Analysis{}, err
			}
			look.Analysed, look.Fraction, look.EffLevel, look.EqLevel, look.Z = true, cur.Fraction, cur.EffLevel, cur.EqLevel, z
			for _, r := range an.Results {
				if r.Role == RolePrimary {
					eff, eq := wider(r.Boot95, r.T95), wider(r.Boot90, r.T90)
					look.Verdict, look.Interval, look.EqInterval = r.Verdict, &eff, &eq
				}
			}
			reported, status.Reported = an, k
		}
		switch {
		case final: // the last look ends it, whatever its verdict
			look.Decision = LookFinal
		case look.Analysed && decisive(look.Verdict):
			look.Decision = LookStop
		case look.Analysed && s.Futility > 0:
			cp := stats.ConditionalPower(look.Z, look.Fraction, s.FinalBound())
			look.ConditionalPower = &cp
			look.Decision = LookContinue
			if cp < s.Futility {
				look.Decision = LookFutility
			}
		default:
			look.Decision = LookContinue
		}
		status.Looks = append(status.Looks, look)
		if look.Decision != LookContinue {
			status.Ended = look.Decision
			break
		}
	}
	if status.Reported == 0 {
		an, _, err := analyze(l, runs, lookLevels{eff: s.EfficacyLevels[0], eq: s.EquivalenceLevels[0], noVerdict: true})
		if err != nil {
			return SeqStatus{}, Analysis{}, err
		}
		reported = an
	}
	return status, reported, nil
}

// wider spans two intervals, as stats.Decide reads them: the bootstrap's estimate, and the wider bound on each side.
func wider(boot, t stats.Interval) stats.Interval {
	return stats.Interval{Estimate: boot.Estimate, Low: math.Min(boot.Low, t.Low), High: math.Max(boot.High, t.High)}
}

// slotsDone reports, per schedule position, whether its slot is settled or out of attempts, as Execute counts them:
// a settling run settles it; other runs, cancelled ones aside, are attempts. A judge-graded run whose grade is pending
// (RunData.Pending) leaves its slot not done, though Execute never runs it again: its grade may still change, so no
// look reads it before it is graded or left ungraded for good.
func slotsDone(l Lock, runs []RunData) []bool {
	settled := make([]bool, len(l.Schedule))
	attempts := make([]int, len(l.Schedule))
	for _, r := range runs {
		if r.Slot < 0 || r.Slot >= len(settled) {
			continue
		}
		switch {
		case Settles(r.Outcome) && r.Pending:
		case Settles(r.Outcome):
			settled[r.Slot] = true
		case r.Outcome != claude.OutcomeCancelled:
			attempts[r.Slot]++
		}
	}
	done := make([]bool, len(settled))
	for i := range done {
		done[i] = settled[i] || attempts[i] >= l.MaxAttempts
	}
	return done
}

// costPairs counts the tasks with a counted cost in both arms: the tasks a look's cost analysis pairs.
func costPairs(runs []RunData, a, b string) int {
	has := map[string]map[string]bool{}
	for _, r := range runs {
		if Fair(r.Outcome) && r.graded() && r.CostUSD > 0 { // as the cost analysis counts them
			if has[r.Task] == nil {
				has[r.Task] = map[string]bool{}
			}
			has[r.Task][r.Arm] = true
		}
	}
	n := 0
	for _, arms := range has {
		if arms[a] && arms[b] {
			n++
		}
	}
	return n
}

// decisive reports whether a look's cost verdict ends a seq-v1 experiment.
func decisive(verdict string) bool {
	return slices.Contains([]string{stats.Improved, stats.ImprovedSmall, stats.Regressed, stats.Equivalent}, verdict)
}

// DescribeLooks is a seq-v1 design's looks in words: "looks after 8, 12 and 16 tasks; it stops at the first look with a
// cost verdict, or for futility".
func DescribeLooks(d Design) string {
	looks := stats.SeqLooks(len(d.Tasks))
	stops := "it stops at the first look with a cost verdict, or for futility"
	if d.NoFutility {
		stops = "it stops at the first look with a cost verdict (futility stops off)"
	}
	switch {
	case looks[0] < stats.SeqFirstLook:
		return fmt.Sprintf("one look, after all %d task(s), below the cost floor of %d: cost stays exploratory", looks[0], stats.SeqFirstLook)
	case len(looks) == 1:
		return fmt.Sprintf("one look, after all %d tasks (a fixed design at the sequential level)", looks[0])
	}
	return fmt.Sprintf("looks after %s tasks; %s", countList(looks), stops)
}

// countList writes counts as "8, 12 and 16".
func countList(counts []int) string {
	out := ""
	for i, n := range counts {
		switch {
		case i == 0:
		case i == len(counts)-1:
			out += " and "
		default:
			out += ", "
		}
		out += fmt.Sprint(n)
	}
	return out
}

// PreviewCut is the true cost cut the preview's expected spend assumes besides none: 20%, as the note's §6.
const PreviewCut = 0.20

// previewPaths and previewSeed are the simulated experiments that estimate the preview's expected tasks: seeded, so
// every preview of a design of the same size gives the same figures.
const (
	previewPaths = 20000
	previewSeed  = 1
)

// SeqLookPreview is one look of a seq-v1 design's preview: what the stages up to it run and may cost, and its levels.
type SeqLookPreview struct {
	Tasks    int
	Runs     int
	CostUSD  float64 // the estimate of every run up to the look, both judges' included; zero when unknown
	WorstUSD float64 // every run up to the look, its judgement and its pair's comparison, at its cap
	EffLevel float64
	EqLevel  float64
}

// SeqPreview is what a seq-v1 design may spend: per look, at most (every stage), and on average with no true change
// and at a PreviewCut cut, from the planner's σ and τ (SigmaLogCost, TauLow).
type SeqPreview struct {
	Looks     []SeqLookPreview
	Known     bool    // every task has an estimate in both arms: the costs below are set
	MaxUSD    float64 // every stage runs: the estimate of all the design's runs
	WorstUSD  float64 // every run at its cap: what the budget must allow for
	TasksNone float64 // tasks used on average with no true change (futility stops as the design has them)
	TasksCut  float64 // tasks used on average at a PreviewCut cut in arm B
	NoneUSD   float64
	CutUSD    float64
}

// PreviewSequential sizes a seq-v1 design: each look's runs, estimate and worst case, in the schedule's seeded task
// order; the maximum, which the default budget and the reserve are sized for; and the expected spend, from simulated
// experiments at the planner's noise (stats.SequentialExpectedTasks).
func PreviewSequential(d Design, est ArmEstimates) (SeqPreview, error) {
	seq, err := NewSequential(len(d.Tasks), !d.NoFutility)
	if err != nil {
		return SeqPreview{}, err
	}
	planned, err := seq.planned()
	if err != nil {
		return SeqPreview{}, err
	}
	var order []string // the tasks in the schedule's seeded order: stage k is a prefix
	for _, s := range Schedule(d) {
		if !slices.Contains(order, s.Task) {
			order = append(order, s.Task)
		}
	}
	judgePair := 0.0 // a task's judgements and its pair's comparison
	if len(d.Tasks) > 0 {
		judgePair = d.JudgingEstimateUSD() / float64(len(d.Tasks))
	}
	p := SeqPreview{Known: true, WorstUSD: d.WorstUSD()}
	for k, n := range seq.Looks {
		cost, known := est.DesignUSD(Design{Tasks: order[:n], Repeats: 1})
		p.Known = p.Known && known
		upTo := d
		upTo.Tasks = order[:n]
		p.Looks = append(p.Looks, SeqLookPreview{Tasks: n, Runs: 2 * n, CostUSD: cost + float64(n)*judgePair, WorstUSD: upTo.WorstUSD(),
			EffLevel: planned[k].EffLevel, EqLevel: planned[k].EqLevel})
	}
	sd := math.Sqrt(2*SigmaLogCost*SigmaLogCost + TauLow*TauLow) // a task's log difference with one run per arm
	p.TasksNone = stats.SequentialExpectedTasks(planned, seq.Futility, 0, previewPaths, previewSeed)
	p.TasksCut = stats.SequentialExpectedTasks(planned, seq.Futility, math.Log(1-PreviewCut)/sd, previewPaths, previewSeed)
	meanA, okA := est[0].MeanUSD(d.Tasks)
	meanB, okB := est[1].MeanUSD(d.Tasks)
	if !p.Known || !okA || !okB {
		p.Known = false
		for i := range p.Looks {
			p.Looks[i].CostUSD = 0
		}
		return p, nil
	}
	p.MaxUSD = p.Looks[len(p.Looks)-1].CostUSD
	p.NoneUSD = p.TasksNone * (meanA + meanB + judgePair)
	p.CutUSD = p.TasksCut * (meanA + (1-PreviewCut)*meanB + judgePair)
	return p, nil
}

// DescribeLook is one look in words, for progress lines and experiment show: "look 1 of 3 (8 of 8 tasks counted):
// cost inconclusive at 99.84%; conditional power 34%: continue".
func DescribeLook(l Look, of int) string {
	head := fmt.Sprintf("look %d of %d (%d of %d tasks counted)", l.Look, of, l.Counted, l.Planned)
	if !l.Analysed {
		return fmt.Sprintf("%s: no verdict (%s): %s", head, l.Note, l.Decision)
	}
	text := fmt.Sprintf("%s: cost %s at %.2f%%", head, l.Verdict, 100*l.LevelOfVerdict())
	if l.ConditionalPower != nil {
		text += fmt.Sprintf("; conditional power %.0f%%", 100**l.ConditionalPower)
	}
	return text + ": " + l.Decision
}

// LevelOfVerdict is the level of the interval the look's verdict reads: the equivalence level for "equivalent", else
// the efficacy level.
func (l Look) LevelOfVerdict() float64 {
	if l.Verdict == stats.Equivalent {
		return l.EqLevel
	}
	return l.EffLevel
}

// IntervalOfVerdict is the interval the look's verdict reads (Interval, or EqInterval for "equivalent"); nil when the
// look was not analysed.
func (l Look) IntervalOfVerdict() *stats.Interval {
	if l.Verdict == stats.Equivalent {
		return l.EqInterval
	}
	return l.Interval
}
