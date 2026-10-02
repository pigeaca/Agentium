package watch

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/store"
)

func grant(projectID int64, interactive bool) Grant {
	return Grant{ProjectID: projectID, Caps: DefaultCaps(), Loops: Loops{Experiments: true, Drift: true}, SignIn: claude.SignInLogin,
		GrantedBy: "ana", Version: "dev", Interactive: interactive}
}

// Budget and consent: no consent row means no spend, and a revoked consent is no consent.
func TestNoConsentMeansNoSpend(t *testing.T) {
	ctx := context.Background()
	s, c, app := newService(t, time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC))
	if _, err := s.Consent(ctx, app.ID, claude.SignInLogin); !errors.Is(err, ErrNoConsent) {
		t.Fatalf("a project without consent: %v, want ErrNoConsent", err)
	}
	if revoked, err := s.Revoke(ctx, app.ID, "ana", "dev"); err != nil || revoked {
		t.Errorf("revoking no consent = %v, %v", revoked, err)
	}
	granted, err := s.Grant(ctx, grant(app.ID, true))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Consent(ctx, app.ID, claude.SignInLogin)
	if err != nil || got.ID != granted.ID || got.Caps != DefaultCaps() || !got.Interactive || got.GrantedBy != "ana" || !got.GrantedAt.Equal(c.now) {
		t.Fatalf("the consent in force = %+v, %v", got, err)
	}
	c.now = c.now.Add(time.Hour)
	if revoked, err := s.Revoke(ctx, app.ID, "ana", "dev"); err != nil || !revoked {
		t.Fatalf("revoke = %v, %v", revoked, err)
	}
	if _, err := s.Consent(ctx, app.ID, claude.SignInLogin); !errors.Is(err, ErrNoConsent) {
		t.Errorf("after a revocation: %v, want ErrNoConsent", err)
	}
}

// Budget and consent: only the terminal raises. A grant that is not interactive cannot be the first, cannot raise any
// cap, threshold or loop (the error names it), and leaves the consent in force unchanged; it may lower. After a
// revocation it cannot restore the consent either.
func TestOnlyTheTerminalRaises(t *testing.T) {
	ctx := context.Background()
	s, _, app := newService(t, time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC))
	if _, err := s.Grant(ctx, grant(app.ID, false)); !errors.Is(err, ErrRaise) {
		t.Fatalf("a first grant that is not interactive: %v, want ErrRaise", err)
	}
	granted, err := s.Grant(ctx, grant(app.ID, true))
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
		"the screens loop":                           func(g *Grant) { g.Loops.Screens = true },
		"the sign-in mode (login to token-file)":     func(g *Grant) { g.SignIn = claude.SignInTokenFile },
	} {
		g := grant(app.ID, false)
		raise(&g)
		if _, err := s.Grant(ctx, g); !errors.Is(err, ErrRaise) || !strings.Contains(err.Error(), want) {
			t.Errorf("raising %s without the terminal: %v", want, err)
		}
	}
	if got, err := s.Consent(ctx, app.ID, claude.SignInLogin); err != nil || got.ID != granted.ID {
		t.Fatalf("the consent in force after refused raises = %+v, %v", got, err)
	}
	lower := grant(app.ID, false)
	lower.Caps.WeeklyUSD, lower.Caps.PassShare, lower.Loops.Drift = 10, 0.2, false
	if _, err := s.Grant(ctx, lower); err != nil {
		t.Fatalf("a lowering without the terminal: %v", err)
	}
	if got, _ := s.Consent(ctx, app.ID, claude.SignInLogin); got.Caps.WeeklyUSD != 10 || got.Loops.Drift || got.Interactive {
		t.Errorf("the lowered consent = %+v", got)
	}
	if _, err := s.Grant(ctx, grant(app.ID, false)); !errors.Is(err, ErrRaise) {
		t.Errorf("restoring the defaults without the terminal: %v, want ErrRaise", err)
	}
	if _, err := s.Revoke(ctx, app.ID, "ana", "dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grant(ctx, lower); !errors.Is(err, ErrRaise) {
		t.Errorf("a grant after a revocation without the terminal: %v, want ErrRaise", err)
	}
	if _, err := s.Grant(ctx, grant(app.ID, true)); err != nil {
		t.Errorf("an interactive grant after a revocation: %v", err)
	}
}

