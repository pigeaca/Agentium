// Package sandbox holds Agentium's macOS sandbox policy, shared by agent runs (internal/agent's adapters, such as
// internal/claude, whose sandbox Claude Code builds from settings) and grading (a seatbelt profile of Agentium's own,
// run through sandbox-exec): how a path is denied in every form the sandbox matches (Forms), and which credential
// stores every sandbox denies (CredentialPaths, MovedCredentials); the deny list every agent run shares (AgentDenied,
// with the shared temp and log folders: SharedTempDirs, SharedLogDirs) and the environment allowlist (Environ,
// EnvironFor); and the grader's deny-default profile (Profile), its file, the sandbox-exec wrapper for a runner.Spec
// (Wrap), and the canary that proves the sandbox holds before a grade (Canary).
package sandbox
