package experiment

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Create needs the tier and the seed that Prepare sets; without them it must fail, not store a design with tier 0
// and seed 0.
func TestCreateRefusesUnpreparedOptions(t *testing.T) {
	t.Parallel()
	_, err := Create(context.Background(), Project{}, "x", NewOptions{Template: TemplateAA, ContextA: BaseContext}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "Prepare") {
		t.Fatalf("err = %v, want one naming Prepare", err)
	}
}

func TestPrepareFillsTierAndSeed(t *testing.T) {
	t.Parallel()
	o := NewOptions{Template: TemplateAA, ContextA: BaseContext}
	if err := o.Prepare("x"); err != nil {
		t.Fatal(err)
	}
	if o.Seed == 0 || o.Repeats != Tiers()[0].Repeats || !o.prepared {
		t.Errorf("seed %d, repeats %d, prepared %v", o.Seed, o.Repeats, o.prepared)
	}
}

// --b decides the template: none an A/A, a model (one the price table knows, dated or not, or any claude-… name, with
// or without an effort) a model A/B, and anything else a snapshot's context A/B. Aliases are not models here.
func TestInferTemplate(t *testing.T) {
	t.Parallel()
	for b, want := range map[string]string{
		"":                           TemplateAA,
		"lean":                       TemplateContextAB,
		"trimmed-v2":                 TemplateContextAB,
		"sonnet":                     TemplateContextAB, // an alias Claude Code resolves; Agentium cannot price it
		"opus:high":                  TemplateContextAB, // no snapshot can be named so: the lookup then fails, naming both readings
		"claude-sonnet-5-5":          TemplateModelAB,
		"claude-opus-5-5:high":       TemplateModelAB,
		"claude-haiku-4-5-20251001":  TemplateModelAB,
		"claude-next-9":              TemplateModelAB, // unpriced, but shaped like a model ID
		"claude-opus-6-20270101":     TemplateModelAB,
		"claude-rules":               TemplateContextAB, // a claude-… name that is no model ID: a snapshot, maybe mistyped
		"claude-md-v2":               TemplateContextAB,
		"claude-opus":                TemplateContextAB,
		"claude-sonnet-5-5:huge":     TemplateModelAB, // read as a model; its effort is then refused
		"Claude-sonnet-5-5-snapshot": TemplateContextAB,
	} {
		if got := InferTemplate(b); got != want {
			t.Errorf("InferTemplate(%q) = %s, want %s", b, got, want)
		}
	}
}
