package watch

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

// TerminalConfirmation proves that the user, at a terminal, confirmed these caps for this sign-in, and optionally one
// project's loops, by typing back the weekly amount. Only ConfirmAtTerminal makes a valid one: its fields are unexported, and a zero value (which another
// package can write as a literal) is refused. Grant and SetLoops need one to raise anything. Only the CLI's
// `watch enable` may call ConfirmAtTerminal (TestOnlyTheEnableCommandConfirms).
type TerminalConfirmation struct {
	caps       Caps
	signIn     SignIn
	project    ProjectLoops // what SetLoops may turn on; only with hasProject
	hasProject bool
	valid      bool
}

// ProjectLoops are a project's loops to confirm with the caps: the confirmation then lets SetLoops turn on these
// loops in this project, and no others.
type ProjectLoops struct {
	ProjectID int64
	Name      string // shown at the terminal
	Loops     Loops
}

// ErrNotConfirmed: there is no valid confirmation at a terminal for what is being granted.
var ErrNotConfirmed = errors.New("the watch's budget was not confirmed at a terminal")

// covers checks that c confirms g's caps and sign-in.
func (c *TerminalConfirmation) covers(g Grant) error {
	switch {
	case !c.valid:
		return ErrNotConfirmed
	case c.caps != g.Caps || c.signIn != g.SignIn:
		return fmt.Errorf("the confirmation was for other caps or another sign-in: %w", ErrNotConfirmed)
	}
	return nil
}

// coversLoops checks that c confirms turning on, in the project, every loop that to turns on over from.
func (c *TerminalConfirmation) coversLoops(projectID int64, from, to Loops) error {
	switch {
	case !c.valid:
		return ErrNotConfirmed
	case !c.hasProject || c.project.ProjectID != projectID:
		return fmt.Errorf("the confirmation was not for this project's loops: %w", ErrNotConfirmed)
	}
	if unconfirmed := loopRaises(c.project.Loops, Loops{Experiments: to.Experiments && !from.Experiments, Drift: to.Drift && !from.Drift,
		Screens: to.Screens && !from.Screens}); len(unconfirmed) > 0 {
		return fmt.Errorf("the confirmation did not cover %s: %w", strings.Join(unconfirmed, ", "), ErrNotConfirmed)
	}
	return nil
}

// ConfirmAtTerminal asks the user to confirm caps for signIn, and project's loops when project is not nil, at the
// controlling terminal: it opens /dev/tty itself, so
// it fails where there is none (launchd, cron, setsid, a pipe), shows the caps, and accepts only the weekly amount
// typed back. Standard input and output are not used, so a script cannot answer through them. Cancelling ctx closes
// the terminal and returns ctx's error.
func (s Service) ConfirmAtTerminal(ctx context.Context, caps Caps, signIn SignIn, project *ProjectLoops) (*TerminalConfirmation, error) {
	open := s.openTTY
	if open == nil {
		open = openControllingTerminal
	}
	tty, err := open()
	if err != nil {
		return nil, fmt.Errorf("confirming the watch's budget needs a terminal (%w): %w", err, ErrNotConfirmed)
	}
	defer tty.Close()
	stop := context.AfterFunc(ctx, func() { tty.Close() }) // unblocks the read below
	defer stop()
	fmt.Fprintf(tty, "The watch may spend, for all projects together:\n"+
		"  $%.2f a week at list price, at most $%.2f a run\n"+
		"  %.0f%% of a five-hour window per pass, %.0f%% of the seven-day window a week (subscription only)\n"+
		"  and starts nothing above %.0f%% (five-hour) or %.0f%% (seven-day) used.\n"+
		"Sign-in: %s.\n",
		caps.WeeklyUSD, caps.RunCapUSD, 100*caps.PassShare, 100*caps.WeeklyShare, 100*caps.StartFiveHour, 100*caps.StartSevenDay, signIn.Mode)
	if project != nil {
		fmt.Fprintf(tty, "In project %s, the watch may: %s.\n", project.Name, describeLoops(project.Loops))
	}
	fmt.Fprint(tty, "Type the weekly amount in dollars to confirm: ")
	line, err := bufio.NewReader(tty).ReadString('\n')
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read the confirmation: %w", err)
	}
	typed, err := strconv.ParseFloat(strings.TrimPrefix(strings.TrimSpace(line), "$"), 64)
	if err != nil || math.IsNaN(typed) || math.Round(typed*100) != math.Round(caps.WeeklyUSD*100) {
		fmt.Fprintln(tty, "Not confirmed: nothing changed.")
		return nil, fmt.Errorf("typed %q, not the weekly amount $%.2f: %w", strings.TrimSpace(line), caps.WeeklyUSD, ErrNotConfirmed)
	}
	c := &TerminalConfirmation{caps: caps, signIn: signIn, valid: true}
	if project != nil {
		c.project, c.hasProject = *project, true
	}
	return c, nil
}

// describeLoops lists loops in words for the prompt.
func describeLoops(l Loops) string {
	var parts []string
	for _, p := range []struct {
		on   bool
		text string
	}{{l.Experiments, "continue enrolled experiments"}, {l.Drift, "run drift checks"}, {l.Screens, "run queued cost screens"}} {
		if p.on {
			parts = append(parts, p.text)
		}
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return strings.Join(parts, ", ")
}

// openControllingTerminal opens the process's controlling terminal, which fails without one.
func openControllingTerminal() (io.ReadWriteCloser, error) {
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}
