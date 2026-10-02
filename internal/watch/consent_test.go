package watch

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/store"
)

var login = SignIn{Mode: claude.SignInLogin}

func grant(caps Caps, signIn SignIn) Grant {
	return Grant{Caps: caps, SignIn: signIn, GrantedBy: "ana", Version: "dev"}
}

// confirmed is a confirmation made through ConfirmAtTerminal, with the weekly amount typed back.
func confirmed(t *testing.T, s Service, caps Caps, signIn SignIn) *TerminalConfirmation {
	t.Helper()
	s, _ = withTTY(s, fmt.Sprintf("%.2f\n", caps.WeeklyUSD))
	c, err := s.ConfirmAtTerminal(context.Background(), caps, signIn)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Budget and consent: no consent row means no spend, and a revoked consent is no consent.
func TestNoConsentMeansNoSpend(t *testing.T) {
	ctx := context.Background()
	s, c, _ := newService(t, time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC))
	if _, err := s.Consent(ctx, login); !errors.Is(err, ErrNoConsent) {
		t.Fatalf("no consent: %v, want ErrNoConsent", err)
	}
	if revoked, err := s.Revoke(ctx, "ana", "dev"); err != nil || revoked {
		t.Errorf("revoking no consent = %v, %v", revoked, err)
	}
	granted, err := s.Grant(ctx, grant(DefaultCaps(), login), confirmed(t, s, DefaultCaps(), login))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Consent(ctx, login)
	if err != nil || got.ID != granted.ID || got.Caps != DefaultCaps() || !got.Confirmed || got.GrantedBy != "ana" || !got.GrantedAt.Equal(c.now) {
		t.Fatalf("the consent in force = %+v, %v", got, err)
	}
	c.now = c.now.Add(time.Hour)
	if revoked, err := s.Revoke(ctx, "ana", "dev"); err != nil || !revoked {
		t.Fatalf("revoke = %v, %v", revoked, err)
	}
	if _, err := s.Consent(ctx, login); !errors.Is(err, ErrNoConsent) {
		t.Errorf("after a revocation: %v, want ErrNoConsent", err)
	}
}

// Budget and consent: only the terminal raises. Without a confirmation a grant cannot be the first, cannot raise any
// cap or threshold (the error names it), and leaves the consent in force unchanged; it may lower. After a revocation
// it cannot restore the consent either.
func TestOnlyTheTerminalRaises(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newService(t, time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC))
	if _, err := s.Grant(ctx, grant(DefaultCaps(), login), nil); !errors.Is(err, ErrRaise) {
		t.Fatalf("an unconfirmed first grant: %v, want ErrRaise", err)
	}
	granted, err := s.Grant(ctx, grant(DefaultCaps(), login), confirmed(t, s, DefaultCaps(), login))
	if err != nil {
		t.Fatal(err)
	}
	for want, raise := range map[string]func(g *Grant){
		"the weekly budget ($20.00 to $25.00)":       func(g *Grant) { g.Caps.WeeklyUSD = 25 },
		"the run cap ($3.00 to $5.00)":               func(g *Grant) { g.Caps.RunCapUSD = 5 },
		"the pass share (30% to 40%)":                func(g *Grant) { g.Caps.PassShare = 0.4 },
		"the weekly share (15% to 20%)":              func(g *Grant) { g.Caps.WeeklyShare = 0.2 },
		"the five-hour start threshold (50% to 80%)": func(g *Grant) { g.Caps.StartFiveHour = 0.8 },
		"the seven-day start threshold (60% to 90%)": func(g *Grant) { g.Caps.StartSevenDay = 0.9 },
		"the sign-in (from login to token-file)":     func(g *Grant) { g.SignIn = SignIn{Mode: claude.SignInTokenFile, Identity: "x"} },
	} {
		g := grant(DefaultCaps(), login)
		raise(&g)
		if _, err := s.Grant(ctx, g, nil); !errors.Is(err, ErrRaise) || !strings.Contains(err.Error(), want) {
			t.Errorf("raising %s without the terminal: %v", want, err)
		}
	}
	if got, err := s.Consent(ctx, login); err != nil || got.ID != granted.ID {
		t.Fatalf("the consent in force after refused raises = %+v, %v", got, err)
	}
	lower := grant(DefaultCaps(), login)
	lower.Caps.WeeklyUSD, lower.Caps.PassShare = 10, 0.2
	if _, err := s.Grant(ctx, lower, nil); err != nil {
		t.Fatalf("a lowering without the terminal: %v", err)
	}
	if got, _ := s.Consent(ctx, login); got.Caps.WeeklyUSD != 10 || got.Confirmed {
		t.Errorf("the lowered consent = %+v", got)
	}
	if _, err := s.Grant(ctx, grant(DefaultCaps(), login), nil); !errors.Is(err, ErrRaise) {
		t.Errorf("restoring the defaults without the terminal: %v, want ErrRaise", err)
	}
	if _, err := s.Revoke(ctx, "ana", "dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grant(ctx, lower, nil); !errors.Is(err, ErrRaise) {
		t.Errorf("an unconfirmed grant after a revocation: %v, want ErrRaise", err)
	}
	if _, err := s.Grant(ctx, grant(DefaultCaps(), login), confirmed(t, s, DefaultCaps(), login)); err != nil {
		t.Errorf("a confirmed grant after a revocation: %v", err)
	}
}

