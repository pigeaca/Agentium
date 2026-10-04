package sandbox

import (
	"path/filepath"
	"testing"
)

// Every credential store grading denies is denied to agent runs too: the deny list every agent run shares
// (AgentDenied.Reads: the credential stores every sandbox denies, and the build tools' user caches) covers each of
// graderCredentialFiles, and the environment's moved pip and uv configs as well, whatever the agent adds of its own.
func TestAgentsDenyWhatGradingDenies(t *testing.T) {
	home := "/home/u"
	environ := []string{"HOME=" + home, "PIP_CONFIG_FILE=/cfg/pip.conf", "UV_CONFIG_FILE=/cfg/uv.toml", "XDG_CONFIG_HOME=/xdg"}
	denied := AgentDenied{Home: home, Environ: environ}.Reads()
	covered := func(path string) bool {
		for _, d := range denied {
			if rel, err := filepath.Rel(d, path); err == nil && filepath.IsLocal(rel) {
				return true
			}
		}
		return false
	}
	var want []string
	for _, name := range graderCredentialFiles() {
		want = append(want, filepath.Join(home, name))
	}
	want = append(want, "/cfg/pip.conf", "/cfg/uv.toml", filepath.Join(home, ".config/pypoetry/auth.toml"),
		filepath.Join(home, ".local/share/python_keyring"), filepath.Join(home, ".config/uv/uv.toml"), "/xdg/uv/uv.toml", "/xdg/uv/other", "/xdg/pip/pip.conf")
	for _, path := range want {
		if !covered(path) {
			t.Errorf("%s is denied to grading but not to agent runs", path)
		}
	}
}
