package watch

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
)

// fakeTTY is a terminal: what the user types comes from in, and what Agentium shows goes to out.
type fakeTTY struct {
	in     io.Reader
	out    bytes.Buffer
	closed bool
}

func (f *fakeTTY) Read(p []byte) (int, error)  { return f.in.Read(p) }
func (f *fakeTTY) Write(p []byte) (int, error) { return f.out.Write(p) }
func (f *fakeTTY) Close() error {
	f.closed = true
	if c, ok := f.in.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// withTTY gives s a terminal where the user types typed.
func withTTY(s Service, typed string) (Service, *fakeTTY) {
	tty := &fakeTTY{in: strings.NewReader(typed)}
	s.openTTY = func() (io.ReadWriteCloser, error) { return tty, nil }
	return s, tty
}

// ConfirmAtTerminal shows the caps for all projects together and confirms only the weekly amount typed back (with or
// without a dollar sign or cents); a wrong amount, nothing typed or no terminal confirm nothing; cancelling stops the
// wait for the user.
func TestConfirmAtTerminal(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newService(t, time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC))
	for _, typed := range []string{"20\n", "$20.00\n", "  20.0  \n", "20"} {
		withTerminal, tty := withTTY(s, typed)
		proof, err := withTerminal.ConfirmAtTerminal(ctx, DefaultCaps(), login)
		if err != nil || proof == nil || !proof.valid || proof.caps != DefaultCaps() || proof.signIn != login {
			t.Errorf("typing %q = %+v, %v", typed, proof, err)
		}
		shown := tty.out.String()
		for _, want := range []string{"for all projects together", "$20.00 a week", "at most $3.00 a run", "30% of a five-hour window per pass",
			"15% of the seven-day window a week", "above 50% (five-hour) or 60% (seven-day)", "Sign-in: login.", "Type the weekly amount"} {
			if !strings.Contains(shown, want) {
				t.Errorf("the prompt lacks %q:\n%s", want, shown)
			}
		}
		if !tty.closed {
			t.Error("the terminal was left open")
		}
	}
	for _, typed := range []string{"200\n", "19.99\n", "yes\n", "\n", "", "NaN\n"} {
		withTerminal, tty := withTTY(s, typed)
		if proof, err := withTerminal.ConfirmAtTerminal(ctx, DefaultCaps(), login); !errors.Is(err, ErrNotConfirmed) || proof != nil {
			t.Errorf("typing %q = %+v, %v; want ErrNotConfirmed", typed, proof, err)
		}
		if !strings.Contains(tty.out.String(), "Not confirmed: nothing changed.") {
			t.Errorf("typing %q: the user was not told:\n%s", typed, tty.out.String())
		}
	}
	noTerminal := s
	noTerminal.openTTY = func() (io.ReadWriteCloser, error) {
		return nil, &fs.PathError{Op: "open", Path: "/dev/tty", Err: syscall.ENXIO}
	}
	if _, err := noTerminal.ConfirmAtTerminal(ctx, DefaultCaps(), login); !errors.Is(err, ErrNotConfirmed) || !strings.Contains(err.Error(), "needs a terminal") {
		t.Errorf("without a terminal: %v", err)
	}
	waiting, _ := io.Pipe() // the user never types
	blocked := s
	blocked.openTTY = func() (io.ReadWriteCloser, error) { return &fakeTTY{in: waiting}, nil }
	cancelled, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := blocked.ConfirmAtTerminal(cancelled, DefaultCaps(), login); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a cancelled wait: %v, want the context's error", err)
	}
}

