package watch

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/pigeaca/agentium/internal/claude"
)

// SignIn is how Agentium signs in to Claude Code, as far as consent is concerned: the mode, and a fingerprint that
// tells two sign-ins of one mode apart without deriving from a secret.
//   - A token file's identity is a digest of its absolute path, device and inode: another file, or the path replaced by
//     a new file (a rename), changes it. A token rewritten in place in the same file does not.
//   - A login and an API key have no identity (empty): Agentium reads neither the account behind Claude Code's login
//     (its config and keychain) nor the key, so switching the account or the key under the same mode is not detected.
type SignIn struct {
	Mode     string // claude.SignIn*
	Identity string
}

// SignInOf is the sign-in for mode; tokenFile is the token file's path (project.TokenFile), read only with the
// token-file mode, and only its metadata.
func SignInOf(mode, tokenFile string) (SignIn, error) {
	if mode != claude.SignInTokenFile {
		return SignIn{Mode: mode}, nil
	}
	path, err := filepath.Abs(tokenFile)
	if err != nil {
		return SignIn{}, fmt.Errorf("the token file's identity: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return SignIn{}, fmt.Errorf("the token file's identity: %w", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return SignIn{}, fmt.Errorf("the token file's identity: no device and inode for %s", path)
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "token-file\x00%s\x00%d\x00%d", path, st.Dev, st.Ino))
	return SignIn{Mode: mode, Identity: hex.EncodeToString(sum[:])}, nil
}

// changeTo says how to differs from s, or "" when it does not.
func (s SignIn) changeTo(to SignIn) string {
	switch {
	case s.Mode != to.Mode:
		return fmt.Sprintf("from %s to %s", s.Mode, to.Mode)
	case s.Identity != to.Identity:
		return "to another " + s.Mode
	}
	return ""
}
