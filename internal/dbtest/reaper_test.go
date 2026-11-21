package dbtest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

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
			name:         "recovers while shared reaper starts",
			failures:     []error{errors.New(`reaper: from container "aaaaaaaa": wait for reaper aaaaaaaa: unexpected container status "created"`)},
			wantAttempts: 2,
		},
		{
			name:         "does not retry other created containers",
			failures:     []error{errors.New(`wait until ready: unexpected container status "created"`)},
			wantErr:      `unexpected container status "created"`,
			wantAttempts: 1,
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
			got, err := retryReaperRace(t.Context(), t, "test call", func() (string, error) {
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

// TestRetryReaperWhileStarting exercises the reaper's error when another
// process has created its container but has not started it yet.
func TestRetryReaperWhileStarting(t *testing.T) {
	ctx := t.Context()
	if session := os.Getenv("BENCHDB_TEST_REAPER_STARTING"); session != "" {
		provider, err := testcontainers.NewDockerProvider()
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, provider.Close()) })
		_, err = testcontainers.NewReaper(ctx, session, provider, "")
		require.True(t, err != nil && isReaperStarting(err), "%v", err)
		return
	}

	session := uuid.NewString()
	id := strings.Repeat("a", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Api-Version", "1.44")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			_, _ = w.Write([]byte("OK"))
		case strings.HasSuffix(r.URL.Path, "/version"):
			_, _ = w.Write([]byte(`{"ApiVersion":"1.44","Version":"test"}`))
		case strings.HasSuffix(r.URL.Path, "/info"):
			_, _ = w.Write([]byte(`{"ID":"test","ServerVersion":"test","OperatingSystem":"test","OSType":"linux"}`))
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			assert.Contains(t, r.URL.Query().Get("filters"), session)
			assert.Contains(t, r.URL.Query().Get("filters"), "reaper_"+session)
			_, _ = fmt.Fprintf(w, `[{"Id":%q,"Names":[%q],"Image":%q,"State":"created","Labels":{"org.testcontainers.sessionId":%q,"org.testcontainers.reaper":"true","org.testcontainers.ryuk":"true"}}]`, id, "/reaper_"+session, testcontainers.ReaperDefaultImage, session)
		case strings.HasSuffix(r.URL.Path, "/containers/"+id+"/json"):
			_, _ = fmt.Fprintf(w, `{"Id":%q,"Name":%q,"Config":{"Image":%q},"State":{"Status":"created","Running":false},"NetworkSettings":{"Ports":{}}}`, id, "/reaper_"+session, testcontainers.ReaperDefaultImage)
		case strings.HasSuffix(r.URL.Path, "/containers/"+id+"/logs"):
			w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		default:
			http.Error(w, "unexpected Docker API request", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	childCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestRetryReaperWhileStarting$")
	cmd.Env = append(os.Environ(), "BENCHDB_TEST_REAPER_STARTING="+session, "DOCKER_HOST="+strings.Replace(server.URL, "http://", "tcp://", 1), "TESTCONTAINERS_RYUK_DISABLED=false", "DOCKER_TLS_VERIFY=", "DOCKER_CERT_PATH=")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestRetryReaperRaceStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
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
	ctx := t.Context()
	for _, prefix := range []string{"reaper_", "other_"} {
		t.Run(prefix, func(t *testing.T) {
			name := prefix + "decoy-" + uuid.NewString()
			var request testcontainers.GenericContainerRequest
			request.Image = Postgres15Image
			request.Name = name
			// Other test binaries can race this one for the shared reaper.
			first, err := retryReaperRace(ctx, t, "start the first container", func() (testcontainers.Container, error) {
				started, err := testcontainers.GenericContainer(ctx, request)
				if err != nil {
					// testcontainers creates the named container before the
					// reaper, so an attempt that lost the reaper race returns
					// a container holding the name the next attempt needs.
					_ = testcontainers.TerminateContainer(started)
				}
				return started, err
			})
			testcontainers.CleanupContainer(t, first)
			require.NoError(t, err)

			second, err := testcontainers.GenericContainer(ctx, request)
			testcontainers.CleanupContainer(t, second)
			require.Error(t, err)
			assert.Equal(t, prefix == "reaper_", isReaperNameConflict(err), "%v", err)
		})
	}
}
