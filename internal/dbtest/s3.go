package dbtest

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	// S3Image is the disposable S3-compatible service for archive tests:
	// RustFS 1.0.0, the release Kit's S3 conformance job pins. The index
	// digest was read from Docker Hub on 2026-10-25. Update both together.
	S3Image = "rustfs/rustfs:1.0.0@sha256:8cc9801755448b71a786705ce76692c77e14936cccd87cf2fc31842e58f4d1ff"

	s3Port   = "9000/tcp"
	s3Bucket = "benchdb-archive"
)

// S3 is a disposable S3-compatible service with one bucket. Its credentials
// are fixtures generated for the test.
type S3 struct {
	// Endpoint is the service's loopback http origin.
	Endpoint             string
	Bucket               string
	AccessKey, SecretKey string
	// Client manages the bucket outside the code under test.
	Client *minio.Client
}

// NewS3 starts the service with fixture credentials and creates its bucket.
// Like NewArchivePool it never skips for a missing Docker or image; only an
// explicit `go test -short` skips.
func NewS3(t *testing.T) S3 {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping S3 archive test in -short mode")
	}
	ctx := t.Context()
	accessKey, secretKey := "fixture-"+randomPassword(t)[:12], randomPassword(t)
	container, err := retryReaperRace(ctx, t, "start S3",
		func() (testcontainers.Container, error) {
			started, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
				Image:        S3Image,
				ExposedPorts: []string{s3Port},
				Env:          map[string]string{"RUSTFS_ACCESS_KEY": accessKey, "RUSTFS_SECRET_KEY": secretKey},
				WaitingFor:   wait.ForHTTP("/health").WithPort(s3Port),
				Started:      true,
			})
			if err != nil {
				_ = testcontainers.TerminateContainer(started)
			}
			return started, err
		})
	testcontainers.CleanupContainer(t, container)
	require.NoError(t, err, "start S3 container %s (Docker required)", S3Image)
	host, err := container.PortEndpoint(ctx, s3Port, "")
	require.NoError(t, err, "S3 endpoint")
	hostname, _, err := net.SplitHostPort(host)
	require.NoError(t, err, "S3 endpoint")
	ip := net.ParseIP(hostname)
	require.True(t, hostname == "localhost" || (ip != nil && ip.IsLoopback()),
		"S3 archive tests need a loopback endpoint, got %s; run Docker locally", host)
	client, err := minio.New(host, &minio.Options{Creds: credentials.NewStaticV4(accessKey, secretKey, "")})
	require.NoError(t, err)
	require.NoError(t, client.MakeBucket(ctx, s3Bucket, minio.MakeBucketOptions{}))
	return S3{Endpoint: "http://" + host, Bucket: s3Bucket, AccessKey: accessKey, SecretKey: secretKey, Client: client}
}

// IssueAWSCredentials makes the AWS credential chain of this process,
// including DuckDB's, return the given keys, expiring at expires, until the
// test ends. It clears every AWS_* variable, so ambient keys, profiles, web
// identity, container credentials, and instance metadata never reach the
// test.
//
// The AWS SDK inside DuckDB reads its config file once per process. Every
// call therefore writes the same config file, at a path fixed for the
// process, whose credential_process command prints the keys issued last.
func IssueAWSCredentials(t *testing.T, accessKey, secretKey string, expires time.Time) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "AWS_") {
			t.Setenv(name, "") // Restores the value after the test.
			require.NoError(t, os.Unsetenv(name))
		}
	}
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("benchdb-test-aws-%d", os.Getpid()))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	issued := filepath.Join(dir, "issued.json")
	script := filepath.Join(dir, "issue-credentials")
	config := filepath.Join(dir, "config")
	files := []struct {
		path, content string
		mode          os.FileMode
	}{
		{issued, fmt.Sprintf(`{"Version":1,"AccessKeyId":%q,"SecretAccessKey":%q,"Expiration":%q}`,
			accessKey, secretKey, expires.UTC().Format(time.RFC3339)), 0o600},
		// Both AWS SDKs run credential_process through a shell, so quote paths
		// that may contain spaces, as a temporary directory can. The Go SDK
		// strips quotes around a whole value, so the path cannot come first.
		{script, "#!/bin/sh\nexec cat " + shellQuote(issued) + "\n", 0o700},
		{config, "[default]\ncredential_process = /bin/sh " + shellQuote(script) + "\n", 0o600},
	}
	for _, file := range files {
		require.NoError(t, os.WriteFile(file.path, []byte(file.content), file.mode))
	}
	t.Setenv("AWS_CONFIG_FILE", config)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

// shellQuote quotes s as one POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
