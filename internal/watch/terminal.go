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

// TerminalConfirmation proves that the user, at a terminal, confirmed these caps for this sign-in by typing back the
// weekly amount. Only ConfirmAtTerminal makes a valid one: its fields are unexported, and a zero value (which another
// package can write as a literal) is refused. Grant and SetLoops need one to raise anything. Only the CLI's
// `watch enable` may call ConfirmAtTerminal (TestOnlyTheEnableCommandConfirms).
type TerminalConfirmation struct {
	caps   Caps
	signIn SignIn
	valid  bool
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

// ConfirmAtTerminal asks the user to confirm caps for signIn at the controlling terminal: it opens /dev/tty itself, so
// it fails where there is none (launchd, cron, setsid, a pipe), shows the caps, and accepts only the weekly amount
// typed back. Standard input and output are not used, so a script cannot answer through them. Cancelling ctx closes
// the terminal and returns ctx's error.
func (s Service) ConfirmAtTerminal(ctx context.Context, caps Caps, signIn SignIn) (*TerminalConfirmation, error) {
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
		"Sign-in: %s.\n"+
		"Type the weekly amount in dollars to confirm: ",
		caps.WeeklyUSD, caps.RunCapUSD, 100*caps.PassShare, 100*caps.WeeklyShare, 100*caps.StartFiveHour, 100*caps.StartSevenDay, signIn.Mode)
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
	return &TerminalConfirmation{caps: caps, signIn: signIn, valid: true}, nil
}

// openControllingTerminal opens the process's controlling terminal, which fails without one.
func openControllingTerminal() (io.ReadWriteCloser, error) {
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}
