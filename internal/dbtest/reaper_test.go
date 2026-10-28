package dbtest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
)

func reaperConflict() error {
	return fmt.Errorf("reaper: new reaper: %w",
		errdefs.ErrConflict.WithMessage(`The container name "/reaper_abc" is already in use`))
}

func TestRetryReaperRace(t *testing.T) {
	tests := []struct {
		name         string
		failures     []error
		wantErr      string
		wantAttempts int
	}{
		{name: "succeeds first time", wantAttempts: 1},
		{
			name:         "recovers after reaper conflicts",
			failures:     []error{reaperConflict(), reaperConflict()},
			wantAttempts: 3,
		},
		{
			name:         "stops at the attempt cap",
			failures:     []error{reaperConflict(), reaperConflict(), reaperConflict(), reaperConflict()},
			wantErr:      "already in use",
			wantAttempts: reaperRaceAttempts,
		},
		{
			name:         "does not retry other errors",
			failures:     []error{errors.New("image pull failed")},
			wantErr:      "image pull failed",
			wantAttempts: 1,
		},
		{
			name:         "does not retry other conflicts",
			failures:     []error{errdefs.ErrConflict.WithMessage(`network "x" already exists`)},
			wantErr:      "already exists",
			wantAttempts: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempts := 0
			got, err := retryReaperRace(context.Background(), t, "test call", func() (string, error) {
				attempts++
				if attempts <= len(tt.failures) {
					return "", tt.failures[attempts-1]
				}
				return "ok", nil
			})
			assert.Equal(t, tt.wantAttempts, attempts)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "ok", got)
		})
	}
}

func TestRetryReaperRaceStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	_, err := retryReaperRace(ctx, t, "test call", func() (string, error) {
		attempts++
		cancel()
		return "", reaperConflict()
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, attempts)
}

// TestReaperNameConflictMatchesDocker creates two containers with one name and
// checks that the daemon's real refusal is classified as a reaper conflict only
// when the name is a reaper's.
func TestReaperNameConflictMatchesDocker(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Docker-backed test in -short mode")
	}
	ctx := context.Background()
	for _, prefix := range []string{"reaper_", "other_"} {
		t.Run(prefix, func(t *testing.T) {
			name := prefix + "decoy-" + uuid.NewString()
			var request testcontainers.GenericContainerRequest
			request.Image = Postgres15Image
			request.Name = name
			first, err := testcontainers.GenericContainer(ctx, request)
			testcontainers.CleanupContainer(t, first)
			require.NoError(t, err)

			second, err := testcontainers.GenericContainer(ctx, request)
			testcontainers.CleanupContainer(t, second)
			require.Error(t, err)
			assert.Equal(t, prefix == "reaper_", isReaperNameConflict(err), "%v", err)
		})
	}
}
