package agent

import "time"

// Metrics are what a run reports, as its adapter read them from the transcript (Adapter.Parse). Every agent fills the
// shared fields: the version, model, cost, tokens and duration; the commands, file paths, tool uses and denials; the
// result, the usage readings and the first request's tokens. The fields marked "Claude Code only" are Claude Code's
// (its permission mode, skills, MCP tools, API time and retries, the isolated-run cost's first reads, subagent models):
// they stay in records' JSON as they were before the seam, and another agent leaves them empty. The JSON keys and
// their order are the records' and reports' contract: they must not change.
type Metrics struct {
	CLIVersion     string   `json:"cli_version"`
	Model          string   `json:"model"`
	PermissionMode string   `json:"permission_mode"` // Claude Code only
	Tools          []string `json:"tools"`           // offered to the agent
	Skills         []string `json:"-"`               // names can be personal: compared, never stored
	SlashCommands  []string `json:"-"`               // likewise; Claude Code only
	SkillCount     int      `json:"skills"`          // offered to the agent
	MCPTools       int      `json:"mcp_tools"`       // Claude Code only

	CostUSD          float64        `json:"cost_usd"`
	Turns            int            `json:"turns"`
	DurationMS       int64          `json:"duration_ms"`
	APIDurationMS    int64          `json:"api_ms"` // Claude Code only
	InputTokens      int64          `json:"input_tokens"`
	OutputTokens     int64          `json:"output_tokens"`
	CacheReadTokens  int64          `json:"cache_read_tokens"`
	CacheWriteTokens int64          `json:"cache_write_tokens"`
	FirstRequest     int64          `json:"first_request_tokens"` // context size of the first request: what the model saw at start
	ToolUses         map[string]int `json:"tools_used"`
	APIRetries       int            `json:"api_retries"` // Claude Code only
	// EstimatedCostUSD prices the transcript's requests at list prices, for a run that ended without the agent's own
	// figure (CostUSD). For Claude Code, input and cache counts are exact per request; the stream reports output only in
	// part, so output is estimated from the content's size. UnpricedRequests counts requests on models the price table
	// lacks.
	EstimatedCostUSD float64 `json:"estimated_cost_usd,omitempty"`
	UnpricedRequests int     `json:"unpriced_requests,omitempty"`
	Denials          int     `json:"permission_denials"` // sandbox or permission denials

	Result        string `json:"result_subtype"` // Claude Code: success, error_max_turns, error_max_budget_usd, error_during_execution
	ResultIsError bool   `json:"result_is_error"`
	ResultExcerpt string `json:"result_excerpt"`
	SawInit       bool   `json:"saw_init"`
	SawResult     bool   `json:"saw_result"`

	Commands []string `json:"-"` // shell commands, in order, denied ones included
	// RanCommands are Commands less the calls the agent reported as denied, which never ran: "ran tests" and "ran the
	// checks" come from them. Claude Code: less the result event's permission_denials (its permission checks and the
	// sandbox's refusals); a transcript without a result event (a run killed at its timeout) lists no denials, so all of
	// Commands.
	RanCommands []string `json:"-"`
	FilePaths   []string `json:"-"` // paths the file tools touched
	ReadPaths   []string `json:"-"` // paths the file tools read: what the agent looked at, not what it wrote
	// CWD is the folder the agent started in, as it reported it: file tools name paths under it. A local path.
	CWD string `json:"-"`
	// SkillCalls are the skills the agent invoked, in order. Names can be personal: compared, never stored.
	SkillCalls []string `json:"-"`
	// SubagentTypes are the subagent types the agent started (Claude Code: through the Agent or Task tool), sorted,
	// each once.
	SubagentTypes []string `json:"-"`

	// UsageFirst and UsageLast are the first and last of the subscription's usage readings in the run (none with an
	// API key): experiments pause before the five-hour limit, and estimate a run's share of the window from them.
	UsageFirst *UsageReading `json:"usage_first,omitempty"`
	UsageLast  *UsageReading `json:"usage_last,omitempty"`
	// SubagentModels maps each subagent type the run used to the models its requests ran on. A role that names a
	// model by alias (model: sonnet) follows Claude Code to newer models, which the run's pinned --model does not
	// cover: an experiment compares these across its runs. Claude Code only.
	SubagentModels map[string][]string `json:"subagent_models,omitempty"`
	// FirstReads are the cache reads the isolated-run cost reprices: the first real request of the main session (first
	// in the list), then of each subagent launch that could not read its type's prefix from this run, in the order
	// their first requests arrived. Nil when the transcript shows no real main-session request (and in records made
	// before the field existed). Holds no subagent type names. Claude Code only.
	FirstReads []FirstRead `json:"first_reads,omitempty"`
	// UnmatchedLaunches counts subagent launches whose requests name a parent tool call (parent_tool_use_id) that no
	// Agent (Task) call in the transcript made: their type is unknown, so whether they are first launches is too.
	// Claude Code only.
	UnmatchedLaunches int `json:"unmatched_launches,omitempty"`

	// The fields below are another agent's (Codex's), absent from Claude Code's records. Codex reports no cost: CostUSD
	// is its requests priced by Agentium (Record.CostSource), with the tokens beside it.
	//
	// Effort, ApprovalPolicy, SandboxPolicy, PermissionProfile and NetworkAccess are the session's, as it recorded them
	// (Codex: its rollout's turn_context), for the drift check; they never hold an account, user or organization ID.
	Effort            string `json:"effort,omitempty"`
	ApprovalPolicy    string `json:"approval_policy,omitempty"`
	SandboxPolicy     string `json:"sandbox_policy,omitempty"`
	PermissionProfile string `json:"permission_profile,omitempty"`
	NetworkAccess     bool   `json:"network_access,omitempty"`
	// ReasoningTokens are the output tokens spent reasoning: part of OutputTokens, and priced as output.
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
	// Rerouted is the agent's report that the session's model was switched mid-run (Codex: "model rerouted: A -> B");
	// such a run is unfair.
	Rerouted string `json:"rerouted,omitempty"`
	// Rollouts counts the session files the metrics were read from (Codex: the main session's rollout and any
	// subagent's); zero when the agent keeps none.
	Rollouts int `json:"rollouts,omitempty"`
}

