package ghx

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// PullRequest is an open pull request whose head is the commit asked about. Only identifiers are kept: titles and
// bodies are never read.
type PullRequest struct {
	Number  int
	URL     string // its page on github.com
	HeadSHA string
}

// PullRequests returns the open pull requests of repo whose head commit is sha, by number; none is not an error (the
// screen waits until one exists). Several are possible (one branch opened against two bases, or two branches at the
// same commit): the caller chooses.
//
// It asks GitHub for the pull requests associated with the commit (GET repos/{o}/{r}/commits/{sha}/pulls) rather
// than searching (`gh pr list --search <sha>`): the lookup is exact and current, while search reads an index that
// lags new pushes, has its own low rate limit, and also matches a pull request that merely mentions the commit in its
// text. The associated list holds every pull request whose branch contains the commit, so it is filtered here to open
// ones whose head is exactly sha and whose base is repo. A commit GitHub does not have yet (not pushed) is none.
func (c Client) PullRequests(ctx context.Context, repo Repo, sha string) ([]PullRequest, error) {
	if err := repo.Valid(); err != nil {
		return nil, err
	}
	if err := checkCommit(sha); err != nil {
		return nil, err
	}
	out, err := c.api(ctx, nil, "--paginate", "repos/"+repo.String()+"/commits/"+sha+"/pulls?per_page=100")
	var call *CallError
	if errors.As(err, &call) && strings.Contains(call.Stderr, "No commit found for SHA") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	type pull struct {
		Number  int    `json:"number"`
		State   string `json:"state"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
	}
	pulls, err := decodeAll[pull](out)
	if err != nil {
		return nil, err
	}
	var found []PullRequest
	for _, p := range pulls {
		if p.State == "open" && p.Head.SHA == sha && strings.EqualFold(p.Base.Repo.FullName, repo.String()) && p.Number > 0 &&
			!slices.ContainsFunc(found, func(f PullRequest) bool { return f.Number == p.Number }) {
			found = append(found, PullRequest{Number: p.Number, URL: p.HTMLURL, HeadSHA: p.Head.SHA})
		}
	}
	slices.SortFunc(found, func(a, b PullRequest) int { return cmp.Compare(a.Number, b.Number) })
	return found, nil
}

// ScreenMarker marks the cost screen's comment. It is an HTML comment, so GitHub does not show it.
const ScreenMarker = "<!-- agentium:cost-screen -->"

var markerPattern = regexp.MustCompile(`^<!-- agentium:[a-z0-9-]+ -->$`)

// maxCommentRunes is GitHub's limit on a comment's body.
const maxCommentRunes = 65536

// Comment is the marked comment after an upsert.
type Comment struct {
	ID      int64
	URL     string
	Created bool // false: an existing comment was edited
}

// UpsertComment keeps one comment on pull request number of repo: marker on the first line, then body. It edits the
// comment that the signed-in account (Login) wrote and whose body starts with that marker line, the oldest if there
// are several, or creates one. A comment anyone else wrote is never edited, whatever it contains: another account can
// copy the marker, and only authorship (the API's user.login) is trusted. body is the caller's own text (Agentium's
// redacted report); it reaches gh only on stdin, inside a JSON request.
func (c Client) UpsertComment(ctx context.Context, repo Repo, number int, marker, body string) (Comment, error) {
	if err := repo.Valid(); err != nil {
		return Comment{}, err
	}
	if number <= 0 {
		return Comment{}, fmt.Errorf("pull request number %d is not valid", number)
	}
	if !markerPattern.MatchString(marker) {
		return Comment{}, fmt.Errorf("marker %q is not an Agentium marker (<!-- agentium:name -->)", marker)
	}
	if !utf8.ValidString(body) {
		return Comment{}, errors.New("the comment is not valid UTF-8")
	}
	text := marker + "\n" + body
	if n := utf8.RuneCountInString(text); n > maxCommentRunes {
		return Comment{}, fmt.Errorf("the comment has %d characters; GitHub allows %d", n, maxCommentRunes)
	}
	request, err := json.Marshal(map[string]string{"body": text})
	if err != nil {
		return Comment{}, fmt.Errorf("encode the comment: %w", err)
	}
	login, err := c.Login(ctx)
	if err != nil {
		return Comment{}, err
	}
	issue := "repos/" + repo.String() + "/issues/" + strconv.Itoa(number)
	out, err := c.api(ctx, nil, "--paginate", issue+"/comments?per_page=100")
	if err != nil {
		return Comment{}, err
	}
	type listed struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	comments, err := decodeAll[listed](out)
	if err != nil {
		return Comment{}, err
	}
	var own int64
	for _, cm := range comments {
		if cm.ID > 0 && strings.EqualFold(cm.User.Login, login) && startsWithLine(cm.Body, marker) && (own == 0 || cm.ID < own) {
			own = cm.ID
		}
	}
	method, endpoint := "POST", issue+"/comments"
	if own != 0 {
		method, endpoint = "PATCH", "repos/"+repo.String()+"/issues/comments/"+strconv.FormatInt(own, 10)
	}
	out, err = c.api(ctx, request, "--method", method, endpoint, "--input", "-")
	if err != nil {
		return Comment{}, err
	}
	var saved struct {
		ID      int64  `json:"id"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(out, &saved); err != nil {
		return Comment{}, fmt.Errorf("read gh's JSON: %w", err)
	}
	return Comment{ID: saved.ID, URL: saved.HTMLURL, Created: own == 0}, nil
}

// startsWithLine reports whether body's first line is exactly line (GitHub may store \r\n line ends).
func startsWithLine(body, line string) bool {
	first, _, _ := strings.Cut(body, "\n")
	return strings.TrimSuffix(first, "\r") == line
}

// State is a commit status's state. Only the three a warn-only screen uses exist: GitHub's fourth, "failure", would
// fail the pull request's checks, and the screen must never do that.
type State string

const (
	// StatePending: the check is queued or running.
	StatePending State = "pending"
	// StateSuccess: the check finished, whatever its verdict ("regressed" included); the verdict is in the description.
	// Commit statuses have no "neutral" (that is a check run's conclusion, which needs a GitHub App), so success it is.
	StateSuccess State = "success"
	// StateError: the check itself broke (not the verdict).
	StateError State = "error"
)

// StatusContext names the screen's commit status on GitHub.
const StatusContext = "agentium/cost-screen"

// maxDescription is GitHub's limit on a status description.
const maxDescription = 140

// SetStatus sets the commit status StatusContext on sha in repo. description is the caller's text (the verdict in
// words), stripped of control characters and cut to GitHub's 140 characters. It is sent on stdin as JSON.
func (c Client) SetStatus(ctx context.Context, repo Repo, sha string, state State, description string) error {
	if err := repo.Valid(); err != nil {
		return err
	}
	if err := checkCommit(sha); err != nil {
		return err
	}
	switch state {
	case StatePending, StateSuccess, StateError:
	default:
		return fmt.Errorf("commit status state %q is not one the screen sets (pending, success, error)", state)
	}
	request, err := json.Marshal(map[string]string{"state": string(state), "context": StatusContext,
		"description": printable(description, maxDescription-1)})
	if err != nil {
		return fmt.Errorf("encode the status: %w", err)
	}
	_, err = c.api(ctx, request, "--method", "POST", "repos/"+repo.String()+"/statuses/"+sha, "--input", "-")
	return err
}