// The store refuses a raise that is not interactive even from a writer that skips this package's check.
func TestTheStoreRefusesARaiseOnItsOwn(t *testing.T) {
	ctx := context.Background()
	s, c, app := newService(t, time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC))
	if _, err := s.Grant(ctx, grant(app.ID, true)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.AddWatchConsent(ctx, store.WatchConsent{ProjectID: app.ID, Enabled: true, WeeklyUSD: 500, RunCapUSD: 3, PassShare: 0.3,
		WeeklyShare: 0.15, StartFiveHour: 0.5, StartSevenDay: 0.6, LoopExperiments: true, SignIn: claude.SignInLogin, GrantedBy: "agentium.toml",
		GrantedAt: c.now}); !errors.Is(err, ErrRaise) {
		t.Errorf("a direct raise: %v, want ErrRaise", err)
	}
}

// Budget and consent: a changed sign-in mode needs consent again. The consent stays recorded, but it does not hold
// for the new mode until an interactive grant for it.
func TestASignInChangeNeedsConsentAgain(t *testing.T) {
	ctx := context.Background()
	s, _, app := newService(t, time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC))
	if _, err := s.Grant(ctx, grant(app.ID, true)); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{claude.SignInAPIKey, claude.SignInTokenFile} {
		if _, err := s.Consent(ctx, app.ID, mode); !errors.Is(err, ErrSignInChanged) || !strings.Contains(err.Error(), "login") {
			t.Errorf("the consent under %s: %v, want ErrSignInChanged", mode, err)
		}
	}
	apiKey := grant(app.ID, false)
	apiKey.SignIn = claude.SignInAPIKey
	if _, err := s.Grant(ctx, apiKey); !errors.Is(err, ErrRaise) {
		t.Errorf("a new sign-in mode without the terminal: %v, want ErrRaise", err)
	}
	apiKey.Interactive = true
	if _, err := s.Grant(ctx, apiKey); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Consent(ctx, app.ID, claude.SignInAPIKey); err != nil {
		t.Errorf("the consent under the new mode: %v", err)
	}
	if _, err := s.Consent(ctx, app.ID, claude.SignInLogin); !errors.Is(err, ErrSignInChanged) {
		t.Errorf("the old mode after the new consent: %v, want ErrSignInChanged", err)
	}
}

// A grant is checked before it is stored: caps above zero, shares at most 100%, a run cap within the weekly budget, a
// known sign-in mode, a loop and a user.
func TestGrantValidation(t *testing.T) {
	ctx := context.Background()
	s, _, app := newService(t, time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC))
	for want, change := range map[string]func(g *Grant){
		"the weekly budget must be above 0":               func(g *Grant) { g.Caps.WeeklyUSD = 0 },
		"the weekly budget must be above 0 (got NaN":      func(g *Grant) { g.Caps.WeeklyUSD = math.NaN() },
		"the run cap must be above 0":                     func(g *Grant) { g.Caps.RunCapUSD = -1 },
		"the pass share must be above 0 and at most 100%": func(g *Grant) { g.Caps.PassShare = 30 },
		"the seven-day start threshold":                   func(g *Grant) { g.Caps.StartSevenDay = 1.5 },
		"is above the weekly budget":                      func(g *Grant) { g.Caps.RunCapUSD = 25 },
		`unknown sign-in mode "oauth"`:                    func(g *Grant) { g.SignIn = "oauth" },
		"no loop enabled":                                 func(g *Grant) { g.Loops = Loops{} },
		"no user named":                                   func(g *Grant) { g.GrantedBy = " " },
	} {
		g := grant(app.ID, true)
		change(&g)
		if _, err := s.Grant(ctx, g); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
	if _, err := s.Consent(ctx, app.ID, claude.SignInLogin); !errors.Is(err, ErrNoConsent) {
		t.Errorf("after invalid grants: %v, want no consent", err)
	}
}