// FirstRead is the first real request of the main session or of a repriced subagent launch, for the isolated-run
// cost: how many tokens it read from the prompt cache, and at which time to live the launch wrote the cache.
type FirstRead struct {
	Main      bool   `json:"main,omitempty"`  // the main session's; otherwise a subagent launch's
	Model     string `json:"model,omitempty"` // the request's model; empty when the stream did not name it
	CacheRead int64  `json:"cache_read"`      // cache_read_input_tokens
	// WriteTTL is the cache-write time to live of the launch's first request that wrote the cache, from the stream's
	// cache_creation split: TTL5m when it wrote only five-minute entries, TTL1h when it wrote any one-hour entry. When
	// no request of the launch reported a split write, it is what other launches of the type wrote in the run, else
	// TTL5m for a subagent and TTL1h for the main session (what recorded runs wrote), and TTLAssumed is set. Empty
	// only in records made before the fallback.
	WriteTTL   string `json:"write_ttl,omitempty"`
	TTLAssumed bool   `json:"ttl_assumed,omitempty"`
}

// Cache-write times to live, as FirstRead.WriteTTL records them.
const (
	TTL5m = "5m"
	TTL1h = "1h"
)

// UsageReading is a subscription's usage as the agent reports it (Claude Code: rate_limit_event): the share of the
// five-hour and seven-day windows used, when each resets, and the status (allowed, allowed_warning, rejected).
type UsageReading struct {
	FiveHour       float64   `json:"five_hour"`
	FiveHourResets time.Time `json:"five_hour_resets"`
	SevenDay       float64   `json:"seven_day"`
	SevenDayResets time.Time `json:"seven_day_resets"`
	Status         string    `json:"status"`
}

// FiveHourAt is the five-hour window's share used at now: nothing once the window has reset.
func (u UsageReading) FiveHourAt(now time.Time) float64 {
	if u.FiveHourResets.IsZero() || !now.Before(u.FiveHourResets) {
		return 0
	}
	return u.FiveHour
}

// Newer reports whether u is a later reading than v: a later window, or more of the same window used.
func (u UsageReading) Newer(v UsageReading) bool {
	if !u.FiveHourResets.Equal(v.FiveHourResets) {
		return u.FiveHourResets.After(v.FiveHourResets)
	}
	return u.FiveHour > v.FiveHour
}

// Expect is what a fair run's environment looks like (Adapter.Check). Empty fields are not checked.
type Expect struct {
	CLIVersion string
	Model      string
	// RequestedModel and Effort are what the run asked for (the model and effort given; an empty Effort: the agent's
	// default). An agent whose session records them is checked against them (Codex); Claude Code's Check ignores them.
	RequestedModel string
	Effort         string
	Tools          []string // the tool set every run of an experiment must get
	// Skills and SlashCommands are the sets every run of an arm must get, taken from a calibration run under the same
	// isolation (the agent bundles skills and commands of its own, so the resolver cannot list them). Reported by
	// count: names can be personal.
	Skills        []string
	SlashCommands []string
	// PersonalSkills are the names of the user's own skills: none may load. A name is not counted when the arm has a
	// project skill of that name (ProjectSkills) or a calibration run had it (Skills): a bundled skill can share a
	// personal skill's name. Personal commands show up as slash commands, which SlashCommands covers.
	PersonalSkills []string
	ProjectSkills  []string
}
