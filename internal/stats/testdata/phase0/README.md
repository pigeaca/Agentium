# Phase 0 spike data

The 60 runs of the Phase 0 context A/B spike and its committed summary, from git history (`9bae530`,
`spikes/phase0/results/context-ab/`). `runs.jsonl` keeps only the fields the analysis reads, in the original order
(the bootstrap's draws depend on it); `summary.json` is unchanged. The spike's own code regenerates that summary
exactly (checked with Python 3.9.6), and `reproduce_test.go` checks the Go port against it.
