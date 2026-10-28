package dbtest_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/dbtest"
)

// TestIssuedAWSCredentialsWorkFromAPathWithSpaces covers a temporary directory
// with spaces in its path, which the credential command must survive.
func TestIssuedAWSCredentialsWorkFromAPathWithSpaces(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "with spaces")
	require.NoError(t, os.Mkdir(tmp, 0o700))
	t.Setenv("TMPDIR", tmp)
	dbtest.IssueAWSCredentials(t, "access-key", "secret-key", time.Now().Add(time.Hour))

	cfg, err := config.LoadDefaultConfig(t.Context())
	require.NoError(t, err)
	credentials, err := cfg.Credentials.Retrieve(t.Context())

	require.NoError(t, err)
	assert.Equal(t, "access-key", credentials.AccessKeyID)
	assert.Equal(t, "secret-key", credentials.SecretAccessKey)
}