// A confirmation proves only what was typed at the terminal: a zero value (which any package can write) proves
// nothing, and a confirmation of some caps or sign-in does not cover others.
func TestAConfirmationCoversOnlyWhatWasConfirmed(t *testing.T) {
	ctx := context.Background()
	s, _, app := newService(t, time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC))
	if _, err := s.Grant(ctx, grant(DefaultCaps(), login), &TerminalConfirmation{}); !errors.Is(err, ErrNotConfirmed) {
		t.Errorf("a zero confirmation: %v, want ErrNotConfirmed", err)
	}
	if err := s.SetLoops(ctx, app.ID, Loops{Drift: true}, "ana", &TerminalConfirmation{}); !errors.Is(err, ErrNotConfirmed) {
		t.Errorf("loops with a zero confirmation: %v, want ErrNotConfirmed", err)
	}
	proof := confirmed(t, s, DefaultCaps(), login)
	more := DefaultCaps()
	more.WeeklyUSD = 200
	if _, err := s.Grant(ctx, grant(more, login), proof); !errors.Is(err, ErrNotConfirmed) {
		t.Errorf("a confirmation of $20 used for $200: %v, want ErrNotConfirmed", err)
	}
	if _, err := s.Grant(ctx, grant(DefaultCaps(), SignIn{Mode: claude.SignInAPIKey}), proof); !errors.Is(err, ErrNotConfirmed) {
		t.Errorf("a confirmation for the login used for an API key: %v, want ErrNotConfirmed", err)
	}
	if _, err := s.Consent(ctx, login); !errors.Is(err, ErrNoConsent) {
		t.Errorf("after refused grants: %v, want no consent", err)
	}
}

// The store refuses an unconfirmed raise even from a writer that skips this package's check.
func TestTheStoreRefusesARaiseOnItsOwn(t *testing.T) {
	ctx := context.Background()
	s, c, _ := newService(t, time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC))
	if _, err := s.Grant(ctx, grant(DefaultCaps(), login), confirmed(t, s, DefaultCaps(), login)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.AddWatchConsent(ctx, store.WatchConsent{Enabled: true, WeeklyUSD: 500, RunCapUSD: 3, PassShare: 0.3, WeeklyShare: 0.15,
		StartFiveHour: 0.5, StartSevenDay: 0.6, SignIn: claude.SignInLogin, GrantedBy: "agentium.toml", GrantedAt: c.now}); !errors.Is(err, ErrRaise) {
		t.Errorf("a direct raise: %v, want ErrRaise", err)
	}
}

// Budget and consent: a changed sign-in needs consent again, whether the mode changed or the token file did. The
// consent stays recorded, but it does not hold for the new sign-in until a confirmed grant for it.
func TestASignInChangeNeedsConsentAgain(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newService(t, time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC))
	if _, err := s.Grant(ctx, grant(DefaultCaps(), login), confirmed(t, s, DefaultCaps(), login)); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{claude.SignInAPIKey, claude.SignInTokenFile} {
		if _, err := s.Consent(ctx, SignIn{Mode: mode}); !errors.Is(err, ErrSignInChanged) || !strings.Contains(err.Error(), "from login to "+mode) {
			t.Errorf("the consent under %s: %v, want ErrSignInChanged", mode, err)
		}
	}
	token := tokenFile(t, "first token")
	first, err := SignInOf(claude.SignInTokenFile, token)
	if err != nil || first.Identity == "" {
		t.Fatal(first, err)
	}
	if _, err := s.Grant(ctx, grant(DefaultCaps(), first), nil); !errors.Is(err, ErrRaise) {
		t.Errorf("a new sign-in without the terminal: %v, want ErrRaise", err)
	}
	if _, err := s.Grant(ctx, grant(DefaultCaps(), first), confirmed(t, s, DefaultCaps(), first)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Consent(ctx, first); err != nil {
		t.Errorf("the consent under the token file: %v", err)
	}
	if _, err := s.Consent(ctx, login); !errors.Is(err, ErrSignInChanged) {
		t.Errorf("the login after the token file's consent: %v, want ErrSignInChanged", err)
	}
	replaced := replaceFile(t, token, "second token")
	if replaced.Identity == first.Identity {
		t.Fatal("a replaced token file kept its identity")
	}
	if _, err := s.Consent(ctx, replaced); !errors.Is(err, ErrSignInChanged) || !strings.Contains(err.Error(), "to another token-file") {
		t.Errorf("the consent after the token file was replaced: %v, want ErrSignInChanged", err)
	}
}

