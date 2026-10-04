package codex

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/pricing"
)

// Rollout is the main session's rollout in a run's records (Gather moves it there), and Subagents the folder of the
// other sessions' (subagents Codex started in the same folder).
const (
	Rollout   = "rollout.jsonl"
	Subagents = "rollouts"
)

// maxLine bounds one line of a stream or rollout: a rollout's first line holds Codex's whole base instructions.
const maxLine = 64 << 20

// Parse reads what a run reported from its records: the exec stream (stream.jsonl: the session's start, commands,
// changed files, the final message and how the turn ended, reroutes), and the rollouts Gather moved there (the
// session's CLI version, model, effort, sandbox and profile; every request's tokens, priced here; the usage windows;
// the denials, which the stream never shows). A missing rollout leaves those empty, which Check reports. Partial
// metrics come back with an error.
func (Adapter) Parse(records string) (agent.Metrics, error) {
	var m agent.Metrics
	turn, err := parseStream(filepath.Join(records, agent.Transcript), &m)
	if err != nil {
		return m, err
	}
	if text, err := os.ReadFile(filepath.Join(records, LastMessage)); err == nil && strings.TrimSpace(string(text)) != "" {
		m.ResultExcerpt = excerpt(strings.TrimSpace(string(text)), 300)
	}
	files := []string{filepath.Join(records, Rollout)}
	if extra, err := filepath.Glob(filepath.Join(records, Subagents, "*.jsonl")); err == nil {
		sort.Strings(extra)
		files = append(files, extra...)
	}
	var s spend
	if _, err := os.Stat(filepath.Join(records, Incomplete)); err == nil {
		m.RolloutsIncomplete = true // Gather left a rollout behind
	}
	for i, file := range files {
		err := parseRollout(file, i == 0, &m, &s)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			// What was read is kept, and marked as a part: the run's cost is estimated from it (never less).
			m.CostUSD, m.UnpricedRequests, m.RolloutsIncomplete = s.usd, s.unpriced, true
			return m, err
		}
		m.Rollouts++
	}
	if m.Rollouts == 0 && turn != nil {
		// No rollout (Check reports it): the stream's whole-turn counts are all there is. They are not priced: the
		// session's model and the requests' sizes are unknown.
		m.InputTokens, m.CacheReadTokens, m.CacheWriteTokens = turn.Uncached(), turn.Cached, turn.CacheWrite
		m.OutputTokens, m.ReasoningTokens = turn.Output, turn.Reasoning
		m.UnpricedRequests = 1
		return m, nil
	}
	m.CostUSD, m.UnpricedRequests = s.usd, s.unpriced
	return m, nil
}

// streamEvent is one line of `codex exec --json`.
type streamEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
	Error    *struct {
		Message string `json:"message"`
	} `json:"error"`
	Usage *struct {
		Input      int64 `json:"input_tokens"`
		Cached     int64 `json:"cached_input_tokens"`
		CacheWrite int64 `json:"cache_write_input_tokens"`
		Output     int64 `json:"output_tokens"`
		Reasoning  int64 `json:"reasoning_output_tokens"`
	} `json:"usage"`
	Item *struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Message string `json:"message"`
		Command string `json:"command"`
		Status  string `json:"status"`
		Changes []struct {
			Path string `json:"path"`
		} `json:"changes"`
	} `json:"item"`
}

// Result values (agent.Metrics.Result) for Codex: how the stream's turn ended.
const (
	ResultCompleted = "turn.completed"
	ResultFailed    = "turn.failed"
	// ResultNetwork: the stream ends on a network error (Codex reconnecting, or waiting for the network) without a final
	// event; Agentium stopped it or it died.
	ResultNetwork = "network"
)

// networkText matches the stream's errors when Codex could not reach the API: it then retries (or, with unbounded
// retries, waits for the network for ever), which is never the agent's doing.
var networkText = regexp.MustCompile(`(?i)reconnecting|waiting for network|connection failed|stream disconnected|error sending request`)

