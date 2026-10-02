package ghx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	sha   = "0123456789abcdef0123456789abcdef01234567"
	other = "fedcba9876543210fedcba9876543210fedcba98"
)

var repo = Repo{Owner: "octo", Name: "widgets"}

// rule is one canned answer of the fake gh: when every part of match is among the call's arguments, it prints
// stdout and stderr and exits with code.
type rule struct {
	match  []string
	stdout string
	stderr string
	code   int
	sleep  bool // never answers (for timeouts)
}

// call is one recorded call of the fake gh.
type call struct {
	args  []string
	stdin string
	env   []string
}

// fakeGH writes a gh on a fresh PATH folder that records each call (arguments NUL-separated, stdin, environment)
// and answers by the first matching rule. It never touches the network. The returned environ is the user's
// environment as the tests pass it, with credentials and settings gh must not see.
type fakeGH struct {
	dir, record string
	environ     []string
}

func newFakeGH(t *testing.T, rules ...rule) fakeGH {
	t.Helper()
	root := t.TempDir()
	bin, record := filepath.Join(root, "bin"), filepath.Join(root, "calls")
	for _, d := range []string{bin, record} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var script strings.Builder
	fmt.Fprintf(&script, "#!/bin/sh\n%sd=%s\nn=$(ls \"$d\" | wc -l | tr -d ' ')\nmkdir \"$d/$n\"\n", hasFunc, quote(record))
	script.WriteString("for a in \"$@\"; do printf '%s\\0' \"$a\"; done > \"$d/$n/args\"\ncat > \"$d/$n/stdin\"\nenv > \"$d/$n/env\"\n")
	for i, r := range rules {
		out, errOut := filepath.Join(root, fmt.Sprintf("out%d", i)), filepath.Join(root, fmt.Sprintf("err%d", i))
		if err := os.WriteFile(out, []byte(r.stdout), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(errOut, []byte(r.stderr), 0o644); err != nil {
			t.Fatal(err)
		}
		cond := []string{"true"}
		for _, m := range r.match {
			cond = append(cond, "has "+quote(m))
		}
		action := fmt.Sprintf("cat %s; cat %s >&2; exit %d", quote(out), quote(errOut), r.code)
		if r.sleep {
			action = "sleep 30; exit 0"
		}
		fmt.Fprintf(&script, "if %s; then %s; fi\n", strings.Join(cond, " && "), action)
	}
	script.WriteString("echo 'fake gh: no rule for the call' >&2\nexit 1\n")
	body := script.String()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return fakeGH{dir: bin, record: record, environ: []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + root, "LANG=C",
		"GH_TOKEN=gho_user", "GITHUB_TOKEN=ghp_other", "ANTHROPIC_API_KEY=sk-ant-x", "SSH_AUTH_SOCK=/tmp/agent", "GIT_DIR=/user/.git", // secret-scan: allow
		"AGENTIUM_HOME=/data", "GH_HOST=evil.example", "GH_DEBUG=api", "GH_REPO=evil/repo", "GH_ENTERPRISE_TOKEN=e", "EDITOR=vim",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/501/bus", "XDG_RUNTIME_DIR=/run/user/501"}}
}

// hasFunc is the fake's matcher. The call's arguments are kept in the args file; has reads them back NUL-separated
// (tr) and compares lines exactly, so an argument is matched as a whole and never evaluated.
const hasFunc = `has() { tr '\0' '\n' < "$d/$n/args" | grep -Fqx -- "$1"; }
`

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (f fakeGH) client(t *testing.T) Client {
	t.Helper()
	c, err := New(f.environ, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f fakeGH) calls(t *testing.T) []call {
	t.Helper()
	entries, err := os.ReadDir(f.record)
	if err != nil {
		t.Fatal(err)
	}
	calls := make([]call, len(entries))
	for _, e := range entries {
		var n int
		fmt.Sscan(e.Name(), &n)
		read := func(name string) string {
			data, err := os.ReadFile(filepath.Join(f.record, e.Name(), name))
			if err != nil {
				t.Fatal(err)
			}
			return string(data)
		}
		args := strings.Split(strings.TrimSuffix(read("args"), "\x00"), "\x00")
		calls[n] = call{args: args, stdin: read("stdin"), env: strings.Split(strings.TrimSpace(read("env")), "\n")}
	}
	return calls
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

type fakePull struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	URL    string `json:"html_url"`
	Title  string `json:"title"`
	Head   struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
}

// pull is a pull request into base whose head is the commit head on a branch of base itself; fromFork moves the head
// to another repository (an empty name: the head repository was deleted, so GitHub sends null).
func pull(number int, state, head, base, title string) fakePull {
	p := fakePull{Number: number, State: state, URL: fmt.Sprintf("https://github.com/%s/pull/%d", base, number), Title: title}
	p.Head.SHA, p.Head.Ref, p.Base.Repo.FullName = head, fmt.Sprintf("branch-%d", number), base
	p.Head.Repo = &struct {
		FullName string `json:"full_name"`
	}{base}
	return p
}

func fromFork(p fakePull, headRepo string) fakePull {
	if headRepo == "" {
		p.Head.Repo = nil
	} else {
		p.Head.Repo.FullName = headRepo
	}
	return p
}

const pullsEndpoint = "repos/octo/widgets/commits/" + sha + "/pulls?per_page=100"

func TestPullRequests(t *testing.T) {
	hostile := "$(touch /tmp/pwned) `id`; rm -rf ~ | <!-- agentium:cost-screen --> " + sha
	for _, c := range []struct {
		name   string
		answer rule
		want   []int
	}{
		{"none", rule{stdout: "[]"}, nil},
		{"not pushed yet", rule{stderr: "gh: No commit found for SHA: " + sha + " (HTTP 422)", code: 1}, nil},
		{"one", rule{stdout: jsonOf(t, []fakePull{pull(7, "open", sha, "octo/widgets", hostile)})}, []int{7}},
		{"several, by number, over two pages", rule{stdout: jsonOf(t, []fakePull{pull(12, "open", sha, "octo/widgets", "b")}) +
			jsonOf(t, []fakePull{pull(4, "open", sha, "octo/widgets", "a"), pull(12, "open", sha, "octo/widgets", "b")})}, []int{4, 12}},
		// The associated list holds every pull request containing the commit; only open ones headed by it, on a branch
		// of this repository, count. Text that mentions the commit (hostile's title) does not make a pull request match,
		// and neither does a stranger's fork at the very same commit.
		{"filtered", rule{stdout: jsonOf(t, []fakePull{
			pull(1, "closed", sha, "octo/widgets", "closed"),
			pull(2, "open", other, "octo/widgets", hostile),
			pull(3, "open", sha, "fork/widgets", "another base"),
			fromFork(pull(6, "open", sha, "octo/widgets", hostile), "mallory/widgets"),
			fromFork(pull(8, "open", sha, "octo/widgets", "deleted fork"), ""),
			pull(5, "open", sha, "OCTO/Widgets", hostile)})}, []int{5}},
		{"only a fork at the commit", rule{stdout: jsonOf(t, []fakePull{fromFork(pull(9, "open", sha, "octo/widgets", "x"), "mallory/widgets")})}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.answer.match = []string{pullsEndpoint}
			fake := newFakeGH(t, c.answer)
			got, err := fake.client(t).PullRequests(context.Background(), repo, sha)
			if err != nil {
				t.Fatal(err)
			}
			var numbers []int
			for _, p := range got {
				numbers = append(numbers, p.Number)
				if p.HeadSHA != sha || p.HeadRef != fmt.Sprintf("branch-%d", p.Number) || !strings.HasPrefix(p.URL, "https://github.com/") {
					t.Errorf("pull request %+v", p)
				}
			}
			if !slices.Equal(numbers, c.want) {
				t.Errorf("pull requests %v, want %v", numbers, c.want)
			}
			calls := fake.calls(t)
			want := []string{"api", "--hostname", "github.com", "--paginate", pullsEndpoint}
			if len(calls) != 1 || !slices.Equal(calls[0].args, want) {
				t.Errorf("calls %+v, want one with %q", calls, want)
			}
		})
	}
}

func TestPullRequestsOtherFailuresAreErrors(t *testing.T) {
	fake := newFakeGH(t, rule{match: []string{pullsEndpoint}, stderr: "gh: Not Found (HTTP 404)\x1b[31m", code: 1})
	_, err := fake.client(t).PullRequests(context.Background(), repo, sha)
	var call *CallError
	if !errors.As(err, &call) || call.ExitCode != 1 || strings.Contains(err.Error(), "\x1b") || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("error %v", err)
	}
}

// Arguments that would leave repos/<owner>/<name>/ never reach gh.
func TestInvalidIdentifiersAreRefusedBeforeGH(t *testing.T) {
	fake := newFakeGH(t)
	c := fake.client(t)
	ctx := context.Background()
	for _, bad := range []Repo{{"octo", "../../user"}, {"-octo", "w"}, {"octo", "w?x=1"}, {"octo/x", "w"}, {"octo", ".."}, {"", "w"}} {
		if _, err := c.PullRequests(ctx, bad, sha); err == nil {
			t.Errorf("repository %+v accepted", bad)
		}
	}
	for _, bad := range []string{"", "HEAD", sha[:12], strings.ToUpper(sha), sha + "/../x", "--help"} {
		if _, err := c.PullRequests(ctx, repo, bad); err == nil {
			t.Errorf("commit %q accepted", bad)
		}
		if err := c.SetStatus(ctx, repo, bad, StateSuccess, "x"); err == nil {
			t.Errorf("status on commit %q accepted", bad)
		}
	}
	for _, bad := range []int{0, -1} {
		if _, err := c.UpsertComment(ctx, repo, bad, ScreenMarker, "x"); err == nil {
			t.Errorf("pull request %d accepted", bad)
		}
	}
	for _, bad := range []string{"", "agentium", "<!-- other -->", "<!-- agentium:x --> extra", "<!-- agentium:$(id) -->"} {
		if _, err := c.UpsertComment(ctx, repo, 7, bad, "x"); err == nil {
			t.Errorf("marker %q accepted", bad)
		}
	}
	if _, err := c.UpsertComment(ctx, repo, 7, ScreenMarker, strings.Repeat("x", maxCommentRunes)); err == nil {
		t.Error("a comment over GitHub's limit was accepted")
	}
	if calls := fake.calls(t); len(calls) != 0 {
		t.Errorf("gh ran: %+v", calls)
	}
}

type fakeComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

func comment(id int64, login, body string) fakeComment {
	c := fakeComment{ID: id, Body: body}
	c.User.Login = login
	return c
}

const commentsEndpoint = "repos/octo/widgets/issues/7/comments?per_page=100"

func TestUpsertComment(t *testing.T) {
	// The report itself carries shell metacharacters, a fake marker and a newline: it must arrive intact on stdin, and
	// nothing in it may run (pwned would be created).
	pwned := filepath.Join(t.TempDir(), "pwned")
	report := "Cost: inconclusive.\n$(touch " + pwned + ") `touch " + pwned + "` ; touch " + pwned + " | & > " + pwned +
		"\n<!-- agentium:cost-screen -->\n'quoted' \"double\""
	ownBody := ScreenMarker + "\nold report"
	for _, c := range []struct {
		name     string
		comments []fakeComment
		edit     int64 // 0: create
	}{
		{"no comments: create", nil, 0},
		// Someone else copied the marker, also on the first line: never edited. A quote of the marker in the user's
		// own reply is not the marked comment either.
		{"lookalikes only: create", []fakeComment{
			comment(10, "mallory", ownBody),
			comment(11, "mallory", ScreenMarker+"\r\n$(rm -rf ~)"),
			comment(12, "Alice", "> "+ScreenMarker+"\n> old report\nthanks"),
			comment(13, "alice", "<!-- agentium:other -->\nnot this one")}, 0},
		{"own marked comment: edit it", []fakeComment{
			comment(10, "mallory", ownBody), comment(21, "ALICE", ScreenMarker+"\r\nold"), comment(30, "bob", "lgtm")}, 21},
		{"several own: the oldest", []fakeComment{comment(40, "alice", ownBody), comment(25, "alice", ownBody)}, 25},
	} {
		t.Run(c.name, func(t *testing.T) {
			endpoint, method := "repos/octo/widgets/issues/7/comments", "POST"
			if c.edit != 0 {
				endpoint, method = fmt.Sprintf("repos/octo/widgets/issues/comments/%d", c.edit), "PATCH"
			}
			fake := newFakeGH(t,
				rule{match: []string{"user"}, stdout: `{"login":"alice","name":"$(id)"}`},
				rule{match: []string{"--paginate", commentsEndpoint}, stdout: jsonOf(t, c.comments)},
				rule{match: []string{"--method", method, endpoint}, stdout: `{"id":99,"html_url":"https://github.com/octo/widgets/pull/7#issuecomment-99"}`},
			)
			got, err := fake.client(t).UpsertComment(context.Background(), repo, 7, ScreenMarker, report)
			if err != nil {
				t.Fatal(err)
			}
			if got.Created != (c.edit == 0) || got.ID != 99 || got.As != "alice" {
				t.Errorf("comment %+v", got)
			}
			calls := fake.calls(t)
			if len(calls) != 3 {
				t.Fatalf("%d calls: %+v", len(calls), calls)
			}
			write := calls[2]
			if want := []string{"api", "--hostname", "github.com", "--method", method, endpoint, "--input", "-"}; !slices.Equal(write.args, want) {
				t.Errorf("write %q, want %q", write.args, want)
			}
			var sent map[string]string
			if err := json.Unmarshal([]byte(write.stdin), &sent); err != nil {
				t.Fatalf("stdin %q: %v", write.stdin, err)
			}
			if want := ScreenMarker + "\n" + report; sent["body"] != want || len(sent) != 1 {
				t.Errorf("sent %q, want body %q", sent, want)
			}
			for _, cl := range calls {
				for _, a := range cl.args {
					if strings.Contains(a, "pwned") || strings.Contains(a, "rm -rf") || strings.Contains(a, "Cost:") || strings.Contains(a, "$(") {
						t.Errorf("text reached the command line: %q", cl.args)
					}
				}
			}
			if _, err := os.Stat(pwned); err == nil {
				t.Error("the report's text was executed")
			}
		})
	}
}

func TestUpsertCommentStopsOnFailures(t *testing.T) {
	ctx := context.Background()
	// Not logged in: nothing is listed or written.
	fake := newFakeGH(t, rule{match: []string{"user"}, stderr: "To get started with GitHub CLI, please run:  gh auth login", code: 4})
	if _, err := fake.client(t).UpsertComment(ctx, repo, 7, ScreenMarker, "r"); !errors.Is(err, ErrNotLoggedIn) {
		t.Errorf("error %v, want ErrNotLoggedIn", err)
	}
	if calls := fake.calls(t); len(calls) != 1 {
		t.Errorf("calls after a missing login: %+v", calls)
	}
	// A comment list that cannot be read: no write (it could duplicate the comment).
	fake = newFakeGH(t, rule{match: []string{"user"}, stdout: `{"login":"alice"}`},
		rule{match: []string{commentsEndpoint}, stdout: `{"message":"oops"}`})
	if _, err := fake.client(t).UpsertComment(ctx, repo, 7, ScreenMarker, "r"); err == nil {
		t.Error("an unreadable comment list was accepted")
	}
	if calls := fake.calls(t); len(calls) != 2 {
		t.Errorf("calls after an unreadable list: %+v", calls)
	}
}

func TestSetStatus(t *testing.T) {
	endpoint := "repos/octo/widgets/statuses/" + sha
	long := "Cost regressed by 21% (96.5% interval 4% to 41%).\n\x1b[2J" + strings.Repeat("ü", 200)
	for _, c := range []struct {
		state State
		desc  string
		want  string
	}{
		{StatePending, "Queued for tonight's pass", "Queued for tonight's pass"},
		{StateSuccess, long, ""},
		{StateSuccess, "Agentium could not screen this commit: a run failed to start", "Agentium could not screen this commit: a run failed to start"},
	} {
		fake := newFakeGH(t, rule{match: []string{"--method", "POST", endpoint}, stdout: `{"id":1}`})
		if err := fake.client(t).SetStatus(context.Background(), repo, sha, c.state, c.desc); err != nil {
			t.Fatal(err)
		}
		calls := fake.calls(t)
		if want := []string{"api", "--hostname", "github.com", "--method", "POST", endpoint, "--input", "-"}; len(calls) != 1 || !slices.Equal(calls[0].args, want) {
			t.Fatalf("calls %+v", calls)
		}
		var sent map[string]string
		if err := json.Unmarshal([]byte(calls[0].stdin), &sent); err != nil {
			t.Fatal(err)
		}
		if sent["state"] != string(c.state) || sent["context"] != StatusContext || len(sent) != 3 {
			t.Errorf("sent %q", sent)
		}
		desc := sent["description"]
		if c.want != "" && desc != c.want {
			t.Errorf("description %q, want %q", desc, c.want)
		}
		if n := len([]rune(desc)); n > maxDescription || strings.ContainsAny(desc, "\n\x1b") {
			t.Errorf("description %q (%d characters)", desc, n)
		}
	}
	// A warn-only screen never fails a pull request: "error" and "failure" both would, and are refused before gh runs,
	// as is anything else.
	fake := newFakeGH(t)
	for _, state := range []State{"error", "failure", "neutral", "Success", ""} {
		if err := fake.client(t).SetStatus(context.Background(), repo, sha, state, "x"); err == nil {
			t.Errorf("state %q accepted", state)
		}
	}
	if calls := fake.calls(t); len(calls) != 0 {
		t.Errorf("gh ran: %+v", calls)
	}
}

func TestTimeoutKillsGH(t *testing.T) {
	fake := newFakeGH(t, rule{match: []string{"user"}, sleep: true})
	c, err := New(fake.environ, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = c.Login(context.Background())
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("error %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %s", elapsed)
	}
	// Cancelling the caller's context stops gh too, and is reported as the cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	c.timeout = time.Minute
	if _, err := c.Login(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v, want the context's", err)
	}
}

func TestMissingOrSignedOutGH(t *testing.T) {
	// gh is not on PATH (and relative PATH entries are never searched).
	empty := t.TempDir()
	if err := os.WriteFile(filepath.Join(empty, "gh"), []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil { // not executable
		t.Fatal(err)
	}
	for _, environ := range [][]string{nil, {"PATH=" + empty}, {"PATH=bin:.:" + empty}} {
		if _, err := New(environ, 0); !errors.Is(err, ErrNoGH) {
			t.Errorf("PATH %q: error %v, want ErrNoGH", environ, err)
		}
	}
	// gh's exit code 4 (authentication required) and an HTTP 401 (a revoked token) both mean "log in".
	for _, answer := range []rule{
		{match: []string{"user"}, stderr: "To get started with GitHub CLI, please run:  gh auth login", code: 4},
		{match: []string{"user"}, stderr: "gh: Bad credentials (HTTP 401)", code: 1},
	} {
		fake := newFakeGH(t, answer)
		_, err := fake.client(t).Login(context.Background())
		if !errors.Is(err, ErrNotLoggedIn) {
			t.Errorf("error %v, want ErrNotLoggedIn", err)
		}
	}
	var zero Client
	if _, err := zero.Login(context.Background()); err == nil {
		t.Error("a zero client ran")
	}
}

// gh gets the user's login and nothing else that is secret, and cannot be redirected to another host or made to log.
func TestGHEnvironment(t *testing.T) {
	fake := newFakeGH(t, rule{match: []string{"user"}, stdout: `{"login":"alice"}`})
	if _, err := fake.client(t).Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, kv := range fake.calls(t)[0].env {
		name, value, _ := strings.Cut(kv, "=")
		env[name] = value
	}
	for name, want := range map[string]string{"GH_TOKEN": "gho_user", "GH_PROMPT_DISABLED": "1", "GH_NO_UPDATE_NOTIFIER": "1",
		"GH_TELEMETRY": "false", "NO_COLOR": "1", "LANG": "C", "DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/501/bus",
		"XDG_RUNTIME_DIR": "/run/user/501"} {
		if env[name] != want {
			t.Errorf("%s = %q, want %q", name, env[name], want)
		}
	}
	if !strings.HasPrefix(env["PATH"], fake.dir) || env["HOME"] == "" {
		t.Errorf("PATH %q, HOME %q", env["PATH"], env["HOME"])
	}
	// GITHUB_TOKEN is often another tool's or another account's: only gh's explicit GH_TOKEN passes.
	for _, gone := range []string{"GITHUB_TOKEN", "ANTHROPIC_API_KEY", "SSH_AUTH_SOCK", "GIT_DIR", "AGENTIUM_HOME", "GH_HOST", "GH_DEBUG", "GH_REPO",
		"GH_ENTERPRISE_TOKEN", "EDITOR"} {
		if _, ok := env[gone]; ok {
			t.Errorf("%s reached gh", gone)
		}
	}
}

// A rate limit is its own error (the caller waits for the next pass); a 403 without one is an ordinary failure.
func TestRateLimit(t *testing.T) {
	for _, c := range []struct {
		stderr string
		want   error
	}{
		{"gh: API rate limit exceeded for user ID 1. (HTTP 403)", ErrRateLimited},
		{"gh: You have exceeded a secondary rate limit. Please wait a few minutes before you try again. (HTTP 403)", ErrRateLimited},
		{"gh: API rate limit exceeded (HTTP 429)", ErrRateLimited},
		{"gh: Resource not accessible by personal access token (HTTP 403)", nil},
	} {
		fake := newFakeGH(t, rule{match: []string{"user"}, stderr: c.stderr, code: 1})
		_, err := fake.client(t).Login(context.Background())
		var call *CallError
		switch {
		case c.want != nil && !errors.Is(err, c.want):
			t.Errorf("%q: error %v, want %v", c.stderr, err, c.want)
		case c.want == nil && (errors.Is(err, ErrRateLimited) || !errors.As(err, &call)):
			t.Errorf("%q: error %v, want a CallError", c.stderr, err)
		}
	}
}

// Login is the identity a caller shows before posting; logins GitHub no longer issues are refused (fail-closed).
func TestLogin(t *testing.T) {
	for login, ok := range map[string]bool{"alice": true, "Alice-B": true, "a1": true, "legacy-": false, "le--gacy": false, "": false,
		"$(id)": false} {
		fake := newFakeGH(t, rule{match: []string{"user"}, stdout: jsonOf(t, map[string]string{"login": login})})
		got, err := fake.client(t).Login(context.Background())
		if ok && (err != nil || got != login) || !ok && err == nil {
			t.Errorf("login %q: %q, %v", login, got, err)
		}
	}
}

func TestPrintable(t *testing.T) {
	if got := printable("  a\x1b[31mb\nc\x00d  ", 10); got != "a[31mb cd" {
		t.Errorf("printable = %q", got)
	}
	if got := printable("abcdef", 3); got != "abc…" {
		t.Errorf("printable cut = %q", got)
	}
}
