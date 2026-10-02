package ghx

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/pigeaca/agentium/internal/gitx"
)

// Host is the only GitHub host the adapter talks to. Every gh call names it (--hostname), so a GH_HOST in the user's
// environment cannot send a call elsewhere.
const Host = "github.com"

// Repo is a GitHub repository's identity. It comes only from the user's git remote (RepoOf), never from pull request
// text, and both parts are checked against GitHub's own name rules before they go into an API path.
type Repo struct {
	Owner string
	Name  string
}

// String is "owner/name".
func (r Repo) String() string { return r.Owner + "/" + r.Name }

var (
	// GitHub logins and organisation names: letters, digits and single hyphens, not at either end, at most 39.
	ownerPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9]|-[A-Za-z0-9]){0,38}$`)
	// Repository names: letters, digits, '.', '-', '_' (at most 100); "." and ".." are not names.
	namePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	remoteName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
)

// Valid reports whether both parts follow GitHub's naming rules, so an API path built from them stays inside
// repos/<owner>/<name>/ (no '/', '?', '#', '..', or a leading '-').
func (r Repo) Valid() error {
	if !ownerPattern.MatchString(r.Owner) {
		return fmt.Errorf("repository owner %q is not a GitHub owner name", r.Owner)
	}
	if !namePattern.MatchString(r.Name) || r.Name == "." || r.Name == ".." {
		return fmt.Errorf("repository name %q is not a GitHub repository name", r.Name)
	}
	return nil
}

// ErrNotGitHub is returned when a remote's URL does not name a repository on github.com.
var ErrNotGitHub = errors.New("not a github.com repository URL")

// ParseRemote reads a github.com repository from a git remote URL: https://github.com/o/r(.git), ssh://git@github.com/o/r,
// git://github.com/o/r, or the scp form git@github.com:o/r.git (also with a leading slash, git@github.com:/o/r.git).
// Other hosts (GitHub Enterprise, ssh host aliases) are ErrNotGitHub. A URL may carry a user and a token
// (https://x:TOKEN@github.com/...): the returned error never includes the URL or any part of it.
func ParseRemote(raw string) (Repo, error) {
	raw = strings.TrimSpace(raw)
	var host, path string
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return Repo{}, ErrNotGitHub // url's error quotes the URL
		}
		switch u.Scheme {
		case "https", "http", "ssh", "git", "git+ssh", "ssh+git":
		default:
			return Repo{}, ErrNotGitHub
		}
		if u.RawQuery != "" || u.Fragment != "" {
			return Repo{}, ErrNotGitHub
		}
		host, path = u.Hostname(), u.Path
		if u.Scheme == "ssh" && host == "ssh.github.com" { // GitHub's ssh over port 443
			host = Host
		}
	} else {
		// scp form: [user@]host:path. A local path (no colon, or a slash before it) is not a remote on GitHub.
		at := strings.LastIndex(raw[:max(strings.Index(raw, ":"), 0)], "@")
		hostPart, rest, ok := strings.Cut(raw[at+1:], ":")
		if !ok || strings.Contains(hostPart, "/") {
			return Repo{}, ErrNotGitHub
		}
		host, path = hostPart, rest
	}
	if !strings.EqualFold(host, Host) && !strings.EqualFold(host, "www."+Host) {
		return Repo{}, ErrNotGitHub
	}
	path = strings.TrimSuffix(strings.TrimSuffix(strings.Trim(path, "/"), ".git"), "/")
	owner, name, ok := strings.Cut(path, "/")
	if !ok || strings.Contains(name, "/") {
		return Repo{}, ErrNotGitHub
	}
	repo := Repo{Owner: owner, Name: name}
	if repo.Valid() != nil {
		return Repo{}, ErrNotGitHub
	}
	return repo, nil
}

// RepoOf is the github.com repository of remote (for example "origin") in the git repository at dir, read through
// gitx (hooks and fsmonitor off, inherited GIT_* dropped). The remote lives in the repository's own config, which a
// commit or a pull request cannot change. Errors name the remote, never its URL (it may hold a token).
func RepoOf(ctx context.Context, dir, remote string) (Repo, error) {
	if !remoteName.MatchString(remote) {
		return Repo{}, fmt.Errorf("remote name %q is not valid", remote)
	}
	raw, err := gitx.Run(ctx, "-C", dir, "remote", "get-url", "--", remote)
	if err != nil {
		if ctx.Err() != nil {
			return Repo{}, fmt.Errorf("read remote %s: %w", remote, ctx.Err())
		}
		return Repo{}, fmt.Errorf("read remote %s: no such remote, or not a git repository", remote)
	}
	repo, err := ParseRemote(raw)
	if err != nil {
		return Repo{}, fmt.Errorf("remote %s: %w", remote, err)
	}
	return repo, nil
}