// rerouted is the stream's item when Codex moves the session to another model ("model rerouted: A -> B (REASON)"):
// the rollout does not keep it, so only the stream tells.
var rerouted = regexp.MustCompile(`^model rerouted: `)

// parseStream reads the exec stream into m, and returns the turn's token counts when it completed (turn.completed).
func parseStream(file string, m *agent.Metrics) (turn *pricing.OpenAIUsage, err error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("run transcript: %w", err)
	}
	defer f.Close()
	m.ToolUses = map[string]int{}
	lastMessage, lastError := "", ""
	waiting := false // the last event was a network error
	err = eachLine(f, func(line []byte) {
		var e streamEvent
		if json.Unmarshal(line, &e) != nil {
			return
		}
		waiting = e.Type == "error" && networkText.MatchString(e.Message)
		switch e.Type {
		case "thread.started":
			m.SawInit = true
		case "turn.completed":
			m.SawResult, m.Result, m.ResultIsError = true, ResultCompleted, false
			if u := e.Usage; u != nil { // the whole turn's: used only when there is no rollout (Parse)
				turn = &pricing.OpenAIUsage{Input: u.Input, Cached: u.Cached, CacheWrite: u.CacheWrite, Output: u.Output, Reasoning: u.Reasoning}
			}
		case "turn.failed":
			m.Result, m.ResultIsError = ResultFailed, true
			if e.Error != nil {
				lastError = e.Error.Message
			}
		case "error":
			lastError = e.Message
			if networkText.MatchString(e.Message) {
				m.APIRetries++
			}
		case "item.completed":
			if e.Item == nil {
				return
			}
			m.ToolUses[e.Item.Type]++
			switch e.Item.Type {
			case "agent_message":
				lastMessage = e.Item.Text
			case "command_execution":
				m.Commands = append(m.Commands, e.Item.Command)
			case "file_change":
				for _, c := range e.Item.Changes {
					m.FilePaths = append(m.FilePaths, c.Path)
				}
			case "mcp_tool_call":
				m.MCPTools++
			case "error":
				if rerouted.MatchString(e.Item.Message) && m.Rerouted == "" {
					m.Rerouted = e.Item.Message
				}
			}
		}
	})
	// A command the sandbox denied never shows in the stream (its denials are the rollout's), so every command listed
	// ran.
	m.RanCommands = slices.Clone(m.Commands)
	if waiting && m.Result == "" {
		m.Result = ResultNetwork
	}
	switch {
	case m.ResultIsError && lastError != "":
		m.ResultExcerpt = excerpt(lastError, 300)
	case lastMessage != "":
		m.ResultExcerpt = excerpt(lastMessage, 300)
	case !m.SawResult && lastError != "":
		m.ResultExcerpt = excerpt(lastError, 300)
	}
	if err != nil {
		return turn, fmt.Errorf("run transcript: %w", err)
	}
	return turn, nil
}

// rolloutLine is one line of a Codex rollout (sessions/.../rollout-*.jsonl). Account fields are never read.
type rolloutLine struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type sessionMeta struct {
	ID         string `json:"id"`
	CWD        string `json:"cwd"`
	CLIVersion string `json:"cli_version"`
	Timestamp  string `json:"timestamp"`
}

type turnContext struct {
	Model          string `json:"model"`
	Effort         string `json:"effort"`
	ApprovalPolicy string `json:"approval_policy"`
	SandboxPolicy  struct {
		Type          string `json:"type"`
		NetworkAccess bool   `json:"network_access"`
	} `json:"sandbox_policy"`
	ActiveProfile struct {
		ID string `json:"id"`
	} `json:"active_permission_profile"`
}

type tokenUsageRecord struct {
	Usage struct {
		Input      int64 `json:"input_tokens"`
		Cached     int64 `json:"cached_input_tokens"`
		CacheWrite int64 `json:"cache_write_input_tokens"`
		Output     int64 `json:"output_tokens"`
		Reasoning  int64 `json:"reasoning_output_tokens"`
	} `json:"usage"`
}

