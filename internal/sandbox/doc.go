// Package sandbox holds Agentium's macOS sandbox policy, shared by agent runs (internal/claude, whose sandbox Claude
// Code builds from settings) and grading (a seatbelt profile of Agentium's own, run through sandbox-exec): how a path
// is denied in every form the sandbox matches (Forms), and which credential stores every sandbox denies
// (CredentialPaths, MovedCredentials).
package sandbox
