package codex

import (
	"fmt"

	"github.com/pigeaca/agentium/internal/agent"
)

// Classify decides a Codex run's outcome:
//   - unfair, on any drift (Check);
//   - infra, when the stream ends while Codex retries the API or waits for the network (only a stop ends that), or when
//     Agentium could not watch its spend (agent.StopBlind);
//   - capped at Agentium's cost cap, timeout at Agentium's timeout;
//   - infra when the turn never completed: it failed (a usage limit, the sign-in, the network, the API) or Codex
//     ended without a final event (stopped by something other than Agentium);
//   - ok otherwise.
//
// An interrupted run (Ctrl-C, a dead Agentium process) is cancelled, which the run decides, not the agent.
func Classify(m agent.Metrics, stop agent.Stop, drift []string) string {
	switch {
	case len(drift) > 0:
		return agent.OutcomeUnfair
	case stop == agent.StopBlind:
		return agent.OutcomeInfra
	case m.Result == ResultNetwork:
		return agent.OutcomeInfra
	case stop == agent.StopCap:
		return agent.OutcomeCapped
	case stop == agent.StopTimeout:
		return agent.OutcomeTimeout
	case !m.SawResult:
		return agent.OutcomeInfra
	}
	return agent.OutcomeOK
}

// Classify is Classify.
func (Adapter) Classify(m agent.Metrics, stop agent.Stop, drift []string) string {
	return Classify(m, stop, drift)
}

// Check lists how a Codex run's environment drifted from what it was given: the session's model, effort, approval
// policy, permission profile and network (from its rollout), a reroute (from the stream), an MCP tool, and, against
// expect, the CLI's version and a calibrated model. A run whose session was never recorded (no rollout) is unfair: its
// environment and its spend are unknown. Codex's skill set is not checked yet (the plan's step 4).
func Check(m agent.Metrics, expect agent.Expect) []string {
	if !m.SawInit {
		if m.SawResult {
			return []string{"no thread.started event: the environment is unknown"}
		}
		return nil // nothing ran; Classify reports infra
	}
	if m.Rollouts == 0 {
		if m.SawResult {
			return []string{"Codex's session rollout is missing: its environment and its spend are unknown"}
		}
		return nil // it never got going (a sign-in or network failure): Classify reports infra
	}
	var drift []string
	if expect.CLIVersion != "" && m.CLIVersion != expect.CLIVersion {
		drift = append(drift, fmt.Sprintf("Codex %s, not %s", m.CLIVersion, expect.CLIVersion))
	}
	if expect.RequestedModel != "" && m.Model != expect.RequestedModel {
		drift = append(drift, fmt.Sprintf("model %s, not the %s asked for", m.Model, expect.RequestedModel))
	}
	if expect.Model != "" && m.Model != expect.Model {
		drift = append(drift, fmt.Sprintf("model %s, not %s", m.Model, expect.Model))
	}
	if want, err := Effort(m.Model, expect.Effort); err == nil && m.Effort != want {
		drift = append(drift, fmt.Sprintf("effort %q, not %q", m.Effort, want))
	}
	if m.ApprovalPolicy != "never" {
		drift = append(drift, fmt.Sprintf("approval policy %q, not \"never\"", m.ApprovalPolicy))
	}
	if m.PermissionProfile != Profile {
		drift = append(drift, fmt.Sprintf("permission profile %q, not %q", m.PermissionProfile, Profile))
	}
	if m.NetworkAccess {
		drift = append(drift, "the sandbox allowed network access")
	}
	if m.MCPTools > 0 {
		drift = append(drift, fmt.Sprintf("%d MCP tool call(s)", m.MCPTools))
	}
	if m.Rerouted != "" {
		drift = append(drift, m.Rerouted)
	}
	return drift
}

// Check is Check.
func (Adapter) Check(m agent.Metrics, expect agent.Expect) []string { return Check(m, expect) }
