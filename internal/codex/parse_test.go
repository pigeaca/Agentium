package codex

import (
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/agent"
)

// records is a run's records folder with the spike's fixtures: the exec stream (exec-NAME.jsonl) as stream.jsonl and,
// when rollout is set, its rollout (rollout-NAME.jsonl) as Gather leaves it. extra lines are appended to the stream.
func records(t *testing.T, stream, rollout string, extra ...string) string {
	t.Helper()
	dir := t.TempDir()
	data, err := os.ReadFile(filepath.Join("testdata", "exec-"+stream+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range extra {
		data = append(data, []byte(line+"\n")...)
	}
	if err := os.WriteFile(filepath.Join(dir, agent.Transcript), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if rollout != "" {
		data, err := os.ReadFile(filepath.Join("testdata", "rollout-"+rollout+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, Rollout), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func parse(t *testing.T, dir string) agent.Metrics {
	t.Helper()
	m, err := Adapter{}.Parse(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// cost prices the spike's totals at the table's rates, as an independent check of per-request pricing: uncached input
// at $2, cached at $0.20, output at $10 per million.
func cost(input, cached, output int64) float64 {
	return (float64(input-cached)*2 + float64(cached)*0.2 + float64(output)*10) / 1e6
}

// The golden path (session 5): the stream's commands and changed file, the rollout's session settings, requests and
// windows; four requests priced by Agentium; a fair, ok run.
func TestParseOK(t *testing.T) {
	m := parse(t, records(t, "ok", "ok"))
	if !m.SawInit || !m.SawResult || m.Result != ResultCompleted || m.ResultIsError {
		t.Errorf("result: %+v", m)
	}
	if len(m.Commands) != 4 || !strings.Contains(m.Commands[3], "go test ./...") || !slices.Equal(m.RanCommands, m.Commands) {
		t.Errorf("commands %q", m.Commands)
	}
	if !slices.Equal(m.FilePaths, []string{"<run>/checkout/calc.go"}) {
		t.Errorf("files %q", m.FilePaths)
	}
	if m.CLIVersion != "0.160.0" || m.Model != "gpt-6.1-sol" || m.Effort != "medium" || m.ApprovalPolicy != "never" ||
		m.SandboxPolicy != "workspace-write" || m.PermissionProfile != "agentium" || m.NetworkAccess || m.CWD != "<run>/checkout" {
		t.Errorf("session: %+v", m)
	}
	// The stream's turn.completed totals: 44,416 input (21,248 cached), 316 output; the rollout's four requests agree.
	if m.Turns != 4 || m.FirstRequest != 10692 || m.InputTokens != 44416-21248 || m.CacheReadTokens != 21248 || m.OutputTokens != 316 ||
		m.ReasoningTokens != 0 || m.Rollouts != 1 || m.UnpricedRequests != 0 {
		t.Errorf("tokens: %+v", m)
	}
	if want := cost(44416, 21248, 316); math.Abs(m.CostUSD-want) > 1e-12 {
		t.Errorf("cost $%.7f, want $%.7f", m.CostUSD, want)
	}
	if m.UsageFirst == nil || m.UsageLast == nil || m.UsageLast.FiveHour != 0.02 || m.UsageLast.SevenDay != 0 || m.UsageLast.Status != "allowed" ||
		m.UsageLast.FiveHourResets.Unix() != 1791143360 {
		t.Errorf("usage %+v", m.UsageLast)
	}
	if m.DurationMS != 23260 || m.Denials != 0 || m.ResultExcerpt != "Created notes.txt with the exact heredoc, fixed Sub, and `go test ./...` passed." {
		t.Errorf("duration %d, denials %d, result %q", m.DurationMS, m.Denials, m.ResultExcerpt)
	}
	drift := Check(m, agent.Expect{RequestedModel: "gpt-6.1-sol", Effort: "medium", CLIVersion: "0.160.0"})
	if len(drift) != 0 || Classify(m, agent.StopNone, drift) != agent.OutcomeOK {
		t.Errorf("drift %q, outcome %s", drift, Classify(m, agent.StopNone, drift))
	}
	// The final message comes from -o's file when Codex wrote one.
	dir := records(t, "ok", "ok")
	if err := os.WriteFile(filepath.Join(dir, LastMessage), []byte("from the file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if m := parse(t, dir); m.ResultExcerpt != "from the file" {
		t.Errorf("result %q", m.ResultExcerpt)
	}
}

// An interrupted run (session 4: SIGINT mid-turn): the stream has no final event and no usage, but the rollout keeps
// every request, so its spend is read; capped or timed out by Agentium, infra otherwise.
func TestParseInterrupted(t *testing.T) {
	m := parse(t, records(t, "interrupted", "interrupted"))
	if m.SawResult || m.Result != "" || m.Turns != 5 || m.ReasoningTokens != 66 || m.DurationMS != 44923 {
		t.Errorf("%+v", m)
	}
	if want := cost(54875, 43008, 396); math.Abs(m.CostUSD-want) > 1e-12 || m.CostUSD == 0 {
		t.Errorf("spend $%.7f, want $%.7f", m.CostUSD, want)
	}
	// The spike ran this session without an effort, so its rollout records none: drift, since Agentium always passes one.
	if drift := Check(m, agent.Expect{RequestedModel: "gpt-6.1-sol", Effort: "medium"}); !slices.Equal(drift, []string{`effort "", not "medium"`}) {
		t.Errorf("drift %q", drift)
	}
	m.Effort = "medium" // as an Agentium run's would be
	drift := Check(m, agent.Expect{RequestedModel: "gpt-6.1-sol", Effort: "medium"})
	for stop, want := range map[agent.Stop]string{agent.StopCap: agent.OutcomeCapped, agent.StopTimeout: agent.OutcomeTimeout,
		agent.StopNone: agent.OutcomeInfra, agent.StopBlind: agent.OutcomeInfra} {
		if got := Classify(m, stop, drift); got != want {
			t.Errorf("stopped %q: %s, want %s (drift %q)", stop, got, want, drift)
		}
	}
}

// Denials (session 2): the stream shows none of the denied calls; the rollout's tool outputs show each, through the
// shell, apply_patch (a read and a write outside the profile), view_image, and git's .git.
func TestParseDenials(t *testing.T) {
	m := parse(t, records(t, "denials", "denials"))
	if m.Denials != 5 {
		t.Errorf("denials %d, want 5", m.Denials)
	}
	for _, c := range m.Commands {
		if strings.Contains(c, "<decoy>") || strings.Contains(c, "git commit") {
			t.Errorf("a denied command is in the stream's: %s", c)
		}
	}
	if len(m.Commands) != 5 || m.Turns != 6 {
		t.Errorf("commands %q, requests %d", m.Commands, m.Turns)
	}
}

// Network failures: with unbounded retries off, the turn fails (infra); with them on, Codex waits for the network
// until stopped, which is infra too, even at the timeout.
func TestParseNetworkFailures(t *testing.T) {
	failed := parse(t, records(t, "network-failed", ""))
	if failed.Result != ResultFailed || !failed.ResultIsError || failed.ResultExcerpt != "Connection failed: error sending request" ||
		Classify(failed, agent.StopNone, Check(failed, agent.Expect{})) != agent.OutcomeInfra {
		t.Errorf("turn.failed: %+v", failed)
	}
	waiting := parse(t, records(t, "waiting-for-network", ""))
	if waiting.Result != ResultNetwork {
		t.Errorf("waiting: result %q", waiting.Result)
	}
	for _, stop := range []agent.Stop{agent.StopTimeout, agent.StopNone} {
		if got := Classify(waiting, stop, Check(waiting, agent.Expect{})); got != agent.OutcomeInfra {
			t.Errorf("waiting for the network, stopped %q: %s", stop, got)
		}
	}
	// Stopped at its cap, a run is capped whatever Codex was doing: a run stopped for its spend is never retried.
	if got := Classify(waiting, agent.StopCap, nil); got != agent.OutcomeCapped {
		t.Errorf("waiting for the network, stopped at the cap: %s", got)
	}
	// A reconnect the run recovered from is not an outage.
	recovered := parse(t, records(t, "ok", "ok"))
	recovered.APIRetries = 3
	if Classify(recovered, agent.StopNone, nil) != agent.OutcomeOK {
		t.Error("a recovered reconnect made the run infra")
	}
}

// Drift: a reroute (the stream's), another model, effort, approval, profile or network, an MCP call, another CLI
// version, a missing rollout. Each makes the run unfair.
func TestCheckDrift(t *testing.T) {
	reroute := `{"type":"item.completed","item":{"id":"item_9","type":"error","message":"model rerouted: gpt-6.1-sol -> gpt-5.5 (HighRiskCyberActivity)"}}`
	m := parse(t, records(t, "ok", "ok", reroute))
	if drift := Check(m, agent.Expect{RequestedModel: "gpt-6.1-sol", Effort: "medium"}); !slices.Contains(drift, "model rerouted: gpt-6.1-sol -> gpt-5.5 (HighRiskCyberActivity)") ||
		Classify(m, agent.StopNone, drift) != agent.OutcomeUnfair {
		t.Errorf("reroute: %q", drift)
	}
	base := parse(t, records(t, "ok", "ok"))
	for name, c := range map[string]struct {
		change func(*agent.Metrics)
		expect agent.Expect
	}{
		"another model":         {func(m *agent.Metrics) {}, agent.Expect{RequestedModel: "gpt-6-sol", Effort: "medium"}},
		"the default effort":    {func(m *agent.Metrics) {}, agent.Expect{}}, // gpt-6.1-sol's default is low, the session ran medium
		"approval":              {func(m *agent.Metrics) { m.ApprovalPolicy = "on-request" }, agent.Expect{Effort: "medium"}},
		"profile":               {func(m *agent.Metrics) { m.PermissionProfile = "" }, agent.Expect{Effort: "medium"}},
		"network":               {func(m *agent.Metrics) { m.NetworkAccess = true }, agent.Expect{Effort: "medium"}},
		"an MCP call":           {func(m *agent.Metrics) { m.MCPTools = 1 }, agent.Expect{Effort: "medium"}},
		"another CLI":           {func(m *agent.Metrics) {}, agent.Expect{Effort: "medium", CLIVersion: "0.161.0"}},
		"a calibrated model":    {func(m *agent.Metrics) {}, agent.Expect{Effort: "medium", Model: "gpt-6-sol"}},
		"a missing rollout":     {func(m *agent.Metrics) { m.Rollouts = 0 }, agent.Expect{Effort: "medium"}},
		"a session never begun": {func(m *agent.Metrics) { m.SawInit = false }, agent.Expect{Effort: "medium"}},
	} {
		m := base
		c.change(&m)
		if drift := Check(m, c.expect); len(drift) == 0 || Classify(m, agent.StopNone, drift) != agent.OutcomeUnfair {
			t.Errorf("%s: drift %q", name, drift)
		}
	}
	// No rollout at all: the stream's totals are kept, unpriced, and the run is unfair.
	m = parse(t, records(t, "ok", ""))
	if m.Rollouts != 0 || m.CostUSD != 0 || m.UnpricedRequests != 1 || m.InputTokens != 44416-21248 || len(Check(m, agent.Expect{})) == 0 {
		t.Errorf("no rollout: %+v", m)
	}
}

// A subagent's rollout (Gather puts it in Subagents) adds its requests to the run's spend and tokens, not its turns.
func TestParseSubagents(t *testing.T) {
	dir := records(t, "ok", "ok")
	sub, err := os.ReadFile(filepath.Join("testdata", "rollout-interrupted.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, Subagents), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, Subagents, "rollout-sub.jsonl"), sub, 0o600); err != nil {
		t.Fatal(err)
	}
	m := parse(t, dir)
	if want := cost(44416, 21248, 316) + cost(54875, 43008, 396); math.Abs(m.CostUSD-want) > 1e-12 || m.Turns != 4 || m.Rollouts != 2 {
		t.Errorf("cost $%.7f (want $%.7f), turns %d, rollouts %d", m.CostUSD, want, m.Turns, m.Rollouts)
	}
}

// A failed turn is infrastructure when its error never reached the task (a usage limit, the sign-in, the network, the
// API), and the agent's own failure otherwise (the context window overflowing, a policy refusal): a fair attempt,
// graded. The spike's network failure is infra.
func TestFailedTurnsByTheirError(t *testing.T) {
	for message, want := range map[string]string{
		"You've hit your usage limit. Try again later.":                                 agent.OutcomeInfra,
		"unexpected status 401 Unauthorized: Missing bearer authentication":             agent.OutcomeInfra,
		"Your access token could not be refreshed. Please log in again.":                agent.OutcomeInfra,
		"Rate limit reached for requests":                                               agent.OutcomeInfra,
		"stream disconnected before completion: Connection refused (os error 61)":       agent.OutcomeInfra,
		"unexpected status 503 Service Unavailable":                                     agent.OutcomeInfra,
		"Your input exceeds the context window of this model. Please adjust your input": agent.OutcomeOK,
		"This request was refused under the usage policies.":                            agent.OutcomeOK,
		"something else went wrong":                                                     agent.OutcomeOK,
		"unexpected status 502 Bad Gateway":                                             agent.OutcomeInfra,
		"HTTP/1.1 500 from the API":                                                     agent.OutcomeInfra,
		"the test on line 512 failed and the context window filled":                     agent.OutcomeOK,
		"Ran 500 tests before the context window filled":                                agent.OutcomeOK,
		"exit status 503 from the project's own test server":                            agent.OutcomeInfra,
	} {
		line := `{"type":"turn.failed","error":{"message":` + strconv.Quote(message) + `}}`
		dir := t.TempDir()
		data := `{"type":"thread.started","thread_id":"00000000-0000-7000-8000-000000000001"}` + "\n" + line + "\n"
		if err := os.WriteFile(filepath.Join(dir, agent.Transcript), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		m := parse(t, dir)
		if got := Classify(m, agent.StopNone, nil); m.Result != ResultFailed || got != want {
			t.Errorf("%q: %s (result %q), want %s", message, got, m.Result, want)
		}
		if got := Classify(m, agent.StopCap, nil); got != agent.OutcomeCapped {
			t.Errorf("%q stopped at the cap: %s", message, got)
		}
	}
	failed := parse(t, records(t, "network-failed", ""))
	if got := Classify(failed, agent.StopNone, nil); got != agent.OutcomeInfra {
		t.Errorf("the spike's network failure: %s", got)
	}
}

// The stream's ended turns against the rollouts, when the run is read: rollouts that fall short of the turns' usage
// missed requests, so the run is incomplete and costs the larger of the two; the watcher's mark of lost accounting
// makes it incomplete too, whatever the rollouts say.
func TestParseChecksTheTurnsAgainstTheRollouts(t *testing.T) {
	dir := records(t, "ok", "")
	full, err := os.ReadFile(filepath.Join("testdata", "rollout-ok.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	requests := 0
	for _, line := range strings.Split(strings.TrimSpace(string(full)), "\n") {
		if strings.Contains(line, `"type":"token_usage_record"`) {
			if requests++; requests > 2 {
				continue // the last two requests never reached the rollout
			}
		}
		kept = append(kept, line)
	}
	if err := os.WriteFile(filepath.Join(dir, Rollout), []byte(strings.Join(kept, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := parse(t, dir)
	if want := cost(44416, 21248, 316); !m.RolloutsIncomplete || math.Abs(m.CostUSD-want) > 1e-12 {
		t.Errorf("rollouts short of the turn: incomplete %v, $%.6f (want the turn's $%.6f)", m.RolloutsIncomplete, m.CostUSD, want)
	}
	whole := records(t, "ok", "ok")
	if m := parse(t, whole); m.RolloutsIncomplete {
		t.Error("rollouts that match the turn were taken as incomplete")
	}
	if err := os.WriteFile(filepath.Join(whole, AccountingLost), []byte("lost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if m := parse(t, whole); !m.RolloutsIncomplete {
		t.Error("the watcher's mark of lost accounting was ignored")
	}
}

// When the stream's usage is known, the cost is the larger of the rollouts' and the stream's, even within the 1%
// that decides incompleteness. Without it, the main rollout must close its turn: a rollout cut before its
// turn_aborted (a stopped run whose last requests never reached it) is incomplete.
func TestParseTakesTheLargerCostAndNeedsAClosedTurn(t *testing.T) {
	dir := records(t, "ok", "ok")
	stream, err := os.ReadFile(filepath.Join(dir, agent.Transcript))
	if err != nil {
		t.Fatal(err)
	}
	slightly := strings.Replace(string(stream), `"input_tokens":44416`, `"input_tokens":44600`, 1) // 0.4% more than the rollout
	if err := os.WriteFile(filepath.Join(dir, agent.Transcript), []byte(slightly), 0o600); err != nil {
		t.Fatal(err)
	}
	m := parse(t, dir)
	if want := cost(44600, 21248, 316); m.RolloutsIncomplete || math.Abs(m.CostUSD-want) > 1e-12 {
		t.Errorf("a stream 0.4%% above the rollout: incomplete %v, $%.7f (want the stream's $%.7f)", m.RolloutsIncomplete, m.CostUSD, want)
	}

	cut := records(t, "interrupted", "")
	full, err := os.ReadFile(filepath.Join("testdata", "rollout-interrupted.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(strings.TrimSpace(string(full)), "\n") {
		if !strings.Contains(line, `"turn_aborted"`) {
			kept = append(kept, line)
		}
	}
	if err := os.WriteFile(filepath.Join(cut, Rollout), []byte(strings.Join(kept, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if m := parse(t, cut); !m.RolloutsIncomplete {
		t.Error("a stopped run's rollout that never closed its turn was taken as whole")
	}
	if m := parse(t, records(t, "interrupted", "interrupted")); m.RolloutsIncomplete {
		t.Error("a stopped run's rollout that closed its turn (turn_aborted) was taken as incomplete")
	}
}

// A rollout that lost request records but went on to close its turn (persisting a record is never retried): its last
// token_count's running total (total_token_usage) is more than its records add up to, so it is incomplete, and its
// cost is that total priced whole. Kept whole, the same rollout is complete.
func TestParseComparesTheRunningTotal(t *testing.T) {
	full, err := os.ReadFile(filepath.Join("testdata", "rollout-interrupted.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	dropped := 0
	for _, line := range strings.Split(strings.TrimSpace(string(full)), "\n") {
		if strings.Contains(line, `"token_usage_record"`) && dropped < 2 {
			dropped++
			continue
		}
		kept = append(kept, line)
	}
	if dropped != 2 || !strings.Contains(strings.Join(kept, "\n"), `"turn_aborted"`) {
		t.Fatalf("the fixture changed: %d records dropped", dropped)
	}
	dir := records(t, "interrupted", "")
	if err := os.WriteFile(filepath.Join(dir, Rollout), []byte(strings.Join(kept, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := parse(t, dir)
	if want := cost(54875, 43008, 396); !m.RolloutsIncomplete || math.Abs(m.CostUSD-want) > 1e-12 {
		t.Errorf("a rollout missing 2 records: incomplete %v, $%.7f (want incomplete, the running total's $%.7f)", m.RolloutsIncomplete, m.CostUSD, want)
	}
	if m := parse(t, records(t, "interrupted", "interrupted")); m.RolloutsIncomplete || math.Abs(m.CostUSD-cost(54875, 43008, 396)) > 1e-12 {
		t.Errorf("the whole rollout: incomplete %v, $%.7f", m.RolloutsIncomplete, m.CostUSD)
	}
}
