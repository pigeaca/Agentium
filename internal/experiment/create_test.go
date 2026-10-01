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
