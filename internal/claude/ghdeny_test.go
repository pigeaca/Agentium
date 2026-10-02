package claude

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/buildtool"
)

// ghEnviron is a user's environment logged in to GitHub every way gh reads: token variables for github.com and for
// GitHub Enterprise, beside the variables a run keeps.
func ghEnviron(home string) []string {
	return []string{"PATH=/usr/bin:/bin", "HOME=" + home, "LANG=C", "GOPATH=" + home + "/go", "GH_TOKEN=gho_user", // secret-scan: allow
		"GITHUB_TOKEN=ghp_user", "GH_ENTERPRISE_TOKEN=e", "GITHUB_ENTERPRISE_TOKEN=e", "GH_CONFIG_DIR=" + home + "/.config/gh"} // secret-scan: allow
}

var ghTokens = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

// The pull-request screen (internal/ghx) uses the user's own gh login. That login must never reach an agent: gh's
// config folder (~/.config/gh, where gh keeps its hosts and, without a keychain, its token) is denied to the shell,
// the Read tool and the sandbox's credential files in every sign-in mode, and gh's token variables are dropped from
// every run's and judge call's environment, whatever build-tool profiles widen the allowlist; GH_CONFIG_DIR, which
// would point the agent at a config folder elsewhere, does not pass either.
func TestGHLoginNeverReachesTheAgent(t *testing.T) {
	if !slices.Contains(credentialFiles(), ".config/gh") {
		t.Fatalf("credential files %q do not include .config/gh", credentialFiles())
	}
	// A real home folder: paths under /home/u make each command resolve missing paths slowly on macOS.
	home := t.TempDir()
	environ := ghEnviron(home)
	for _, mode := range []string{SignInLogin, SignInAPIKey, SignInTokenFile} {
		secret := ""
		if mode != SignInLogin {
			secret = "S"
		}
		inv := invocation(t, mode, secret)
		inv.Home = home
		args, env, err := inv.Command(environ)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		var settings struct {
			Sandbox struct {
				Filesystem struct {
					DenyRead []string `json:"denyRead"`
				} `json:"filesystem"`
				Credentials struct {
					Files []map[string]string `json:"files"`
				} `json:"credentials"`
			} `json:"sandbox"`
			Permissions struct {
				Deny []string `json:"deny"`
			} `json:"permissions"`
		}
		if err := json.Unmarshal([]byte(flagValue(args, "--settings")), &settings); err != nil {
			t.Fatal(err)
		}
		gh := home + "/.config/gh"
		if !slices.Contains(settings.Sandbox.Filesystem.DenyRead, gh) {
			t.Errorf("%s: the sandbox does not deny reading %s", mode, gh)
		}
		if !slices.Contains(settings.Permissions.Deny, "Read(/"+gh+"/**)") {
			t.Errorf("%s: the Read tool is not denied %s", mode, gh)
		}
		if !slices.ContainsFunc(settings.Sandbox.Credentials.Files, func(f map[string]string) bool { return f["path"] == gh && f["mode"] == "deny" }) {
			t.Errorf("%s: %s is not a denied credential file", mode, gh)
		}
		checkNoGHToken(t, mode+" run", env)

		_, judge, err := Judgement{CLI: "/c", Dir: "/d", Model: "m", SystemPrompt: "s", Schema: "{}", Home: home, SignIn: mode,
			Secret: secret, ConfigDir: inv.ConfigDir}.Command(environ)
		if err != nil {
			t.Fatalf("%s judge: %v", mode, err)
		}
		checkNoGHToken(t, mode+" judge", judge)
	}
	for _, tools := range [][]string{nil, {"go"}, {"maven"}, {"gradle"}, {"cargo"}} {
		checkNoGHToken(t, "profiles "+strings.Join(tools, ","), EnvironFor(environ, buildtool.Select(tools)))
	}
}

func checkNoGHToken(t *testing.T, what string, env []string) {
	t.Helper()
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if slices.Contains(ghTokens, name) || name == "GH_CONFIG_DIR" || strings.Contains(kv, "gho_user") || strings.Contains(kv, "ghp_user") {
			t.Errorf("%s: %s reached the agent", what, name)
		}
	}
}