func (r tokenUsageRecord) usage() pricing.OpenAIUsage {
	u := r.Usage
	return pricing.OpenAIUsage{Input: u.Input, Cached: u.Cached, CacheWrite: u.CacheWrite, Output: u.Output, Reasoning: u.Reasoning}
}

// eventMsg is a rollout's event_msg payload: the usage windows (token_count), and how the turn ended.
type eventMsg struct {
	Type       string `json:"type"`
	DurationMS int64  `json:"duration_ms"`
	RateLimits *struct {
		Primary   *limitWindow `json:"primary"`
		Secondary *limitWindow `json:"secondary"`
		Reached   *string      `json:"rate_limit_reached_type"`
	} `json:"rate_limits"`
}

type limitWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int64   `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}

// toolOutput is a tool's result in the rollout (a response_item): code mode's custom_tool_call_output, whose output is
// a list of text parts (one per tool call the script made), or a function_call_output, whose output is one text.
type toolOutput struct {
	Type   string          `json:"type"`
	Output json.RawMessage `json:"output"`
}

// deniedText matches a tool result the sandbox or the approval policy refused: a shell command's "Operation not
// permitted", apply_patch's and view_image's errors, and a patch outside the writable folders.
var deniedText = regexp.MustCompile(`Operation not permitted|patch rejected|rejected by user approval settings|sandbox denied`)

// spend is a run's requests priced so far.
type spend struct {
	usd      float64
	unpriced int
}

// add prices one request at model's rates; a request on a model without a list price, or above the long-context
// limit, counts as unpriced.
func (s *spend) add(u pricing.OpenAIUsage, model string) {
	rates, ok := pricing.OpenAILookup(model)
	if !ok {
		s.unpriced++
		return
	}
	usd, ok := rates.Cost(u)
	if !ok {
		s.unpriced++
		return
	}
	s.usd += usd
}

// parseRollout reads one rollout into m: the main session's (main) gives the session's settings, its usage windows and
// its duration; every rollout adds its requests' tokens and price, and its denials.
func parseRollout(file string, main bool, m *agent.Metrics, s *spend) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	model := ""
	err = eachLine(f, func(line []byte) {
		var l rolloutLine
		if json.Unmarshal(line, &l) != nil {
			return
		}
		switch l.Type {
		case "session_meta":
			var meta sessionMeta
			if main && json.Unmarshal(l.Payload, &meta) == nil {
				m.CLIVersion, m.CWD = meta.CLIVersion, meta.CWD
			}
		case "turn_context":
			var tc turnContext
			if json.Unmarshal(l.Payload, &tc) != nil {
				return
			}
			model = tc.Model
			if main && m.Model == "" {
				m.Model, m.Effort, m.ApprovalPolicy = tc.Model, tc.Effort, tc.ApprovalPolicy
				m.SandboxPolicy, m.NetworkAccess, m.PermissionProfile = tc.SandboxPolicy.Type, tc.SandboxPolicy.NetworkAccess, tc.ActiveProfile.ID
			}
		case "token_usage_record":
			var r tokenUsageRecord
			if json.Unmarshal(l.Payload, &r) != nil {
				return
			}
			u := r.usage()
			if main {
				if m.Turns == 0 {
					m.FirstRequest = u.Input
				}
				m.Turns++ // Codex's turn is the whole run: Turns counts the main session's requests
			}
			m.InputTokens += u.Uncached()
			m.CacheReadTokens += u.Cached
			m.CacheWriteTokens += u.CacheWrite
			m.OutputTokens += u.Output
			m.ReasoningTokens += u.Reasoning
			// Priced at the model this rollout's session runs (a subagent's own), else the main session's.
			s.add(u, cmp.Or(model, m.Model))
		case "event_msg":
			var e eventMsg
			if json.Unmarshal(l.Payload, &e) != nil {
				return
			}
			switch e.Type {
			case "token_count":
				if main && e.RateLimits != nil {
					if reading, ok := usageReading(e.RateLimits.Primary, e.RateLimits.Secondary, e.RateLimits.Reached); ok {
						if m.UsageFirst == nil {
							first := reading
							m.UsageFirst = &first
						}
						m.UsageLast = &reading
					}
				}
			case "task_complete", "turn_aborted":
				if main {
					m.DurationMS = e.DurationMS
				}
			}
		case "response_item":
			var out toolOutput
			if json.Unmarshal(l.Payload, &out) != nil || (out.Type != "custom_tool_call_output" && out.Type != "function_call_output") {
				return
			}
			for _, text := range outputTexts(out.Output) {
				if deniedText.MatchString(text) {
					m.Denials++
				}
			}
		}
	})
	if err != nil {
		return fmt.Errorf("Codex rollout %s: %w", filepath.Base(file), err)
	}
	return nil
}

// outputTexts are a tool output's text parts: a list of {type, text} parts, or one string.
func outputTexts(raw json.RawMessage) []string {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		texts = append(texts, p.Text)
	}
	return texts
}

// usageReading is the ChatGPT plan's windows as a usage reading: the five-hour window (primary, 300 minutes) and the
// weekly one (secondary, 10080 minutes), as shares. Windows of other lengths are not read as these. Nothing else of
// the rate limits (the plan's type, credits) is kept.
func usageReading(primary, secondary *limitWindow, reached *string) (agent.UsageReading, bool) {
	if primary == nil || primary.WindowMinutes != 300 {
		return agent.UsageReading{}, false
	}
	r := agent.UsageReading{FiveHour: primary.UsedPercent / 100, FiveHourResets: time.Unix(primary.ResetsAt, 0).UTC(), Status: "allowed"}
	if secondary != nil && secondary.WindowMinutes == 10080 {
		r.SevenDay, r.SevenDayResets = secondary.UsedPercent/100, time.Unix(secondary.ResetsAt, 0).UTC()
	}
	if reached != nil && *reached != "" {
		r.Status = "rejected"
	}
	return r, true
}

// eachLine calls fn with each line of r (without its newline), up to maxLine bytes; a longer line is an error.
func eachLine(r io.Reader, fn func(line []byte)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxLine)
	for scanner.Scan() {
		fn(scanner.Bytes())
	}
	return scanner.Err()
}

func excerpt(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && (text[cut]&0xC0) == 0x80 { // do not split a UTF-8 character
		cut--
	}
	return text[:cut]
}

// CommandResult is one shell command the stream shows Codex ran, with its output.
type CommandResult struct {
	Command  string
	Output   string
	ExitCode *int
}

// Commands lists the shell commands in a run's exec stream (transcript), in order, with their outputs. Commands the
// sandbox denied are not among them: the stream never shows those.
func Commands(transcript string) ([]CommandResult, error) {
	f, err := os.Open(transcript)
	if err != nil {
		return nil, fmt.Errorf("run transcript: %w", err)
	}
	defer f.Close()
	var out []CommandResult
	err = eachLine(f, func(line []byte) {
		var e struct {
			Type string `json:"type"`
			Item *struct {
				Type     string `json:"type"`
				Command  string `json:"command"`
				Output   string `json:"aggregated_output"`
				ExitCode *int   `json:"exit_code"`
			} `json:"item"`
		}
		if json.Unmarshal(line, &e) == nil && e.Type == "item.completed" && e.Item != nil && e.Item.Type == "command_execution" {
			out = append(out, CommandResult{Command: e.Item.Command, Output: e.Item.Output, ExitCode: e.Item.ExitCode})
		}
	})
	if err != nil {
		return out, fmt.Errorf("run transcript: %w", err)
	}
	return out, nil
}
