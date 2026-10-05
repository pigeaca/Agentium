package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
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

var fileTools = map[string]bool{"Read": true, "Edit": true, "Write": true, "NotebookEdit": true}

// The stream's events, decoded in parts: each part holds only the fields Agentium reads, so fields Claude Code adds or
// types it changes (service_tier, cache_creation, costUSD, ...) are ignored instead of failing the whole line.
type (
	envelope struct {
		Type            string          `json:"type"`
		Subtype         string          `json:"subtype"`
		Timestamp       string          `json:"timestamp"` // on assistant and user events
		ParentToolUseID *string         `json:"parent_tool_use_id"`
		Message         json.RawMessage `json:"message"`
	}
	initEvent struct {
		ClaudeCodeVersion string   `json:"claude_code_version"`
		Model             string   `json:"model"`
		PermissionMode    string   `json:"permissionMode"`
		Tools             []string `json:"tools"`
		Skills            []string `json:"skills"`
		SlashCommands     []string `json:"slash_commands"`
		CWD               string   `json:"cwd"`
	}
	assistantMessage struct {
		ID      string            `json:"id"`
		Model   string            `json:"model"`
		Usage   json.RawMessage   `json:"usage"`
		Content []json.RawMessage `json:"content"`
	}
	requestUsage struct {
		Input         float64 `json:"input_tokens"`
		CacheCreation float64 `json:"cache_creation_input_tokens"`
		CacheRead     float64 `json:"cache_read_input_tokens"`
		Output        float64 `json:"output_tokens"`
		Split         *struct {
			FiveMinutes float64 `json:"ephemeral_5m_input_tokens"`
			OneHour     float64 `json:"ephemeral_1h_input_tokens"`
		} `json:"cache_creation"`
	}
	toolUse struct {
		Type  string         `json:"type"`
		ID    string         `json:"id"`
		Name  string         `json:"name"`
		Input map[string]any `json:"input"`
	}
	resultEvent struct {
		IsError           bool                       `json:"is_error"`
		Result            string                     `json:"result"`
		TotalCostUSD      float64                    `json:"total_cost_usd"`
		NumTurns          float64                    `json:"num_turns"`
		DurationMS        float64                    `json:"duration_ms"`
		DurationAPIMS     float64                    `json:"duration_api_ms"`
		PermissionDenials []json.RawMessage          `json:"permission_denials"`
		ModelUsage        map[string]json.RawMessage `json:"modelUsage"`
	}
	rateLimitEvent struct {
		Info struct {
			Status  string `json:"status"`
			Windows struct {
				FiveHour *usageWindow `json:"five_hour"`
				SevenDay *usageWindow `json:"seven_day"`
			} `json:"unifiedWindows"`
		} `json:"rate_limit_info"`
	}
	usageWindow struct {
		Utilization float64 `json:"utilization"`
		ResetsAt    int64   `json:"resetsAt"` // Unix seconds
	}
	modelUsage struct {
		Input         float64 `json:"inputTokens"`
		Output        float64 `json:"outputTokens"`
		CacheRead     float64 `json:"cacheReadInputTokens"`
		CacheCreation float64 `json:"cacheCreationInputTokens"`
	}
)