// Without a controlling terminal (as under launchd, cron or setsid), the real /dev/tty cannot be opened, so nothing is
// confirmed. It runs a child in a new session, which has no controlling terminal.
func TestNoControllingTerminalConfirmsNothing(t *testing.T) {
	if os.Getenv("AGENTIUM_WATCH_TTY_CHILD") == "1" {
		s := Service{Now: time.Now}
		if _, err := s.ConfirmAtTerminal(context.Background(), DefaultCaps(), login); errors.Is(err, ErrNotConfirmed) {
			os.Exit(0)
		}
		os.Exit(3)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	attr := &os.ProcAttr{Env: append(os.Environ(), "AGENTIUM_WATCH_TTY_CHILD=1"), Files: []*os.File{nil, os.Stdout, os.Stderr},
		Sys: &syscall.SysProcAttr{Setsid: true}}
	p, err := os.StartProcess(exe, []string{exe, "-test.run=^TestNoControllingTerminalConfirmsNothing$"}, attr)
	if err != nil {
		t.Fatal(err)
	}
	state, err := p.Wait()
	if err != nil || state.ExitCode() != 0 {
		t.Errorf("a session without a terminal confirmed, or failed otherwise: %v, %v", state, err)
	}
}

// Only the future `agentium watch enable` command may confirm at a terminal: outside tests, no file but
// internal/cli/watch_enable*.go may call ConfirmAtTerminal, and none but this package's own may write a
// TerminalConfirmation literal (a zero one would be refused, but it should not be written at all).
func TestOnlyTheEnableCommandConfirms(t *testing.T) {
	root := moduleRoot(t)
	found, err := confirmers(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		t.Errorf("confirmations outside `watch enable`: %v", found)
	}
	// The scan itself finds both kinds, so a clean module is not a broken scanner.
	fake := t.TempDir()
	for name, body := range map[string]string{
		"internal/pool/sneak.go":            "package pool\nfunc f(s watch.Service) { s.ConfirmAtTerminal(nil, watch.Caps{}, watch.SignIn{}) }\n",
		"internal/cli/watch_status.go":      "package cli\nvar c = &watch.TerminalConfirmation{}\n",
		"internal/cli/watch_enable.go":      "package cli\nfunc g(s watch.Service) { s.ConfirmAtTerminal(nil, watch.Caps{}, watch.SignIn{}) }\n",
		"internal/cli/watch_enable_test.go": "package cli\nfunc h(s watch.Service) { s.ConfirmAtTerminal(nil, watch.Caps{}, watch.SignIn{}) }\n",
		"internal/watch/terminal.go":        "package watch\nfunc k() *TerminalConfirmation { return &TerminalConfirmation{valid: true} }\n",
		"internal/watch/elsewhere.go":       "package watch\nfunc m(s Service) { s.ConfirmAtTerminal(nil, Caps{}, SignIn{}) }\n",
	} {
		file := filepath.Join(fake, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	found, err = confirmers(fake)
	want := []string{"internal/cli/watch_status.go: TerminalConfirmation literal", "internal/pool/sneak.go: ConfirmAtTerminal",
		"internal/watch/elsewhere.go: ConfirmAtTerminal"}
	if err != nil || !slices.Equal(found, want) {
		t.Errorf("the scan of a planted module = %v, %v; want %v", found, err, want)
	}
}

// confirmers lists the Go files under root (tests excepted) that call ConfirmAtTerminal outside the enable command,
// or write a TerminalConfirmation literal outside terminal.go, sorted.
func confirmers(root string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		enable, _ := filepath.Match("internal/cli/watch_enable*.go", rel)
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if n.Sel.Name == "ConfirmAtTerminal" && !enable {
					found = append(found, rel+": ConfirmAtTerminal")
				}
			case *ast.CompositeLit:
				if named(n.Type, "TerminalConfirmation") && rel != "internal/watch/terminal.go" {
					found = append(found, rel+": TerminalConfirmation literal")
				}
			}
			return true
		})
		return nil
	})
	slices.Sort(found)
	return slices.Compact(found), err
}

func named(e ast.Expr, name string) bool {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name == name
	case *ast.SelectorExpr:
		return e.Sel.Name == name
	}
	return false
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's directory")
		}
		dir = parent
	}
}

func tokenFile(t *testing.T, content string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "claude-token")
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// replaceFile replaces file by a new one with content (a rename, as an editor or a new `claude setup-token` would) and
// returns its sign-in.
func replaceFile(t *testing.T, file, content string) SignIn {
	t.Helper()
	if err := os.WriteFile(file+".new", []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(file+".new", file); err != nil {
		t.Fatal(err)
	}
	s, err := SignInOf(claude.SignInTokenFile, file)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A token file's identity is its path and inode, never its content: rewriting it in place keeps it (a documented
// limit), another file or a replaced file changes it, and a missing file is an error. Logins and API keys have none.
func TestSignInIdentity(t *testing.T) {
	file := tokenFile(t, "token")
	first, err := SignInOf(claude.SignInTokenFile, file)
	if err != nil || first.Mode != claude.SignInTokenFile || len(first.Identity) != 64 || strings.Contains(first.Identity, "token") {
		t.Fatal(first, err)
	}
	if err := os.WriteFile(file, []byte("another token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if same, _ := SignInOf(claude.SignInTokenFile, file); same != first {
		t.Errorf("rewriting in place changed the identity: %v, %v", first, same)
	}
	if other, _ := SignInOf(claude.SignInTokenFile, tokenFile(t, "token")); other.Identity == first.Identity {
		t.Error("another file with the same content has the same identity")
	}
	if replaced := replaceFile(t, file, "token"); replaced.Identity == first.Identity {
		t.Error("a replaced file kept its identity")
	}
	if _, err := SignInOf(claude.SignInTokenFile, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing token file has an identity")
	}
	for _, mode := range []string{claude.SignInLogin, claude.SignInAPIKey} {
		if s, err := SignInOf(mode, file); err != nil || s != (SignIn{Mode: mode}) {
			t.Errorf("%s = %+v, %v; want no identity", mode, s, err)
		}
	}
}
