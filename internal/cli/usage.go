package cli

import (
	"context"
	"time"

	"github.com/pigeaca/agentium/internal/experiment"
)

// waitUntil sleeps until a little past until (experiment.ResetMargin), or until ctx is cancelled.
func waitUntil(ctx context.Context, env Env, until time.Time) error {
	d := until.Sub(env.Now()) + experiment.ResetMargin
	if env.Sleep != nil {
		return env.Sleep(ctx, d)
	}
	timer := time.NewTimer(max(d, 0))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
