package experiment

import "time"

// The defaults of a new experiment, shared by `experiment new` (its flags) and `start` (which makes one without asking).
const (
	DefaultExperimentModel = "claude-sonnet-5-5"
	DefaultRunBudgetUSD    = 3.0 // each run stops at this cost
	DefaultConcurrency     = 2   // runs at a time
	DefaultRunTimeout      = 20 * time.Minute
	DefaultVerifyTimeout   = 10 * time.Minute
)
