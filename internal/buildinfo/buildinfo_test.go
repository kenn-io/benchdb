package buildinfo_test

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The image build sets the revision with this exact link flag; renaming the
// package or variable would otherwise make it silently do nothing.
func TestImageLinkFlagSetsRevision(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	const commit = "0123456789abcdef0123456789abcdef01234567"
	out, err := exec.Command("go", "run",
		"-ldflags", "-X go.kenn.io/benchdb/internal/buildinfo.revision="+commit,
		"./testdata/printrevision").CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Equal(t, commit, string(out))
}