// Parse reads a stream-json transcript into what the run reports. Totals come from the result event, which includes
// subagents. Lines that are not JSON (a crash message, say) are skipped, and so are parts
// of an event that do not decode.
func Parse(r io.Reader) (agent.Metrics, error) {
	m := agent.Metrics{ToolUses: map[string]int{}}
	seen := map[string]bool{}
	firstSeen := false
	requests := map[string]*request{} // by message ID: a message's content blocks arrive as separate events
	launches := newLaunchLog()        // the isolated-run cost's first reads
	lineNo := 0
	subagentTypes := map[string]string{} // Agent (Task) tool calls by ID: the subagent type their messages run as
	var commandIDs []string              // the tool call ID of each of m.Commands
	denied := map[string]bool{}          // tool call IDs in the result's permission_denials
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024*1024) // tool results can be large
	for scanner.Scan() {
		line := scanner.Bytes()
		lineNo++
		var event envelope
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		switch {
		case event.Type == "system" && event.Subtype == "init":
			var init initEvent
			if json.Unmarshal(line, &init) != nil {
				continue
			}
			m.SawInit = true
			m.CLIVersion, m.Model, m.PermissionMode = init.ClaudeCodeVersion, init.Model, init.PermissionMode
			m.Tools, m.Skills, m.SlashCommands = sorted(init.Tools), sorted(init.Skills), sorted(init.SlashCommands)
			m.CWD = init.CWD
			m.SkillCount, m.MCPTools = len(m.Skills), 0
			for _, tool := range m.Tools {
				if strings.HasPrefix(tool, "mcp__") {
					m.MCPTools++
				}
			}
		case event.Type == "system" && event.Subtype == "api_retry":
			m.APIRetries++
		case event.Type == "rate_limit_event":
			var limit rateLimitEvent
			if json.Unmarshal(line, &limit) != nil || limit.Info.Windows.FiveHour == nil {
				continue
			}
			w := limit.Info.Windows
			reading := agent.UsageReading{FiveHour: w.FiveHour.Utilization, FiveHourResets: unixTime(w.FiveHour.ResetsAt), Status: limit.Info.Status}
			if w.SevenDay != nil {
				reading.SevenDay, reading.SevenDayResets = w.SevenDay.Utilization, unixTime(w.SevenDay.ResetsAt)
			}
			if m.UsageFirst == nil {
				first := reading
				m.UsageFirst = &first
			}
			m.UsageLast = &reading
		case event.Type == "assistant":
			var message assistantMessage
			if json.Unmarshal(event.Message, &message) != nil {
				continue
			}
			var usage requestUsage
			usageOK := json.Unmarshal(message.Usage, &usage) == nil
			if !firstSeen && event.ParentToolUseID == nil && usageOK {
				// Input and cache counts are exact per request: the context the model saw at the start.
				firstSeen = true
				m.FirstRequest = int64(usage.Input + usage.CacheCreation + usage.CacheRead)
			}
			key := message.ID
			if key == "" {
				key = fmt.Sprintf("line-%d", len(requests))
			}
			req := requests[key]
			if req == nil {
				req = &request{model: message.Model}
				requests[key] = req
			}
			at := eventTime(event.Timestamp)
			if usageOK {
				req.add(usage)
			}
			launches.request(event.ParentToolUseID, req, usage, usageOK, message.Model, lineNo, at)
			if event.ParentToolUseID != nil && message.Model != "" { // a subagent's request
				kind := subagentTypes[*event.ParentToolUseID]
				if kind == "" {
					kind = "unknown"
				}
				if m.SubagentModels == nil {
					m.SubagentModels = map[string][]string{}
				}
				if !slices.Contains(m.SubagentModels[kind], message.Model) {
					m.SubagentModels[kind] = sorted(append(m.SubagentModels[kind], message.Model))
				}
			}
			for _, raw := range message.Content {
				req.contentBytes += int64(len(raw))
			}
			for _, raw := range message.Content {
				var block toolUse
				if json.Unmarshal(raw, &block) != nil || block.Type != "tool_use" || seen[block.ID] {
					continue
				}
				seen[block.ID] = true
				m.ToolUses[block.Name]++
				if block.Name == "Agent" || block.Name == "Task" { // Task is the tool's older name
					kind, _ := block.Input["subagent_type"].(string)
					if kind == "" {
						kind = "general-purpose"
					}
					subagentTypes[block.ID] = kind
					launches.call(block.ID, kind, lineNo, at)
					if !slices.Contains(m.SubagentTypes, kind) {
						m.SubagentTypes = sorted(append(m.SubagentTypes, kind))
					}
				}
				switch {
				case block.Name == "Bash":
					if command, ok := block.Input["command"].(string); ok {
						m.Commands, commandIDs = append(m.Commands, command), append(commandIDs, block.ID)
					}
				case block.Name == "Skill":
					if name := skillName(block.Input); name != "" {
						m.SkillCalls = append(m.SkillCalls, name)
					}
				case fileTools[block.Name]:
					if p, ok := block.Input["file_path"].(string); ok {
						m.FilePaths = append(m.FilePaths, p)
						if block.Name == "Read" {
							m.ReadPaths = append(m.ReadPaths, p)
						}
					}
				}
			}
		case event.Type == "result":
			var result resultEvent
			if json.Unmarshal(line, &result) != nil {
				continue
			}
			m.SawResult = true
			m.Result, m.ResultIsError = event.Subtype, result.IsError
			m.CostUSD, m.Turns = result.TotalCostUSD, int(result.NumTurns)
			m.DurationMS, m.APIDurationMS = int64(result.DurationMS), int64(result.DurationAPIMS)
			m.Denials = len(result.PermissionDenials)
			clear(denied)
			for _, raw := range result.PermissionDenials {
				var d struct {
					ToolUseID string `json:"tool_use_id"`
				}
				if json.Unmarshal(raw, &d) == nil && d.ToolUseID != "" {
					denied[d.ToolUseID] = true
				}
			}
			m.ResultExcerpt = excerpt(result.Result, 300)
			m.InputTokens, m.OutputTokens, m.CacheReadTokens, m.CacheWriteTokens = 0, 0, 0, 0
			for _, raw := range result.ModelUsage {
				var usage modelUsage
				if json.Unmarshal(raw, &usage) != nil {
					continue
				}
				m.InputTokens += int64(usage.Input)
				m.OutputTokens += int64(usage.Output)
				m.CacheReadTokens += int64(usage.CacheRead)
				m.CacheWriteTokens += int64(usage.CacheCreation)
			}
		}
	}
	m.FirstReads, m.UnmatchedLaunches = launches.firstReads()
	for i, command := range m.Commands {
		if !denied[commandIDs[i]] {
			m.RanCommands = append(m.RanCommands, command)
		}
	}
	for _, req := range requests {
		model := req.model
		if model == "" {
			model = m.Model
		}
		rates, ok := pricing.Lookup(model)
		if !ok {
			m.UnpricedRequests++
			continue
		}
		m.EstimatedCostUSD += rates.Cost(req.usage())
	}
	if err := scanner.Err(); err != nil {
		return m, fmt.Errorf("read transcript: %w", err)
	}
	return m, nil
}