// A grant is checked before it is stored: caps above zero, shares at most 100%, a run cap within the weekly budget, a
// known sign-in mode and a user.
func TestGrantValidation(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newService(t, time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC))
	for want, change := range map[string]func(g *Grant){
		"the weekly budget must be above 0":               func(g *Grant) { g.Caps.WeeklyUSD = 0 },
		"the weekly budget must be above 0 (got NaN":      func(g *Grant) { g.Caps.WeeklyUSD = math.NaN() },
		"the run cap must be above 0":                     func(g *Grant) { g.Caps.RunCapUSD = -1 },
		"the pass share must be above 0 and at most 100%": func(g *Grant) { g.Caps.PassShare = 30 },
		"the seven-day start threshold":                   func(g *Grant) { g.Caps.StartSevenDay = 1.5 },
		"is above the weekly budget":                      func(g *Grant) { g.Caps.RunCapUSD = 25 },
		`unknown sign-in mode "oauth"`:                    func(g *Grant) { g.SignIn.Mode = "oauth" },
		"no user named":                                   func(g *Grant) { g.GrantedBy = " " },
	} {
		g := grant(DefaultCaps(), login)
		change(&g)
		if _, err := s.Grant(ctx, g, &TerminalConfirmation{caps: g.Caps, signIn: g.SignIn, valid: true}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
	if _, err := s.Consent(ctx, login); !errors.Is(err, ErrNoConsent) {
		t.Errorf("after invalid grants: %v, want no consent", err)
	}
}

// Budget and consent: a project's loops start off; turning one on needs the terminal (the error names it), turning
// them off does not, and each project has its own.
func TestLoopsOnlyTheTerminalTurnsOn(t *testing.T) {
	ctx := context.Background()
	s, c, app := newService(t, time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC))
	other, err := s.DB.SaveProject(ctx, "/work/other", "other", []byte(`{}`), c.now)
	if err != nil {
		t.Fatal(err)
	}
	if loops, err := s.Loops(ctx, app.ID); err != nil || loops != (Loops{}) {
		t.Fatalf("loops before any = %+v, %v", loops, err)
	}
	for want, loops := range map[string]Loops{
		"the experiments loop": {Experiments: true}, "the drift loop": {Drift: true}, "the screens loop": {Screens: true},
	} {
		if err := s.SetLoops(ctx, app.ID, loops, "ana", nil); !errors.Is(err, ErrRaise) || !strings.Contains(err.Error(), want) {
			t.Errorf("turning on %s without the terminal: %v", want, err)
		}
	}
	all := Loops{Experiments: true, Drift: true, Screens: true}
	if err := s.SetLoops(ctx, app.ID, all, "ana", confirmed(t, s, DefaultCaps(), login)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLoops(ctx, app.ID, Loops{Drift: true}, "ana", nil); err != nil {
		t.Fatalf("turning loops off without the terminal: %v", err)
	}
	if err := s.SetLoops(ctx, app.ID, Loops{Drift: true, Screens: true}, "ana", nil); !errors.Is(err, ErrRaise) {
		t.Errorf("turning one back on without the terminal: %v, want ErrRaise", err)
	}
	if loops, _ := s.Loops(ctx, app.ID); loops != (Loops{Drift: true}) {
		t.Errorf("the project's loops = %+v", loops)
	}
	if loops, _ := s.Loops(ctx, other.ID); loops != (Loops{}) {
		t.Errorf("another project's loops = %+v; want none", loops)
	}
}
