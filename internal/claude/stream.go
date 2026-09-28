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

// Parse reads a stream-json transcript. Lines that are not JSON (a crash message, say) are skipped.
func Parse(r io.Reader) (Metrics, error) {
	m := Metrics{ToolUses: map[string]int{}}
	seen := map[string]bool{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024*1024) // tool results can be large
	for scanner.Scan() {
		var event struct {
			Type, Subtype   string
			ParentToolUseID *string `json:"parent_tool_use_id"`
			// init
			ClaudeCodeVersion string `json:"claude_code_version"`
			Model             string
			PermissionMode    string `json:"permissionMode"`
			Tools             []string
			Skills            []string
			// assistant
			Message *struct {
				Usage   map[string]int64
				Content []json.RawMessage
			}
			// result
			TotalCostUSD      float64 `json:"total_cost_usd"`
			NumTurns          int     `json:"num_turns"`
			DurationMS        int64   `json:"duration_ms"`
			DurationAPIMS     int64   `json:"duration_api_ms"`
			IsError           bool    `json:"is_error"`
			Result            string
			PermissionDenials []json.RawMessage           `json:"permission_denials"`
			ModelUsage        map[string]map[string]int64 `json:"modelUsage"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		switch {
		case event.Type == "system" && event.Subtype == "init":
			m.SawInit = true
			m.CLIVersion, m.Model, m.PermissionMode = event.ClaudeCodeVersion, event.Model, event.PermissionMode
			m.Tools, m.Skills = sorted(event.Tools), sorted(event.Skills)
			m.SkillCount = len(m.Skills)
			m.MCPTools = 0
			for _, tool := range m.Tools {
				if strings.HasPrefix(tool, "mcp__") {
					m.MCPTools++
				}
			}
		case event.Type == "system" && event.Subtype == "api_retry":
			m.APIRetries++
		case event.Type == "assistant" && event.Message != nil:
			if m.FirstRequest == 0 && event.ParentToolUseID == nil {
				u := event.Message.Usage // input and cache counts are exact per request: the context the model saw
				m.FirstRequest = u["input_tokens"] + u["cache_creation_input_tokens"] + u["cache_read_input_tokens"]
			}
			for _, raw := range event.Message.Content {
				var block struct {
					Type, ID, Name string
					Input          map[string]any
				}
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
			m.SawResult = true
			m.Result, m.ResultIsError = event.Subtype, event.IsError
			m.CostUSD, m.Turns, m.DurationMS, m.APIDurationMS = event.TotalCostUSD, event.NumTurns, event.DurationMS, event.DurationAPIMS
			m.Denials = len(event.PermissionDenials)
			m.ResultExcerpt = excerpt(event.Result, 300)
			m.InputTokens, m.OutputTokens, m.CacheReadTokens, m.CacheWriteTokens = 0, 0, 0, 0
			for _, usage := range event.ModelUsage {
				m.InputTokens += usage["inputTokens"]
				m.OutputTokens += usage["outputTokens"]
				m.CacheReadTokens += usage["cacheReadInputTokens"]
				m.CacheWriteTokens += usage["cacheCreationInputTokens"]
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
	// PersonalSkills are the names of the user's own skills and commands: none may load (requirement 1).
	PersonalSkills []string
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
	personal := 0
	for _, skill := range m.Skills {
		if slices.Contains(expect.PersonalSkills, skill) {
			personal++
		}
	}
	if personal > 0 { // requirement 1; the names stay private
		drift = append(drift, fmt.Sprintf("%d personal skill(s) loaded", personal))
	}
	return drift
}

// PersonalSkills lists the names of the user's own skills and commands (~/.claude/skills/*, ~/.claude/commands/*.md).
// Only names are read, to recognize them in a run; they are never stored.
func PersonalSkills(home string) []string {
	var names []string
	if entries, err := os.ReadDir(filepath.Join(home, ".claude", "skills")); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				names = append(names, e.Name())
			}
		}
	}
	commands, _ := filepath.Glob(filepath.Join(home, ".claude", "commands", "*.md"))
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