// skillName is the skill a Skill tool call invokes: its skill input (command in older Claude Code versions), without a
// leading slash.
func skillName(input map[string]any) string {
	for _, key := range []string{"skill", "command"} {
		if name, ok := input[key].(string); ok && strings.TrimSpace(name) != "" {
			return strings.TrimPrefix(strings.TrimSpace(name), "/")
		}
	}
	return ""
}

// eventTime is an event's timestamp; zero when it has none or it does not parse.
func eventTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func unixTime(seconds int64) time.Time {
	if seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0).UTC()
}

// SubagentModelChanges lists the subagent types whose models in a run (now) differ from those earlier runs used
// (seen): a role's model alias that moved to a newer model, say. A type no earlier run used is not a change.
func SubagentModelChanges(seen, now map[string][]string) []string {
	var changes []string
	for _, kind := range sortedKeys(now) {
		if before, ok := seen[kind]; ok && !slices.Equal(before, now[kind]) {
			changes = append(changes, fmt.Sprintf("subagent %s ran on %s; earlier runs used %s", kind, strings.Join(now[kind], ", "), strings.Join(before, ", ")))
		}
	}
	return changes
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// request is one model request, as far as the stream shows it.
type request struct {
	model                                 string
	input, write5m, write1h, read, output int64
	split                                 bool // the stream reported the cache write's time-to-live split
	contentBytes                          int64
}

// add keeps the largest count seen for each field: every event of a message repeats its usage, output growing.
func (r *request) add(u requestUsage) {
	write5m, write1h := int64(0), int64(u.CacheCreation) // without the split, the one-hour rate Claude Code uses
	if u.Split != nil {
		write5m, write1h = int64(u.Split.FiveMinutes), int64(u.Split.OneHour)
		r.split = true
	}
	r.input, r.read = max(r.input, int64(u.Input)), max(r.read, int64(u.CacheRead))
	r.write5m, r.write1h, r.output = max(r.write5m, write5m), max(r.write1h, write1h), max(r.output, int64(u.Output))
}

// usage is the request's tokens, with output estimated at four bytes of content per token when the stream shows less.
func (r *request) usage() pricing.Usage {
	return pricing.Usage{Input: r.input, CacheWrite5m: r.write5m, CacheWrite1h: r.write1h, CacheRead: r.read,
		Output: max(r.output, r.contentBytes/4)}
}

// infraText matches results of runs that never reached the task. Agent outcomes never match.
var infraText = regexp.MustCompile(`(?i)usage limit|rate limit|overloaded|authenticat|not logged in|oauth|credit balance|ECONN|socket|\b5\d\d \w`)

// Classify decides a run's outcome from its metrics, whether Agentium stopped it, and its environment drift.
func Classify(m agent.Metrics, timedOut bool, drift []string) string {
	switch {
	case len(drift) > 0:
		return agent.OutcomeUnfair
	case timedOut:
		return agent.OutcomeTimeout
	case !m.SawResult:
		return agent.OutcomeInfra
	case m.Result == "error_max_turns" || m.Result == "error_max_budget_usd":
		return agent.OutcomeCapped
	case m.Result == "error_during_execution" || (m.ResultIsError && infraText.MatchString(m.ResultExcerpt)):
		return agent.OutcomeInfra
	}
	return agent.OutcomeOK
}

// Check lists how a run's environment drifted from what was expected. Any drift makes a run unfair.
func Check(m agent.Metrics, expect agent.Expect) []string {
	if !m.SawInit {
		if m.SawResult {
			return []string{"no init event: the environment is unknown"}
		}
		return nil // nothing ran; Classify reports infra
	}
	var drift []string
	if m.PermissionMode != PermissionMode { // requirement 3
		drift = append(drift, fmt.Sprintf("permission mode %q, not %q", m.PermissionMode, PermissionMode))
	}
	if m.MCPTools > 0 { // requirement 2
		drift = append(drift, fmt.Sprintf("%d MCP or connector tool(s) attached", m.MCPTools))
	}
	if expect.CLIVersion != "" && m.CLIVersion != expect.CLIVersion {
		drift = append(drift, fmt.Sprintf("Claude Code %s, not %s", m.CLIVersion, expect.CLIVersion))
	}
	if expect.Model != "" && m.Model != expect.Model {
		drift = append(drift, fmt.Sprintf("model %s, not %s", m.Model, expect.Model))
	}
	if expect.Tools != nil && !slices.Equal(m.Tools, sorted(expect.Tools)) {
		added, removed := difference(m.Tools, expect.Tools), difference(expect.Tools, m.Tools)
		drift = append(drift, fmt.Sprintf("tools differ (added %s; missing %s)", orNone(added), orNone(removed)))
	}
	if expect.Skills != nil && !slices.Equal(m.Skills, sorted(expect.Skills)) {
		drift = append(drift, fmt.Sprintf("skills differ (%d added, %d missing)", len(difference(m.Skills, expect.Skills)), len(difference(expect.Skills, m.Skills))))
	}
	if expect.SlashCommands != nil && !slices.Equal(m.SlashCommands, sorted(expect.SlashCommands)) {
		drift = append(drift, fmt.Sprintf("slash commands differ (%d added, %d missing)",
			len(difference(m.SlashCommands, expect.SlashCommands)), len(difference(expect.SlashCommands, m.SlashCommands))))
	}
	personal := 0
	for _, name := range m.Skills {
		if slices.Contains(expect.PersonalSkills, name) && !slices.Contains(expect.ProjectSkills, name) && !slices.Contains(expect.Skills, name) {
			personal++
		}
	}
	if personal > 0 { // requirement 1; the names stay private
		drift = append(drift, fmt.Sprintf("%d personal skill(s) loaded", personal))
	}
	return drift
}

// PersonalSkills lists the names of the user's own skills and commands in their Claude Code folder (configDir, see
// UserConfigDir): skills/* and commands/*.md. Only names are read, to recognize them in a run; they are never stored.
// Skills from user-level plugins are not listed; a lock's exact skill set (agent.Expect.Skills) catches those.
func PersonalSkills(configDir string) []string {
	var names []string
	if entries, err := os.ReadDir(filepath.Join(configDir, "skills")); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				names = append(names, e.Name())
			}
		}
	}
	commands, _ := filepath.Glob(filepath.Join(configDir, "commands", "*.md"))
	for _, c := range commands {
		names = append(names, strings.TrimSuffix(filepath.Base(c), ".md"))
	}
	return sorted(names)
}

