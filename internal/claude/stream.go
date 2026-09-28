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
)

// Metrics are what a run reports in its stream-json transcript. Totals come from the result event, which includes
// subagents.
type Metrics struct {
	CLIVersion     string   `json:"cli_version"`
	Model          string   `json:"model"`
	PermissionMode string   `json:"permission_mode"`
	Tools          []string `json:"tools"`  // offered to the agent
	Skills         []string `json:"-"`      // names can be personal: compared, never stored
	SlashCommands  []string `json:"-"`      // likewise
	SkillCount     int      `json:"skills"` // offered to the agent
	MCPTools       int      `json:"mcp_tools"`

	CostUSD          float64        `json:"cost_usd"`
	Turns            int            `json:"turns"`
	DurationMS       int64          `json:"duration_ms"`
	APIDurationMS    int64          `json:"api_ms"`
	InputTokens      int64          `json:"input_tokens"`
	OutputTokens     int64          `json:"output_tokens"`
	CacheReadTokens  int64          `json:"cache_read_tokens"`
	CacheWriteTokens int64          `json:"cache_write_tokens"`
	FirstRequest     int64          `json:"first_request_tokens"` // context size of the first request: what the model saw at start
	ToolUses         map[string]int `json:"tools_used"`
	APIRetries       int            `json:"api_retries"`
	Denials          int            `json:"permission_denials"` // requirement 7: sandbox or permission denials

	Result        string `json:"result_subtype"` // success, error_max_turns, error_max_budget_usd, error_during_execution
	ResultIsError bool   `json:"result_is_error"`
	ResultExcerpt string `json:"result_excerpt"`
	SawInit       bool   `json:"saw_init"`
	SawResult     bool   `json:"saw_result"`

	Commands  []string `json:"-"` // Bash commands, in order: behavior flags come from them
	FilePaths []string `json:"-"` // paths the file tools touched
}

var fileTools = map[string]bool{"Read": true, "Edit": true, "Write": true, "NotebookEdit": true}

// The stream's events, decoded in parts: each part holds only the fields Agentium reads, so fields Claude Code adds or
// types it changes (service_tier, cache_creation, costUSD, ...) are ignored instead of failing the whole line.
type (
	envelope struct {
		Type            string          `json:"type"`
		Subtype         string          `json:"subtype"`
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
	}
	assistantMessage struct {
		Usage   json.RawMessage   `json:"usage"`
		Content []json.RawMessage `json:"content"`
	}
	requestUsage struct {
		Input         float64 `json:"input_tokens"`
		CacheCreation float64 `json:"cache_creation_input_tokens"`
		CacheRead     float64 `json:"cache_read_input_tokens"`
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
	modelUsage struct {
		Input         float64 `json:"inputTokens"`
		Output        float64 `json:"outputTokens"`
		CacheRead     float64 `json:"cacheReadInputTokens"`
		CacheCreation float64 `json:"cacheCreationInputTokens"`
	}
)

// Parse reads a stream-json transcript. Lines that are not JSON (a crash message, say) are skipped, and so are parts
// of an event that do not decode.
func Parse(r io.Reader) (Metrics, error) {
	m := Metrics{ToolUses: map[string]int{}}
	seen := map[string]bool{}
	firstSeen := false
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024*1024) // tool results can be large
	for scanner.Scan() {
		line := scanner.Bytes()
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
			m.SkillCount, m.MCPTools = len(m.Skills), 0
			for _, tool := range m.Tools {
				if strings.HasPrefix(tool, "mcp__") {
					m.MCPTools++
				}
			}
		case event.Type == "system" && event.Subtype == "api_retry":
			m.APIRetries++
		case event.Type == "assistant":
			var message assistantMessage
			if json.Unmarshal(event.Message, &message) != nil {
				continue
			}
			var usage requestUsage
			if !firstSeen && event.ParentToolUseID == nil && json.Unmarshal(message.Usage, &usage) == nil {
				// Input and cache counts are exact per request: the context the model saw at the start.
				firstSeen = true
				m.FirstRequest = int64(usage.Input + usage.CacheCreation + usage.CacheRead)
			}
			for _, raw := range message.Content {
				var block toolUse
				if json.Unmarshal(raw, &block) != nil || block.Type != "tool_use" || seen[block.ID] {
					continue
				}
				seen[block.ID] = true
				m.ToolUses[block.Name]++
				switch {
				case block.Name == "Bash":
					if command, ok := block.Input["command"].(string); ok {
						m.Commands = append(m.Commands, command)
					}
				case fileTools[block.Name]:
					if p, ok := block.Input["file_path"].(string); ok {
						m.FilePaths = append(m.FilePaths, p)
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
	if err := scanner.Err(); err != nil {
		return m, fmt.Errorf("read transcript: %w", err)
	}
	return m, nil
}

// Outcomes. ok, capped and timeout are the agent's; infra and unfair runs never had a fair attempt and are not counted.
const (
	OutcomeOK      = "ok"
	OutcomeCapped  = "capped"  // hit the turn or budget cap
	OutcomeTimeout = "timeout" // Agentium stopped it
	OutcomeInfra   = "infra"   // no result, a crash, sign-in, limits, overload or transport
	OutcomeUnfair  = "unfair"  // the environment drifted (see Check)
)

// infraText matches results of runs that never reached the task. Agent outcomes never match.
var infraText = regexp.MustCompile(`(?i)usage limit|rate limit|overloaded|authenticat|not logged in|oauth|credit balance|ECONN|socket|\b5\d\d \w`)

// Classify decides a run's outcome from its metrics, whether Agentium stopped it, and its environment drift.
func Classify(m Metrics, timedOut bool, drift []string) string {
	switch {
	case len(drift) > 0:
		return OutcomeUnfair
	case timedOut:
		return OutcomeTimeout
	case !m.SawResult:
		return OutcomeInfra
	case m.Result == "error_max_turns" || m.Result == "error_max_budget_usd":
		return OutcomeCapped
	case m.Result == "error_during_execution" || (m.ResultIsError && infraText.MatchString(m.ResultExcerpt)):
		return OutcomeInfra
	}
	return OutcomeOK
}

// Expect is what a fair run's environment looks like. Empty fields are not checked.
type Expect struct {
	CLIVersion string
	Model      string
	Tools      []string // the tool set every run of an experiment must get
	// Skills and SlashCommands are the sets every run of an arm must get, taken from a calibration run under the same
	// isolation (Claude Code bundles skills and commands of its own, so the resolver cannot list them). Reported by
	// count: names can be personal.
	Skills        []string
	SlashCommands []string
	// PersonalSkills are the names of the user's own skills: none may load (requirement 1). A name is not counted when
	// the arm has a project skill of that name (ProjectSkills) or a calibration run had it (Skills): a bundled skill
	// can share a personal skill's name. Personal commands show up as slash commands, which SlashCommands covers.
	PersonalSkills []string
	ProjectSkills  []string
}

// Check lists how a run's environment drifted from what was expected. Any drift makes a run unfair.
func Check(m Metrics, expect Expect) []string {
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
// Skills from user-level plugins are not listed; a lock's exact skill set (Expect.Skills) catches those.
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
	real := dir
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		real = resolved
	}
	return filepath.Join(configDir, "projects", sessionUnsafe.ReplaceAllString(real, "-"))
}
