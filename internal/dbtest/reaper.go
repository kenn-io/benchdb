package dbtest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v7"
	"github.com/containerd/errdefs"
)

const (
	// reaperRaceAttempts caps how often one testcontainers call is tried. The
	// library already retries for 20 seconds inside each attempt.
	reaperRaceAttempts = 3

	// reaperNamePrefix starts the name of every Ryuk container.
	reaperNamePrefix = "/reaper_"
)

// isReaperNameConflict reports whether err is Docker refusing to create the
// shared Ryuk reaper because a container with its name exists.
//
// Test binaries run by one `go test` share a testcontainers session, so they
// share one reaper container named reaper_<session>. A binary that finds no
// reaper creates one; if another binary's reaper holds the name but is not yet
// listed or is still being removed, testcontainers retries for 20 seconds and
// then returns the Docker conflict. testcontainers v0.44.0 types the conflict
// (errdefs.IsConflict) but not which container conflicted, so the reaper name
// is matched in the message.
func isReaperNameConflict(err error) bool {
	return errdefs.IsConflict(err) && strings.Contains(err.Error(), reaperNamePrefix)
}

// isReaperStarting recognizes testcontainers v0.44.0's error when it finds
// the shared reaper before Docker starts it. The library does not type it.
func isReaperStarting(err error) bool {
	return strings.Contains(err.Error(), "reaper: from container ") && strings.Contains(err.Error(), `: unexpected container status "created"`)
}

// retryReaperRace runs op, trying again with backoff only when concurrent test
// binaries raced to create the reaper or find it in the created state. Other
// errors, and the last reaper conflict once attempts run out, are returned
// unchanged for the caller to fail on. op must release anything a failed
// attempt created.
func retryReaperRace[T any](
	ctx context.Context, t *testing.T, what string, op func() (T, error),
) (T, error) {
	t.Helper()
	policy := backoff.NewExponentialBackOff()
	policy.InitialInterval = 500 * time.Millisecond
	result, err := backoff.Retry(ctx, func() (T, error) {
		value, err := op()
		if err != nil && !isReaperNameConflict(err) && !isReaperStarting(err) {
			return value, backoff.Permanent(err)
		}
		return value, err
	},
		backoff.WithBackOff(policy),
		backoff.WithMaxTries(reaperRaceAttempts),
		backoff.WithNotify(func(err error, wait time.Duration) {
			t.Logf("%s lost the testcontainers reaper race; retrying in %s: %v", what, wait, err)
		}),
	)
	if errors.Is(err, backoff.ErrPermanent) {
		return result, backoff.AsRetryError(err).LastErr
	}
	return result, err
}