func sorted(list []string) []string {
	out := append([]string{}, list...)
	sort.Strings(out)
	return out
}

func difference(a, b []string) []string {
	var out []string
	for _, x := range a {
		if !slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

func orNone(list []string) string {
	if len(list) == 0 {
		return "none"
	}
	return strings.Join(list, ", ")
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

// ToolCall is one tool use in a transcript with its result.
type ToolCall struct {
	Name    string
	Input   map[string]any
	Result  string // the tool result's text (joined when it has several parts)
	IsError bool
}

// ToolCalls lists the main agent's and subagents' tool uses with their results, in order. It is for checks that must
// rest on what tools returned, not on what the agent says.
func ToolCalls(r io.Reader) ([]ToolCall, error) {
	var calls []ToolCall
	index := map[string]int{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		var event envelope
		if json.Unmarshal(scanner.Bytes(), &event) != nil || (event.Type != "assistant" && event.Type != "user") {
			continue
		}
		var message struct {
			Content []json.RawMessage `json:"content"`
		}
		if json.Unmarshal(event.Message, &message) != nil {
			continue
		}
		for _, raw := range message.Content {
			var block struct {
				Type      string          `json:"type"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Input     map[string]any  `json:"input"`
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
				IsError   bool            `json:"is_error"`
			}
			if json.Unmarshal(raw, &block) != nil {
				continue
			}
			switch {
			case block.Type == "tool_use" && event.Type == "assistant":
				if _, seen := index[block.ID]; !seen {
					index[block.ID] = len(calls)
					calls = append(calls, ToolCall{Name: block.Name, Input: block.Input})
				}
			case block.Type == "tool_result" && event.Type == "user":
				if i, ok := index[block.ToolUseID]; ok {
					calls[i].Result, calls[i].IsError = resultText(block.Content), block.IsError
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return calls, fmt.Errorf("read transcript: %w", err)
	}
	return calls, nil
}

// resultText is a tool result's content as text: a string, or the text parts of a list.
func resultText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return string(raw)
}

var sessionUnsafe = regexp.MustCompile(`[^A-Za-z0-9]`)

// SessionFolder is where Claude Code keeps the session of a run started in dir, under configDir: projects/ plus dir's
// real path with every character but letters and digits replaced by "-" (as observed on 2.1.281). Large tool outputs
// are saved there.
func SessionFolder(configDir, dir string) string {
	return filepath.Join(configDir, "projects", sessionName(dir))
}

// sessionName is the name SessionFolder gives the session folder of dir: its real path (dir as given when it cannot be
// resolved), encoded.
func sessionName(dir string) string {
	real := dir
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		real = resolved
	}
	return sessionUnsafe.ReplaceAllString(real, "-")
}

// SessionFolderUnder recognises a session folder's name (a name in projects/, as SessionFolder makes it) of a session
// started below root, resolved as SessionFolder resolves dir: rest is what follows root's encoded path and "-". For
// SessionFolder(c, root+"/a/b.c") it returns "a-b-c". A name never leads back to one path: every path that differs from
// root only in characters other than letters and digits (/x/a.b and /x/a_b, or /x-a/b) encodes the same, so a match
// says that the name fits root, not that the session started there.
func SessionFolderUnder(root, name string) (rest string, ok bool) {
	rest, ok = strings.CutPrefix(name, sessionName(root)+"-")
	return rest, ok && sessionNamePart.MatchString(rest)
}

// sessionNamePart is what an encoded path can hold.
var sessionNamePart = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
