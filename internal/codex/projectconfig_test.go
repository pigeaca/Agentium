package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A trusted checkout's Codex configuration is refused unless it only sets the allowed keys on one line each: Codex
// merges it into the run's settings key by key, so anything else (a sandbox mode, a feature, an MCP server, its own
// entries in the agentium profile or the shell policy) could change what the run is given. Every folder from the root
// down to the start folder is a project layer; links, rules, hooks and other files are refused too.
func TestProjectConfigRefusal(t *testing.T) {
	write := func(repo, rel, content string) {
		t.Helper()
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	allowed := "# the project's own choices\nmodel = \"gpt-5.5\"\nmodel_reasoning_effort = 'high'  # ours win\n\nmodel_verbosity = \"low\"\nproject_doc_max_bytes = 65_536\nmodel_reasoning_summary = \"concise\"\n"
	for name, c := range map[string]struct {
		files  map[string]string
		module string
		ok     bool
	}{
		"no .codex":                        {map[string]string{"AGENTS.md": "x"}, "", true},
		"allowed settings and skills":      {map[string]string{".codex/config.toml": allowed, ".codex/skills/s/SKILL.md": "x"}, "", true},
		"a sandbox mode":                   {map[string]string{".codex/config.toml": "sandbox_mode = \"danger-full-access\"\n"}, "", false},
		"network in the profile":           {map[string]string{".codex/config.toml": "[permissions.agentium.network]\nenabled = true\n"}, "", false},
		"a feature, dotted":                {map[string]string{".codex/config.toml": "features.network_proxy = true\n"}, "", false},
		"an MCP server":                    {map[string]string{".codex/config.toml": "[mcp_servers.x]\ncommand = \"/bin/sh\"\n"}, "", false},
		"the shell policy":                 {map[string]string{".codex/config.toml": "shell_environment_policy.include_only = [\"PATH\"]\n"}, "", false},
		"approval":                         {map[string]string{".codex/config.toml": "approval_policy = \"on-request\"\n"}, "", false},
		"a quoted key":                     {map[string]string{".codex/config.toml": "\"model\" = \"x\"\n"}, "", false},
		"a multi-line value":               {map[string]string{".codex/config.toml": "model = \"\"\"\nx\n\"\"\"\n"}, "", false},
		"an inline table":                  {map[string]string{".codex/config.toml": "model = { a = 1 }\n"}, "", false},
		"hooks":                            {map[string]string{".codex/hooks.json": "{}"}, "", false},
		"rules":                            {map[string]string{".codex/rules/default.rules": "x"}, "", false},
		"a module's own layer":             {map[string]string{"svc/.codex/config.toml": "sandbox_mode = \"danger-full-access\"\n"}, "svc", false},
		"another module's layer, not read": {map[string]string{"other/.codex/config.toml": "sandbox_mode = \"danger-full-access\"\n"}, "svc", true},
	} {
		repo := t.TempDir()
		for rel, content := range c.files {
			write(repo, rel, content)
		}
		err := ProjectConfigRefusal(repo, c.module)
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", name, err)
		}
		if err != nil && !strings.Contains(err.Error(), "Codex merges a trusted project's configuration") {
			t.Errorf("%s: the refusal does not say why: %v", name, err)
		}
	}
	link := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(link, ".codex")); err != nil {
		t.Fatal(err)
	}
	if err := ProjectConfigRefusal(link, ""); err == nil {
		t.Error("a linked .codex was followed")
	}
	// A config.toml that is a link (to a file outside, here a stand-in for ~/.netrc) is refused, never read: only a
	// regular file is a project's configuration.
	outside := filepath.Join(t.TempDir(), ".netrc")
	if err := os.WriteFile(outside, []byte("model = \"gpt-6.1-sol\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linked := t.TempDir()
	write(linked, ".codex/skills/s/SKILL.md", "x")
	if err := os.Symlink(outside, filepath.Join(linked, ".codex", "config.toml")); err != nil {
		t.Fatal(err)
	}
	if err := ProjectConfigRefusal(linked, ""); err == nil || !strings.Contains(err.Error(), ".codex/config.toml") {
		t.Errorf("a linked config.toml: %v", err)
	}
}
