package columnar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"cloud.google.com/go/auth"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/signer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/stacktest"

	"go.kenn.io/benchdb/internal/dbtest"
)

func TestArchiveLocationSettings(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cfg       Config
		namespace string
		endpoint  string
		object    string
	}{
		{"implicit local", Config{Dir: "/work"}, "", "", "/work/data/x.parquet"},
		{"explicit local", Config{Dir: "/work", ArchiveURL: "file:///srv/archive/"}, "", "", "/srv/archive/data/x.parquet"},
		{"bucket root", Config{ArchiveURL: "s3://bucket"}, "bucket/", "", "s3://bucket/data/x.parquet"},
		{"prefix", Config{ArchiveURL: "s3://bucket/a/b"}, "bucket/a/b/", "", "s3://bucket/a/b/data/x.parquet"},
		{"prefix slash", Config{ArchiveURL: "s3://bucket/a/b/"}, "bucket/a/b/", "", "s3://bucket/a/b/data/x.parquet"},
		{"endpoint default port", Config{ArchiveURL: "s3://bucket/a", S3Endpoint: "HTTP://LocalHost:80/"}, "bucket/a/", "http://localhost", "s3://bucket/a/data/x.parquet"},
		{"endpoint port", Config{ArchiveURL: "s3://bucket/a", S3Endpoint: "https://store.example:9000", S3URLStyle: "path"}, "bucket/a/", "https://store.example:9000", "s3://bucket/a/data/x.parquet"},
		{"gcs", Config{ArchiveURL: "gs://bucket/p", GCSCredentialsFile: "/creds.json"}, "bucket/p/", "", "gs://bucket/p/data/x.parquet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			location, err := parseArchiveLocation(tc.cfg)
			require.NoError(t, err)
			assert.Equal(t, tc.namespace, location.namespace())
			assert.Equal(t, tc.endpoint, location.endpoint)
			assert.Equal(t, tc.object, location.objectURL("data/x.parquet"))
		})
	}
	for _, tc := range []struct {
		name    string
		cfg     Config
		message string
	}{
		{"credentials", Config{ArchiveURL: "s3://key:hunter2@bucket/p"}, "must not contain credentials"},
		{"query", Config{ArchiveURL: "s3://bucket/p?versionId=1"}, "without a query"},
		{"scheme", Config{ArchiveURL: "azure://bucket/p"}, "file, s3, or gs scheme"},
		{"relative file", Config{ArchiveURL: "file:archive"}, "must be a file://, s3://, or gs:// URL"},
		{"file host", Config{ArchiveURL: "file://host/archive"}, "absolute local path"},
		{"bucket port", Config{ArchiveURL: "s3://bucket:9000/p"}, "without a port"},
		{"bucket case", Config{ArchiveURL: "gs://Bucket/p"}, "lowercase"},
		{"empty segment", Config{ArchiveURL: "s3://bucket/a//b"}, "invalid object prefix"},
		{"escaped prefix", Config{ArchiveURL: "s3://bucket/a%2Fb"}, "without a query or fragment"},
		{"s3 setting on gcs", Config{ArchiveURL: "gs://bucket/p", S3Endpoint: "https://store.example"}, "BENCHDB_ARCHIVE_S3_ENDPOINT applies only to a s3://"},
		{"gcs setting on s3", Config{ArchiveURL: "s3://bucket/p", GCSCredentialsFile: "/creds.json"}, "BENCHDB_ARCHIVE_GCS_CREDENTIALS_FILE applies only to a gs://"},
		{"region without url", Config{S3Region: "eu-west-1"}, "BENCHDB_ARCHIVE_S3_REGION applies only"},
		{"style on file", Config{ArchiveURL: "file:///srv/archive", S3URLStyle: "path"}, "BENCHDB_ARCHIVE_S3_URL_STYLE applies only"},
		{"style", Config{ArchiveURL: "s3://bucket/p", S3URLStyle: "virtual"}, "path or vhost"},
		{"endpoint path", Config{ArchiveURL: "s3://bucket/p", S3Endpoint: "https://store.example/s3"}, "without credentials, path"},
		{"endpoint credentials", Config{ArchiveURL: "s3://bucket/p", S3Endpoint: "https://key:hunter2@store.example"}, "without credentials, path"},
		{"endpoint scheme", Config{ArchiveURL: "s3://bucket/p", S3Endpoint: "store.example:9000"}, "http or https origin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseArchiveLocation(tc.cfg)
			require.ErrorContains(t, err, tc.message)
			assert.NotContains(t, err.Error(), "hunter2")
		})
	}
}

func TestAWSUpstreamsFollowRegionPartition(t *testing.T) {
	for region, host := range map[string]string{
		"eu-west-1":  "s3.eu-west-1.amazonaws.com",
		"cn-north-1": "s3.cn-north-1.amazonaws.com.cn",
	} {
		location, err := parseArchiveLocation(Config{ArchiveURL: "s3://bucket/p", S3Region: region})
		require.NoError(t, err)
		upstreams, err := location.duckdbUpstreams()
		require.NoError(t, err)
		assert.Equal(t, []string{host + ":443", "bucket." + host + ":443"}, upstreams, region)
	}
}

func TestArchiveURLFromEnvironment(t *testing.T) {
	t.Setenv("BENCHDB_DATA_DIR", t.TempDir())
	t.Setenv("BENCHDB_ARCHIVE_URL", "s3://bucket/archive")
	t.Setenv("BENCHDB_ARCHIVE_S3_ENDPOINT", "http://127.0.0.1:9000")
	t.Setenv("BENCHDB_ARCHIVE_S3_REGION", "eu-west-1")
	t.Setenv("BENCHDB_ARCHIVE_S3_URL_STYLE", "path")
	cfg, err := ConfigFromEnv()
	require.NoError(t, err)
	location, err := parseArchiveLocation(cfg)
	require.NoError(t, err)
	assert.Equal(t, archiveLocation{backend: backendS3, bucket: "bucket", prefix: "archive/", endpoint: "http://127.0.0.1:9000", region: "eu-west-1", pathStyle: true}, location)

	t.Setenv("BENCHDB_ARCHIVE_GCS_CREDENTIALS_FILE", "/creds.json")
	_, err = ConfigFromEnv()
	require.ErrorContains(t, err, "BENCHDB_ARCHIVE_GCS_CREDENTIALS_FILE applies only to a gs://")
}

// remoteArchive is one database and one S3 service with fixture credentials
// as the only AWS credential source.
type remoteArchive struct {
	pool *pgxpool.Pool
	s3   dbtest.S3
}

func newRemoteArchive(t *testing.T) (remoteArchive, context.Context) {
	t.Helper()
	pool, _, ctx := dbtest.NewArchivePool(t, dbtest.ArchivePostgresImage(), dbtest.Direct)
	s3 := dbtest.NewS3(t)
	dbtest.IssueAWSCredentials(t, s3.AccessKey, s3.SecretKey, time.Now().Add(time.Hour))
	return remoteArchive{pool: pool, s3: s3}, ctx
}

func (r remoteArchive) config(prefix, dir string) Config {
	return Config{Dir: dir, ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"),
		ArchiveURL: "s3://" + r.s3.Bucket + "/" + prefix, S3Endpoint: r.s3.Endpoint, S3URLStyle: "path",
		Readers: 1, BatchRows: 50, TargetRows: 100, Settle: time.Nanosecond, Retention: immediateRetention}
}

// start opens cfg's archive after checking that Kit can reach only this
// host, and confines the engine's DuckDB handles to this host.
func (r remoteArchive) start(t *testing.T, ctx context.Context, cfg Config) (*Engine, error) {
	t.Helper()
	return startConfined(t, ctx, r.pool, cfg)
}

func (r remoteArchive) open(t *testing.T, ctx context.Context, cfg Config) *Engine {
	t.Helper()
	e, err := r.start(t, ctx, cfg)
	require.NoError(t, err)
	return e
}

func startConfined(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cfg Config) (*Engine, error) {
	t.Helper()
	if cfg.S3Endpoint != "" || strings.HasPrefix(cfg.ArchiveURL, "s3:") {
		endpoint, err := url.Parse(cfg.S3Endpoint)
		require.NoError(t, err)
		require.True(t, isLoopback(endpoint.Hostname()), "S3 tests use a loopback endpoint, never a provider")
	}
	cfg.dialArchive = loopbackDial(t)
	e, err := Open(ctx, pool, cfg)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	if e.proxy != nil {
		// The engine's own proxy carries remote requests, and loopbackDial
		// keeps it on this host.
		confineSecrets(t, e.handles...)
	} else {
		confineDuckDB(t, e.handles...)
	}
	return e, nil
}

// loopbackDial dials only loopback addresses. The test fails if the engine's
// archive proxy tries any other address.
func loopbackDial(t *testing.T) func(context.Context, string, string) (net.Conn, error) {
	t.Helper()
	var mu sync.Mutex
	escaped := []string{}
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		assert.Empty(t, escaped, "the archive proxy must stay on this host")
	})
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil || !isLoopback(host) {
			mu.Lock()
			escaped = append(escaped, address)
			mu.Unlock()
			return nil, fmt.Errorf("test requests stay on this host, not %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
}

func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

// refusingAddress is a loopback address where nothing listens.
func refusingAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	return address
}

// confineDuckDB keeps the remote requests of DuckDB handles built without an
// engine proxy on this host, even when an archive secret loses its endpoint.
// It adds confineSecrets' fallback secrets and sends every request through a
// proxy that forwards only to loopback addresses. The proxy refuses anything
// else without connecting, and the test fails.
func confineDuckDB(t *testing.T, handles ...*sql.DB) {
	t.Helper()
	var mu sync.Mutex
	escaped := []string{}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect || !isLoopback(r.URL.Hostname()) {
			mu.Lock()
			escaped = append(escaped, r.Method+" "+r.Host)
			mu.Unlock()
			http.Error(w, "test requests stay on this host", http.StatusForbidden)
			return
		}
		out := r.Clone(r.Context())
		out.RequestURI = ""
		response, err := http.DefaultTransport.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = response.Body.Close() }()
		maps.Copy(w.Header(), response.Header)
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	t.Cleanup(func() {
		proxy.Close()
		mu.Lock()
		defer mu.Unlock()
		assert.Empty(t, escaped, "DuckDB requests must stay on this host")
	})
	for _, handle := range handles {
		_, err := handle.ExecContext(t.Context(), "LOAD httpfs")
		require.NoError(t, err)
		statement := "SET GLOBAL http_proxy=" + literal(proxy.Listener.Addr().String())
		_, err = handle.ExecContext(t.Context(), statement)
		require.NoError(t, err, statement)
	}
	confineSecrets(t, handles...)
}

// confineSecrets adds unscoped fallback S3 and GCS secrets that send URLs
// outside the archive secret's scope to a loopback port that refuses
// connections.
func confineSecrets(t *testing.T, handles ...*sql.DB) {
	t.Helper()
	refused := literal(refusingAddress(t))
	for _, handle := range handles {
		for _, statement := range []string{"LOAD httpfs",
			"CREATE OR REPLACE SECRET loopback_s3 (TYPE s3, KEY_ID 'fixture-fallback', SECRET 'fixture-fallback', ENDPOINT " + refused + ", URL_STYLE 'path', USE_SSL false)",
			"CREATE OR REPLACE SECRET loopback_gcs (TYPE gcs, BEARER_TOKEN 'fixture-fallback', ENDPOINT " + refused + ", URL_STYLE 'path', USE_SSL false)",
		} {
			_, err := handle.ExecContext(t.Context(), statement)
			require.NoError(t, err, statement)
		}
	}
}

// keys lists the bucket's object keys under prefix.
func (r remoteArchive) keys(t *testing.T, ctx context.Context, prefix string) []string {
	t.Helper()
	keys := []string{}
	for object := range r.s3.Client.ListObjects(ctx, r.s3.Bucket, minio.ListObjectsOptions{Prefix: prefix + "/", Recursive: true}) {
		require.NoError(t, object.Err)
		keys = append(keys, strings.TrimPrefix(object.Key, prefix+"/"))
	}
	return keys
}

// scopedIDs reads the visible result IDs matching where from the facts that
// scope selects.
func scopedIDs(ctx context.Context, e *Engine, scope queryScope, where string) ([]string, error) {
	ids := []string{}
	err := e.Read(ctx, func(ctx context.Context, snapshot *ReadSnapshot) error {
		if err := snapshot.selectFacts(ctx, scope); err != nil {
			return err
		}
		var err error
		ids, err = collectRows(ctx, snapshot, `SELECT id FROM benchmark_result WHERE `+where+` ORDER BY id`, nil, scanString)
		return err
	})
	return ids, err
}

// remoteAnswers reads one result, one run, and one history through the
// lookups, as point queries do.
func remoteAnswers(t *testing.T, ctx context.Context, e *Engine, fixtures []lookupFixture) map[string][]string {
	t.Helper()
	answers := map[string][]string{}
	for name, query := range map[string]struct {
		scope queryScope
		where string
	}{
		"result":  {queryScope{ResultIDs: []string{fixtures[0].id}}, "id=" + literal(fixtures[0].id)},
		"run":     {queryScope{RunIDs: []string{fixtures[1].run}}, "run_id=" + literal(fixtures[1].run)},
		"history": {queryScope{Fingerprints: []string{fixtures[2].fingerprint}}, "history_fingerprint=" + literal(fixtures[2].fingerprint)},
	} {
		ids, err := scopedIDs(ctx, e, query.scope, query.where)
		require.NoError(t, err, name)
		answers[name] = ids
	}
	return answers
}

func expectedAnswers(fixtures []lookupFixture) map[string][]string {
	answers := map[string][]string{"result": {fixtures[0].id}}
	for _, fixture := range fixtures {
		if fixture.run == fixtures[1].run {
			answers["run"] = append(answers["run"], fixture.id)
		}
		if fixture.fingerprint == fixtures[2].fingerprint {
			answers["history"] = append(answers["history"], fixture.id)
		}
	}
	for _, ids := range answers {
		slices.Sort(ids)
	}
	return answers
}

// TestRemoteIdentityAndRestart exports to S3, builds the lookups across a
// replaced working directory, and reads the same answers from every fresh
// working directory. Startup checks the dataset marker, not every object.
func TestRemoteIdentityAndRestart(t *testing.T) {
	r, ctx := newRemoteArchive(t)
	fixtures := legacyFixtures(2, 6)
	insertLookupResults(t, ctx, r.pool, fixtures...)
	expected := expectedAnswers(fixtures)

	first := r.config("archive", t.TempDir())
	e := r.open(t, ctx, first)
	published := exportSettled(t, ctx, e, r.pool)
	require.Len(t, published, 2)
	require.Zero(t, hotRows(t, ctx, r.pool))
	keys := r.keys(t, ctx, "archive")
	assert.Contains(t, keys, datasetMarker)
	for _, path := range published {
		assert.Contains(t, keys, path, "facts are published to the bucket")
		assert.NoFileExists(t, filepath.Join(first.Dir, path), "facts are not kept in the working directory")
	}

	// Remote facts are never selected without a complete lookup directory.
	forgetLookups(t, ctx, r.pool)
	_, err := scopedIDs(ctx, e, queryScope{ResultIDs: []string{fixtures[0].id}}, "true")
	require.ErrorIs(t, err, errRemoteLookupsIncomplete)

	// The backfill checkpoints in the working directory, which a remote
	// deployment may discard between restarts.
	e.lookupTarget, e.lookupBackfillBudget = 4, 0
	complete, err := runBackfill(ctx, e)
	require.NoError(t, err)
	require.False(t, complete)
	require.NotEmpty(t, backfillFiles(t, first.Dir))
	require.NoError(t, e.Close())
	require.NoError(t, os.RemoveAll(first.Dir))

	second := r.config("archive", t.TempDir())
	e = r.open(t, ctx, second)
	backfillUntilComplete(t, ctx, e)
	expectedIDs := remoteAnswers(t, ctx, e, fixtures)
	assert.Equal(t, expected, expectedIDs)
	require.NoError(t, e.Close())

	third := r.config("archive", t.TempDir())
	e = r.open(t, ctx, third)
	readIDsAfterScratchReplacement := remoteAnswers(t, ctx, e, fixtures)
	for name, ids := range expectedIDs {
		assert.ElementsMatch(t, ids, readIDsAfterScratchReplacement[name], name)
	}
	require.NoError(t, e.Close())

	// Startup reads the marker and binding only, so a missing fact does not
	// stop it; a read that needs the object fails instead of omitting it.
	require.NoError(t, r.s3.Client.RemoveObject(ctx, r.s3.Bucket, "archive/"+published[0], minio.RemoveObjectOptions{}))
	e = r.open(t, ctx, r.config("archive", t.TempDir()))
	_, err = scopedIDs(ctx, e, queryScope{AllFacts: true}, "true")
	require.Error(t, err)
}

// TestRemoteBindingRejectsChangedLocation shows that a database bound to one
// archive refuses another bucket prefix, endpoint, or the local archive, and
// accepts an equivalent spelling of its own endpoint.
func TestRemoteBindingRejectsChangedLocation(t *testing.T) {
	r, ctx := newRemoteArchive(t)
	insertHotResults(t, ctx, r.pool, 2)
	bound := r.config("bound", t.TempDir())
	e := r.open(t, ctx, bound)
	require.NotEmpty(t, exportSettled(t, ctx, e, r.pool))
	require.NoError(t, e.Close())

	for name, cfg := range map[string]Config{
		"prefix": r.config("other", t.TempDir()),
		"local":  {Dir: t.TempDir(), ExtensionDir: bound.ExtensionDir},
		"file":   {Dir: t.TempDir(), ExtensionDir: bound.ExtensionDir, ArchiveURL: "file://" + t.TempDir()},
	} {
		_, err := r.start(t, ctx, cfg)
		require.ErrorContains(t, err, "the archive database is bound to s3://"+r.s3.Bucket+"/bound/ at "+r.s3.Endpoint, name)
	}
	assert.Empty(t, r.keys(t, ctx, "other"), "a refused location is never claimed")

	equivalent := bound
	equivalent.Dir = t.TempDir()
	equivalent.S3Endpoint = strings.ToUpper(r.s3.Endpoint[:4]) + r.s3.Endpoint[4:] + "/"
	e = r.open(t, ctx, equivalent)
	assert.Len(t, visibleIDs(t, ctx, e), 2)
}

// TestRemoteNamespaceClaim shows that a new archive claims only an empty
// namespace and that a marker from another database is refused.
func TestRemoteNamespaceClaim(t *testing.T) {
	r, ctx := newRemoteArchive(t)
	put := func(key, body string) {
		_, err := r.s3.Client.PutObject(ctx, r.s3.Bucket, key, bytes.NewReader([]byte(body)), int64(len(body)), minio.PutObjectOptions{})
		require.NoError(t, err)
	}
	put("occupied/notes.txt", "unrelated")
	put("foreign/"+datasetMarker, "0198f2a4-1b2c-7d3e-8f40-123456789abc\n")

	_, err := r.start(t, ctx, r.config("occupied", t.TempDir()))
	require.ErrorContains(t, err, "namespace is not empty and has no BenchDB dataset marker")
	assert.Equal(t, []string{"notes.txt"}, r.keys(t, ctx, "occupied"), "nothing is written into an occupied namespace")

	_, err = r.start(t, ctx, r.config("foreign", t.TempDir()))
	require.ErrorContains(t, err, "belongs to a different database")

	e := r.open(t, ctx, r.config("fresh", t.TempDir()))
	require.NoError(t, e.Close())
	assert.Equal(t, []string{datasetMarker}, r.keys(t, ctx, "fresh"))
	var id string
	require.NoError(t, r.pool.QueryRow(ctx, `SELECT id::text FROM archive_dataset`).Scan(&id))
	object, err := r.s3.Client.GetObject(ctx, r.s3.Bucket, "fresh/"+datasetMarker, minio.GetObjectOptions{})
	require.NoError(t, err)
	var marker bytes.Buffer
	_, err = marker.ReadFrom(object)
	require.NoError(t, err)
	assert.Equal(t, id+"\n", marker.String())
}

var accessKeyID = regexp.MustCompile(`Credential=([^/]+)/`)

// rotatingS3 fronts the S3 service and accepts only the access key that the
// test currently issues, as a provider does after rotating temporary
// credentials. It re-signs accepted requests with the service's fixture keys.
type rotatingS3 struct {
	mu       sync.Mutex
	accepted string
	// used records each signed request's client and access key.
	used []string
}

func (p *rotatingS3) accept(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.accepted = key
}

// usedSince returns the access keys that client signed requests with after
// the first mark requests.
func (p *rotatingS3) usedSince(mark int, client string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	keys := []string{}
	for _, entry := range p.used[mark:] {
		if name, key, _ := strings.Cut(entry, " "); name == client {
			keys = append(keys, key)
		}
	}
	return keys
}

func (p *rotatingS3) mark() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.used)
}

func (p *rotatingS3) serve(t *testing.T, s3 dbtest.S3) *httptest.Server {
	upstream, err := url.Parse(s3.Endpoint)
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := ""
		if match := accessKeyID.FindStringSubmatch(r.Header.Get("Authorization")); match != nil {
			key = match[1]
		}
		client := "kit"
		if strings.HasPrefix(r.Header.Get("User-Agent"), "duckdb") {
			client = "duckdb"
		}
		p.mu.Lock()
		p.used = append(p.used, client+" "+key)
		accepted := key == p.accepted
		p.mu.Unlock()
		if !accepted {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>InvalidAccessKeyId</Code><Message>The access key was revoked.</Message></Error>`)
			return
		}
		out, err := http.NewRequestWithContext(r.Context(), r.Method, upstream.String()+r.URL.RequestURI(), r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		out.ContentLength = r.ContentLength
		for name, values := range r.Header {
			switch strings.ToLower(name) {
			case "authorization", "x-amz-date", "x-amz-security-token":
			default:
				out.Header[name] = values
			}
		}
		out = signer.SignV4(*out, s3.AccessKey, s3.SecretKey, "", "us-east-1")
		response, err := http.DefaultTransport.RoundTrip(out)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer func() { _ = response.Body.Close() }()
		maps.Copy(w.Header(), response.Header)
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	t.Cleanup(server.Close)
	return server
}

// TestAWSCredentialRefresh rotates the temporary credentials that the AWS
// chain supplies while one engine keeps its DuckDB handles open. DuckDB
// resolves the chain again when the provider rejects the old key, and Kit's
// uploads switch keys once the old credentials expire.
func TestAWSCredentialRefresh(t *testing.T) {
	pool, _, ctx := dbtest.NewArchivePool(t, dbtest.ArchivePostgresImage(), dbtest.Direct)
	s3 := dbtest.NewS3(t)
	proxy := &rotatingS3{accepted: "rotating-key-a"}
	endpoint := proxy.serve(t, s3)
	firstExpiry := time.Now().Add(20 * time.Second)
	dbtest.IssueAWSCredentials(t, "rotating-key-a", "fixture-secret", firstExpiry)
	insertHotResults(t, ctx, pool, 3)
	r := remoteArchive{pool: pool, s3: s3}
	cfg := r.config("rotating", t.TempDir())
	cfg.S3Endpoint = endpoint.URL
	e := r.open(t, ctx, cfg)
	require.NotEmpty(t, exportSettled(t, ctx, e, pool))
	before := visibleIDs(t, ctx, e)
	require.Len(t, before, 3)

	// The provider revokes the first key and the chain issues a second.
	dbtest.IssueAWSCredentials(t, "rotating-key-b", "fixture-secret", time.Now().Add(time.Hour))
	proxy.accept("rotating-key-b")
	mark := proxy.mark()
	assert.Equal(t, before, visibleIDs(t, ctx, e), "the long-lived reader handle reads with the new key")
	duckdb := proxy.usedSince(mark, "duckdb")
	require.NotEmpty(t, duckdb)
	assert.Equal(t, "rotating-key-b", duckdb[len(duckdb)-1])

	// Kit's credential cache renews when the first credentials expire.
	time.Sleep(time.Until(firstExpiry))
	insertHotResults(t, ctx, pool, 2)
	mark = proxy.mark()
	require.NotEmpty(t, exportSettled(t, ctx, e, pool))
	assert.Contains(t, proxy.usedSince(mark, "kit"), "rotating-key-b")
	assert.NotContains(t, proxy.usedSince(mark, "kit"), "rotating-key-a")
	assert.Len(t, visibleIDs(t, ctx, e), 5)
}

// googleTokens is a token endpoint for an external_account credential file.
// It issues numbered tokens with a configurable lifetime, or fails. With a
// reissue time it behaves like a metadata server: it returns its current
// token, with the time that token has left, until that falls below reissue.
type googleTokens struct {
	mu       sync.Mutex
	lifetime time.Duration
	reissue  time.Duration
	fail     bool
	expiry   map[string]time.Time
	current  string
	requests int
}

func (g *googleTokens) set(lifetime, reissue time.Duration, fail bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lifetime, g.reissue, g.fail = lifetime, reissue, fail
}

// counts returns the tokens issued and the requests served.
func (g *googleTokens) counts() (issued, requests int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.expiry), g.requests
}

// remaining is how long token stays valid, or false for an unknown token.
func (g *googleTokens) remaining(token string) (time.Duration, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	expiry, ok := g.expiry[token]
	return time.Until(expiry), ok
}

func (g *googleTokens) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests++
	if g.fail {
		http.Error(w, `{"error":"temporarily_unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	// A returned token's lifetime is rounded down to whole seconds, so the
	// client never believes it lasts longer than it does.
	left := int(time.Until(g.expiry[g.current]).Seconds())
	if g.current == "" || g.reissue == 0 || time.Until(g.expiry[g.current]) <= g.reissue {
		g.current = fmt.Sprintf("fixture-token-%d", len(g.expiry))
		g.expiry[g.current] = time.Now().Add(g.lifetime)
		left = int(g.lifetime.Seconds())
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"access_token":%q,"issued_token_type":"urn:ietf:params:oauth:token-type:access_token","token_type":"Bearer","expires_in":%d}`,
		g.current, left)
}

// credentialsFile writes a workload identity federation credential that
// exchanges a fixture subject token at endpoint.
func (g *googleTokens) credentialsFile(t *testing.T, endpoint string) string {
	t.Helper()
	dir := t.TempDir()
	subject := filepath.Join(dir, "subject-token")
	require.NoError(t, os.WriteFile(subject, []byte("fixture-subject-token"), 0o600))
	file := filepath.Join(dir, "credentials.json")
	require.NoError(t, os.WriteFile(file, fmt.Appendf(nil, `{"type":"external_account",
"audience":"//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/fixture/providers/fixture",
"subject_token_type":"urn:ietf:params:oauth:token-type:jwt","token_url":%q,"credential_source":{"file":%q}}`, endpoint+"/token", subject), 0o600))
	return file
}

// gcsReads serves an archive directory as a GCS bucket's XML API. Like GCS,
// it rejects unknown and expired bearer tokens. It also records, by how much,
// each request whose token expires at or before floor.
type gcsReads struct {
	root, prefix string
	tokens       *googleTokens
	mu           sync.Mutex
	requests     int
	floor        time.Time
	short        []time.Duration
	// stall holds each request until its token has expired, like a slow
	// statement that outlives its token.
	stall bool
}

func (g *gcsReads) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	remaining, known := g.tokens.remaining(token)
	g.mu.Lock()
	g.requests++
	if expiry := time.Now().Add(remaining); known && !expiry.After(g.floor) {
		g.short = append(g.short, g.floor.Sub(expiry))
	}
	stall := g.stall
	g.mu.Unlock()
	if stall && known {
		time.Sleep(remaining + 100*time.Millisecond)
		remaining, known = g.tokens.remaining(token)
	}
	if !known || remaining <= 0 {
		http.Error(w, "<Error><Code>AuthenticationRequired</Code></Error>", http.StatusUnauthorized)
		return
	}
	key, ok := strings.CutPrefix(r.URL.Path, g.prefix)
	if !ok || !filepath.IsLocal(key) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(g.root, filepath.FromSlash(key)))
}

// expectFloor sets the time every later request's token must outlive.
func (g *gcsReads) expectFloor(floor time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.floor = floor
}

func (g *gcsReads) counts() (int, []time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requests, slices.Clone(g.short)
}

// TestGCSTokenRenewal reads archived facts as gs:// objects through DuckDB,
// with bearer tokens from the production credential builder and a fixture
// workload identity endpoint. Times are scaled down: a 30-second token
// lifetime stands for an hour, and a 5-second read deadline for two minutes.
// The deadlines leave room for a heavily loaded test host.
func TestGCSTokenRenewal(t *testing.T) {
	t.Setenv("GOOGLE_AUTH_TRUST_BOUNDARY_ENABLED", "")
	pool, ctx := dbtest.NewPool(t)
	fixtures := legacyFixtures(1, 4)
	insertLookupResults(t, ctx, pool, fixtures...)
	// The unit deadline is longer than a token lifetime, so requiring a token
	// to outlive it would reject every token. Reads and builder statements
	// need a token that outlives the read deadline plus skew: 6 seconds.
	const readDeadline, unitDeadline, skew, margin = 5 * time.Second, time.Minute, time.Second, 20 * time.Second
	const lifetime, reissue = 30 * time.Second, 10 * time.Second
	cfg := Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, ReadTimeout: readDeadline,
		BatchRows: 50, TargetRows: 100, Settle: time.Nanosecond, Retention: immediateRetention}
	e, err := startConfined(t, ctx, pool, cfg)
	require.NoError(t, err)
	_, err = e.Maintain(ctx)
	require.NoError(t, err)
	require.Zero(t, hotRows(t, ctx, pool))
	later := legacyFixtures(2, 2)[2:]
	insertLookupResults(t, ctx, pool, later...)

	// The pinned auth library's cache returns a token only while it outlives
	// the margin, and otherwise fetches a new one while the caller waits.
	// A margin longer than the library's 3m45s default shows that the
	// configured margin, not the default, decides.
	const longMargin = 5 * time.Minute
	tokens := &googleTokens{lifetime: longMargin + 2*time.Second, expiry: map[string]time.Time{}}
	tokenServer := httptest.NewServer(tokens)
	t.Cleanup(tokenServer.Close)
	file := tokens.credentialsFile(t, tokenServer.URL)
	cached, err := googleCredentials(file, longMargin)
	require.NoError(t, err)
	first, err := cached.Token(ctx)
	require.NoError(t, err)
	again, err := cached.Token(ctx)
	require.NoError(t, err)
	assert.Equal(t, first.Value, again.Value, "a token outliving the margin is reused")
	time.Sleep(time.Until(first.Expiry.Add(-longMargin)) + 100*time.Millisecond)
	second, err := cached.Token(ctx)
	require.NoError(t, err)
	assert.NotEqual(t, first.Value, second.Value, "a token inside the margin is replaced before it is returned")
	assert.Greater(t, time.Until(second.Expiry), longMargin)

	// Serve the engine's archived files as gs:// objects, and give its
	// handles the shared credential as a GCS engine has.
	reads := &gcsReads{root: cfg.Dir, prefix: "/fixture-bucket/archive/", tokens: tokens}
	gcsServer := httptest.NewServer(reads)
	t.Cleanup(gcsServer.Close)
	google, err := googleCredentials(file, margin)
	require.NoError(t, err)
	e.location = archiveLocation{backend: backendGCS, bucket: "fixture-bucket", prefix: "archive/"}
	e.gcsEndpoint = gcsServer.Listener.Addr().String()
	e.tokens, e.tokenMargin, e.tokenSkew, e.tokenRefetch, e.unitTimeout = google.TokenProvider, margin, skew, time.Second, unitDeadline
	for _, h := range e.handleTokens {
		h.ready = true
	}
	read := func() error {
		_, err := scopedIDs(ctx, e, queryScope{ResultIDs: []string{fixtures[0].id}}, "true")
		return err
	}

	// Builder statements and reads need a token that outlives the read
	// deadline plus skew. Below that floor, even after renewal, the unit
	// and the read fail, and the unit keeps its rows.
	tokens.set(3*time.Second, 0, false)
	_, err = e.Maintain(ctx)
	require.ErrorContains(t, err, "before this operation's deadline")
	require.EqualValues(t, 2, hotRows(t, ctx, pool))
	require.ErrorContains(t, read(), "before this operation's deadline")

	// A failing token source fails reads and maintenance once the current
	// token cannot clear the floor, before any request uses it.
	tokens.set(3*time.Second, 0, true)
	requests, _ := reads.counts()
	require.ErrorContains(t, read(), "renew the GCS archive access token")
	after, _ := reads.counts()
	assert.Equal(t, requests, after, "no request uses the expiring token")
	_, err = e.Maintain(ctx)
	require.ErrorContains(t, err, "renew the GCS archive access token")

	// A statement that outlives its token fails its unit, which keeps its
	// rows. The next cycle succeeds. The token clears the floor by 3 seconds.
	tokens.set(9*time.Second, 0, false)
	reads.mu.Lock()
	reads.stall = true
	reads.mu.Unlock()
	_, err = e.Maintain(ctx)
	require.ErrorContains(t, err, "401")
	require.EqualValues(t, 2, hotRows(t, ctx, pool))
	reads.mu.Lock()
	reads.stall = false
	reads.mu.Unlock()
	tokens.set(lifetime, reissue, false)
	_, err = e.Maintain(ctx)
	require.NoError(t, err)
	require.Zero(t, hotRows(t, ctx, pool))

	// A metadata-style source returns the same token until shortly before
	// it expires. Through a whole token lifetime and its replacement, reads
	// and maintenance cycles succeed, including cycles that start inside the
	// renewal margin while the source still returns the old token. Inside
	// the margin, both keep the cached token while it clears the floor and
	// ask the source at most once per refetch interval. Every request's token
	// outlives the deadline plus skew of an operation starting when its
	// read or cycle starts, which is the floor each operation enforces.
	issuedBefore, requestsBefore := tokens.counts()
	all := append(slices.Clone(fixtures), later...)
	expected := expectedAnswers(all)
	reads.mu.Lock()
	reads.short = nil
	reads.mu.Unlock()
	readCount, cycles, insideMargin := 0, 0, 0
	for start := time.Now(); time.Since(start) < lifetime+5*time.Second; time.Sleep(100 * time.Millisecond) {
		reads.expectFloor(time.Now().Add(readDeadline + skew))
		require.Equal(t, expected, remoteAnswers(t, ctx, e, all))
		readCount++
		if readCount%2 != 0 {
			continue
		}
		e.tokenMu.Lock()
		left := time.Until(e.token.Expiry)
		e.tokenMu.Unlock()
		if left > reissue && left < margin {
			insideMargin++
		}
		cycles++
		reads.expectFloor(time.Now().Add(readDeadline + skew))
		insertLookupResults(t, ctx, pool, lookupFixture{id: fmt.Sprintf("cycle-%03d", cycles), run: fmt.Sprintf("cycle-run-%d", cycles),
			fingerprint: fmt.Sprintf("cycle-history-%d", cycles), at: lookupJanuary})
		_, err := e.Maintain(ctx)
		require.NoError(t, err, "cycle %d started with %s left on the token", cycles, left)
		require.Zero(t, hotRows(t, ctx, pool))
	}
	issued, requested := tokens.counts()
	_, short := reads.counts()
	t.Logf("%d reads and %d maintenance cycles over %s (%d started inside the renewal margin): %d new tokens, %d token requests",
		readCount, cycles, lifetime+5*time.Second, insideMargin, issued-issuedBefore, requested-requestsBefore)
	assert.Empty(t, short, "no request used a token expiring before its operation's deadline plus skew")
	assert.GreaterOrEqual(t, issued-issuedBefore, 1, "the reads crossed a token replacement")
	assert.GreaterOrEqual(t, insideMargin, 1, "a cycle started inside the renewal margin, before the source issued a new token")
	assert.GreaterOrEqual(t, readCount, 5)
	assert.LessOrEqual(t, requested-requestsBefore, 2*int(margin/time.Second)+4, "token requests stay bounded")
}

// TestFileArchiveURL keeps the archive in an explicit directory, apart from
// the working directory, and claims only an empty one.
func TestFileArchiveURL(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	ids := insertHotResults(t, ctx, pool, 2)
	occupied := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(occupied, "notes.txt"), []byte("unrelated"), 0o600))
	cfg := Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), ArchiveURL: "file://" + occupied,
		Readers: 1, Settle: time.Nanosecond, Retention: immediateRetention}
	_, err := Open(ctx, pool, cfg)
	require.ErrorContains(t, err, "directory is not empty and has no BenchDB dataset file")
	assert.NoFileExists(t, filepath.Join(occupied, datasetMarker))

	root := filepath.Join(t.TempDir(), "archive")
	cfg.ArchiveURL = "file://" + root
	e, err := Open(ctx, pool, cfg)
	require.NoError(t, err)
	published := exportSettled(t, ctx, e, pool)
	require.Len(t, published, 1)
	assert.FileExists(t, filepath.Join(root, published[0]))
	assert.NoFileExists(t, filepath.Join(cfg.Dir, published[0]))
	require.NoError(t, e.Close())

	// Another working directory reads the same archive.
	cfg.Dir = t.TempDir()
	e, err = Open(ctx, pool, cfg)
	require.NoError(t, err)
	assert.Equal(t, ids, visibleIDs(t, ctx, e))
	require.NoError(t, e.Close())

	// Staging recovery walks the configured root, not the working directory.
	leftover := filepath.Join(root, "data", stagingPrefix+"crashed")
	writeStagingFile(t, leftover, 5)
	result, err := RecoverStaging(ctx, pool, cfg)
	require.NoError(t, err)
	assert.Equal(t, StagingRecovery{Files: 1, Bytes: 5}, result)
	assert.NoFileExists(t, leftover)
}

// fixedToken issues one bearer token that outlives any test.
type fixedToken string

func (f fixedToken) Token(context.Context) (*auth.Token, error) {
	return &auth.Token{Value: string(f), Expiry: time.Now().Add(time.Hour)}, nil
}

// TestPackagedExtensionsLoadOffline loads the packaged DuckDB extensions
// without downloads, configures S3 and GCS archive secrets as engines do, and
// reads a Parquet object through each from a loopback service. The image
// build runs it with networking disabled against the image's extensions.
func TestPackagedExtensionsLoadOffline(t *testing.T) {
	const keyID = "fixture-packaged-key"
	dbtest.IssueAWSCredentials(t, keyID, "fixture-secret", time.Now().Add(time.Hour))
	dir := t.TempDir()
	handle, err := openDuckDB(os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, handle.Close()) })
	handle.SetMaxOpenConns(1)
	_, err = handle.ExecContext(t.Context(), "LOAD postgres")
	require.NoError(t, err)
	confineDuckDB(t, handle)
	_, err = handle.ExecContext(t.Context(), "COPY (SELECT range AS n FROM range(5)) TO "+literal(filepath.Join(dir, "facts.parquet"))+" (FORMAT PARQUET)")
	require.NoError(t, err)

	var mu sync.Mutex
	authorizations := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		mu.Unlock()
		if path.Base(r.URL.Path) != "facts.parquet" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(dir, "facts.parquet"))
	}))
	t.Cleanup(server.Close)

	s3 := &Engine{location: archiveLocation{backend: backendS3, bucket: "packaged", prefix: "archive/", endpoint: server.URL, region: "us-east-1", pathStyle: true}}
	gcs := &Engine{location: archiveLocation{backend: backendGCS, bucket: "packaged", prefix: "archive/"}, gcsEndpoint: server.Listener.Addr().String(),
		tokens: fixedToken("fixture-bearer"), tokenMargin: time.Minute, tokenFetchTimeout: time.Minute, lifetime: t.Context(), handleTokens: map[*sql.DB]*handleToken{}}
	gcs.handleTokens[handle] = &handleToken{e: gcs, ready: true}
	for _, e := range []*Engine{s3, gcs} {
		require.NoError(t, e.remoteExtensions(t.Context(), handle))
		require.NoError(t, e.renewArchiveToken(t.Context(), handle))
		var rows int
		require.NoError(t, handle.QueryRowContext(t.Context(), "SELECT count(*) FROM read_parquet("+literal(e.location.objectURL("data/facts.parquet"))+")").Scan(&rows))
		assert.Equal(t, 5, rows, e.location.backend)
	}
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, authorizations)
	assert.Contains(t, authorizations[0], "Credential="+keyID+"/", "the AWS credential chain signs S3 reads")
	assert.Equal(t, "Bearer fixture-bearer", authorizations[len(authorizations)-1], "GCS reads carry the bearer token")
}

func activeBinding(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (backend, namespace string, claimed bool) {
	t.Helper()
	require.NoError(t, pool.QueryRow(ctx, `SELECT backend,namespace,claimed_at IS NOT NULL FROM archive_storage WHERE active`).Scan(&backend, &namespace, &claimed))
	return backend, namespace, claimed
}

// TestArchiveBindingNeverSplits starts a process with other archive settings
// while a remote engine is open, then publishes from the first engine. The
// second process is refused, the first keeps publishing to the one bound
// archive, and once the binding moves anyway, the first engine stops
// publishing instead of writing into storage the database no longer reads.
func TestArchiveBindingNeverSplits(t *testing.T) {
	r, ctx := newRemoteArchive(t)
	insertHotResults(t, ctx, r.pool, 2)
	e := r.open(t, ctx, r.config("shared", t.TempDir()))

	extensions := os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR")
	for name, cfg := range map[string]Config{
		"local":  {Dir: t.TempDir(), ExtensionDir: extensions},
		"file":   {Dir: t.TempDir(), ExtensionDir: extensions, ArchiveURL: "file://" + t.TempDir()},
		"prefix": r.config("other", t.TempDir()),
	} {
		_, err := r.start(t, ctx, cfg)
		require.ErrorContains(t, err, "the archive database is bound to s3://"+r.s3.Bucket+"/shared/", name)
		require.ErrorContains(t, err, "Give every server and admin process the BENCHDB_ARCHIVE_* settings", name)
	}
	backend, namespace, claimed := activeBinding(t, ctx, r.pool)
	assert.Equal(t, []any{backendS3, r.s3.Bucket + "/shared/", true}, []any{backend, namespace, claimed})

	published := exportSettled(t, ctx, e, r.pool)
	require.NotEmpty(t, published)
	assert.Subset(t, r.keys(t, ctx, "shared"), published)
	assert.Zero(t, hotRows(t, ctx, r.pool))

	// Move the binding behind the running engine, as a manual change would.
	_, err := r.pool.Exec(ctx, `BEGIN; UPDATE archive_storage SET active=false WHERE active; UPDATE archive_storage SET active=true WHERE backend='file'; COMMIT`)
	require.NoError(t, err)
	insertHotResults(t, ctx, r.pool, 1)
	before := manifestPaths(t, ctx, r.pool)
	_, err = e.Maintain(ctx)
	require.ErrorIs(t, err, errStorageMoved)
	assert.Equal(t, before, manifestPaths(t, ctx, r.pool), "nothing is published after the binding moved")
	assert.EqualValues(t, 1, hotRows(t, ctx, r.pool))
	// Publication and cleanup units refuse too, even with nothing to upload.
	require.NoError(t, e.withBuilder(ctx, func(owner *maintenanceOwner) error {
		_, _, acquired, err := e.openPublication(ctx, owner)
		assert.False(t, acquired)
		assert.ErrorIs(t, err, errStorageMoved, "publication")
		_, acquired, err = e.beginResolved(ctx, owner)
		assert.False(t, acquired)
		assert.ErrorIs(t, err, errStorageMoved, "cleanup")
		return nil
	}))
	conn, err := r.pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	// An upload intent that is valid apart from the moved binding is refused.
	cycle := uuid.New()
	require.NoError(t, e.archive.startCycle(ctx, conn.Conn(), cycle, "test"))
	err = e.archive.recordUpload(ctx, conn.Conn(), uploadIntent{CycleID: cycle, Key: "data/moved.parquet", Size: 1, SHA256: sha256.Sum256([]byte("x"))})
	require.ErrorIs(t, err, errStorageMoved, "no upload intent is recorded either")
	var uploads int
	require.NoError(t, r.pool.QueryRow(ctx, `SELECT count(*) FROM archive_upload WHERE cycle_id=$1`, cycle).Scan(&uploads))
	assert.Zero(t, uploads)
}

// TestOverlappingFirstRemoteStartups releases two first startups of one
// remote archive together. Each waits for the binding lock without holding a
// reader guard, then they claim one at a time and share one binding.
func TestOverlappingFirstRemoteStartups(t *testing.T) {
	r, ctx := newRemoteArchive(t)
	holder, err := r.pool.Acquire(ctx)
	require.NoError(t, err)
	defer holder.Release()
	_, err = holder.Exec(ctx, `SELECT pg_advisory_lock($1)`, bindingLock)
	require.NoError(t, err)

	engines := make([]*Engine, 2)
	errs := make([]error, len(engines))
	var wg sync.WaitGroup
	for i := range engines {
		cfg := r.config("shared", t.TempDir())
		cfg.dialArchive = loopbackDial(t)
		wg.Go(func() { engines[i], errs[i] = Open(ctx, r.pool, cfg) })
	}
	locks := func(key int64, mode string, granted bool) int {
		var n int
		require.NoError(t, r.pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND mode=$2 AND granted=$3
AND database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND (classid::bigint<<32|objid::bigint)=$1`, key, mode, granted).Scan(&n))
		return n
	}
	require.Eventually(t, func() bool { return locks(bindingLock, "ExclusiveLock", false) == 2 }, time.Minute, 10*time.Millisecond)
	assert.Zero(t, locks(reclamationLock, "ShareLock", true), "a startup waiting to claim holds no reader guard")
	_, err = holder.Exec(ctx, `SELECT pg_advisory_unlock($1)`, bindingLock)
	require.NoError(t, err)
	wg.Wait()
	for i, e := range engines {
		require.NoError(t, errs[i])
		t.Cleanup(func() { require.NoError(t, e.Close()) })
	}
	assert.Equal(t, engines[0].archive.storageID, engines[1].archive.storageID)
	backend, namespace, claimed := activeBinding(t, ctx, r.pool)
	assert.Equal(t, []any{backendS3, r.s3.Bucket + "/shared/", true}, []any{backend, namespace, claimed})
}

// TestClaimedLocalArchiveRefusesRemoteURL shows that a local archive that an
// engine has opened is never moved to a remote URL, even before it holds
// any archive state.
func TestClaimedLocalArchiveRefusesRemoteURL(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	dbtest.IssueAWSCredentials(t, "fixture-unused", "fixture-unused", time.Now().Add(time.Hour))
	local, err := startConfined(t, ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1})
	require.NoError(t, err)
	_, _, claimed := activeBinding(t, ctx, pool)
	require.True(t, claimed)

	_, err = startConfined(t, ctx, pool, refusedRemoteConfig(t))
	require.ErrorContains(t, err, "the archive database is bound to a local archive directory")
	backend, _, _ := activeBinding(t, ctx, pool)
	assert.Equal(t, backendFile, backend)
	assert.Empty(t, visibleIDs(t, ctx, local), "the local engine still reads its archive")
}

// refusedRemoteConfig selects an S3 archive whose endpoint refuses
// connections, for startups that must be refused before any request.
func refusedRemoteConfig(t *testing.T) Config {
	return Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1,
		ArchiveURL: "s3://benchdb-archive/refused", S3Endpoint: "http://" + refusingAddress(t), S3URLStyle: "path"}
}

// TestRebindRefusesArchiveState shows that a retirement or an orphan alone
// keeps a new database's local binding, because objects may still exist in
// that storage, and that a remote binding is never moved, even unclaimed.
func TestRebindRefusesArchiveState(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	dbtest.IssueAWSCredentials(t, "fixture-unused", "fixture-unused", time.Now().Add(time.Hour))
	for _, tc := range []struct {
		name, setup, bound string
	}{
		{"retirement", `INSERT INTO archive_retirement(storage_id,key) SELECT id,'data/retired.parquet' FROM archive_storage WHERE active`,
			"a local archive directory"},
		{"orphan", `INSERT INTO archive_orphan(storage_id,key,first_observed_at,delete_after)
SELECT id,'data/orphan.parquet',now(),now()+interval '1 day' FROM archive_storage WHERE active`, "a local archive directory"},
		{"unclaimed remote", `BEGIN; UPDATE archive_storage SET active=false WHERE active;
INSERT INTO archive_storage(dataset_id,backend,namespace,active) SELECT id,'gs','other-bucket/archive/',true FROM archive_dataset; COMMIT`,
			"gs://other-bucket/archive/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, _, _ := activeBinding(t, ctx, pool)
			_, err := pool.Exec(ctx, tc.setup)
			require.NoError(t, err)
			t.Cleanup(func() {
				_, err := pool.Exec(context.WithoutCancel(ctx), `BEGIN; DELETE FROM archive_retirement; DELETE FROM archive_orphan;
UPDATE archive_storage SET active=false WHERE active; UPDATE archive_storage SET active=true WHERE backend='file';
DELETE FROM archive_storage WHERE backend<>'file'; COMMIT`)
				require.NoError(t, err)
			})
			_, err = startConfined(t, ctx, pool, refusedRemoteConfig(t))
			require.ErrorContains(t, err, "the archive database is bound to "+tc.bound)
			after, _, claimed := activeBinding(t, ctx, pool)
			assert.NotEqual(t, backendS3, after)
			assert.False(t, claimed)
			assert.Equal(t, backendFile, before)
		})
	}
}

// sequenceTokens issues the next value on each request, valid for an hour.
type sequenceTokens struct {
	mu     sync.Mutex
	values []string
}

func (s *sequenceTokens) Token(context.Context) (*auth.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := s.values[0]
	if len(s.values) > 1 {
		s.values = s.values[1:]
	}
	return &auth.Token{Value: value, Expiry: time.Now().Add(time.Hour)}, nil
}

// TestBuilderTokenSurvivesRollback replaces the builder's GCS secret inside
// a transaction that rolls back, which also undoes the replacement. The
// next statement installs the token again instead of trusting the record.
func TestBuilderTokenSurvivesRollback(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	bearers := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		bearers = append(bearers, r.Header.Get("Authorization"))
		mu.Unlock()
		http.ServeFile(w, r, filepath.Join(dir, "facts.parquet"))
	}))
	t.Cleanup(server.Close)
	// Every token is inside the two-hour margin and the refetch interval is
	// zero, so each statement asks for the next one.
	e := &Engine{location: archiveLocation{backend: backendGCS, bucket: "builder", prefix: "archive/"}, gcsEndpoint: server.Listener.Addr().String(),
		tokens: &sequenceTokens{values: []string{"token-a", "token-b"}}, tokenMargin: 2 * time.Hour, tokenSkew: time.Second,
		tokenFetchTimeout: 10 * time.Second, lifetime: t.Context(), handleTokens: map[*sql.DB]*handleToken{}}
	guard := &handleToken{e: e}
	handle, err := openDuckDB(os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), guard)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, handle.Close()) })
	handle.SetMaxOpenConns(1)
	confineDuckDB(t, handle)
	_, err = handle.ExecContext(t.Context(), "COPY (SELECT 1 AS n) TO "+literal(filepath.Join(dir, "facts.parquet"))+" (FORMAT PARQUET)")
	require.NoError(t, err)
	guard.ready = true
	count := func(q queryer) {
		var n int
		require.NoError(t, q.QueryRowContext(t.Context(), "SELECT count(*) FROM read_parquet("+literal(e.location.objectURL("data/facts.parquet"))+")").Scan(&n))
	}
	last := func() string {
		mu.Lock()
		defer mu.Unlock()
		return bearers[len(bearers)-1]
	}

	count(handle)
	require.Equal(t, "Bearer token-a", last())
	// The next statement asks for token-b in the background and keeps
	// token-a, which clears its floor.
	count(handle)
	awaitToken(t, e, "token-b")
	tx, err := handle.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	count(tx)
	require.Equal(t, "Bearer token-b", last())
	require.NoError(t, tx.Rollback())
	count(handle)
	assert.Equal(t, "Bearer token-b", last(), "the rolled-back secret is installed again")
}

// stallingTokens issues numbered tokens valid for an hour. While stalled, a
// request waits for release or for its context, like an unresponsive token
// endpoint.
type stallingTokens struct {
	mu        sync.Mutex
	stalled   chan struct{}
	requests  int
	abandoned int
}

func (s *stallingTokens) Token(ctx context.Context) (*auth.Token, error) {
	s.mu.Lock()
	s.requests++
	n, stalled := s.requests, s.stalled
	s.mu.Unlock()
	if stalled != nil {
		select {
		case <-stalled:
		case <-ctx.Done():
			s.mu.Lock()
			s.abandoned++
			s.mu.Unlock()
			return nil, ctx.Err()
		}
	}
	return &auth.Token{Value: fmt.Sprintf("token-%d", n), Expiry: time.Now().Add(time.Hour)}, nil
}

func (s *stallingTokens) stall() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stalled = make(chan struct{})
}

func (s *stallingTokens) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.stalled)
	s.stalled = nil
}

func (s *stallingTokens) counts() (requests, abandoned int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests, s.abandoned
}

// TestGCSTokenFetchStallKeepsReadsBounded stalls the token source while a
// builder statement asks it for a newer token. A read whose cached token
// clears its floor finishes inside its deadline; a read that needs a new
// token returns when its deadline expires; the stalled request ends at its
// own timeout, not the builder's unit deadline.
func TestGCSTokenFetchStallKeepsReadsBounded(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	bearers := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		bearers = append(bearers, r.Header.Get("Authorization"))
		mu.Unlock()
		http.ServeFile(w, r, filepath.Join(dir, "facts.parquet"))
	}))
	t.Cleanup(server.Close)
	tokens := &stallingTokens{}
	// Every token is inside the two-hour margin and the refetch interval is
	// zero, so each statement and read wants a newer token.
	e := &Engine{cfg: Config{ReadTimeout: time.Second}, location: archiveLocation{backend: backendGCS, bucket: "stall", prefix: "archive/"},
		gcsEndpoint: server.Listener.Addr().String(), tokens: tokens, tokenMargin: 2 * time.Hour, tokenSkew: time.Second,
		tokenFetchTimeout: 10 * time.Second, lifetime: t.Context(), handleTokens: map[*sql.DB]*handleToken{}}
	builderToken := &handleToken{e: e}
	builder, err := openDuckDB(os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), builderToken)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, builder.Close()) })
	reader, err := openDuckDB(os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	for _, handle := range []*sql.DB{builder, reader} {
		handle.SetMaxOpenConns(1)
		confineDuckDB(t, handle)
	}
	_, err = reader.ExecContext(t.Context(), "COPY (SELECT 1 AS n) TO "+literal(filepath.Join(dir, "facts.parquet"))+" (FORMAT PARQUET)")
	require.NoError(t, err)
	builderToken.ready = true
	e.handleTokens[reader] = &handleToken{e: e, ready: true}
	count := "SELECT count(*) FROM read_parquet(" + literal(e.location.objectURL("data/facts.parquet")) + ")"
	read := func(timeout time.Duration) (time.Duration, error) {
		ctx, cancel := context.WithTimeout(t.Context(), timeout)
		defer cancel()
		start := time.Now()
		err := e.renewArchiveToken(ctx, reader)
		if err == nil {
			var n int
			err = reader.QueryRowContext(ctx, count).Scan(&n)
		}
		return time.Since(start), err
	}
	_, err = read(10 * time.Second)
	require.NoError(t, err)

	// A builder statement with a long unit deadline asks the stalled source
	// for a newer token.
	tokens.stall()
	built := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		var n int
		built <- builder.QueryRowContext(ctx, count).Scan(&n)
	}()
	require.Eventually(t, func() bool {
		requests, _ := tokens.counts()
		return requests == 2
	}, 10*time.Second, 10*time.Millisecond, "the builder asks for a newer token")

	// The cached token clears the read's floor, so the read neither waits
	// for the stalled request nor starts another.
	elapsed, err := read(2 * time.Second)
	require.NoError(t, err)
	assert.Less(t, elapsed, 2*time.Second)
	select {
	case err := <-built:
		require.NoError(t, err, "the builder statement uses its cached token")
	case <-time.After(20 * time.Second):
		stacktest.Dump(t)
		require.Fail(t, "the builder statement waited for the stalled token request")
	}

	// A read whose cached token has expired needs the stalled request, and
	// waits for it only until its own deadline.
	e.tokenMu.Lock()
	e.token = &auth.Token{Value: e.token.Value, Expiry: time.Now()}
	e.tokenMu.Unlock()
	elapsed, err = read(time.Second)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, elapsed, 3*time.Second)

	// The stalled request ends at its own timeout, and the next read asks
	// again.
	require.Eventually(t, func() bool {
		_, abandoned := tokens.counts()
		return abandoned == 1
	}, 20*time.Second, 10*time.Millisecond, "the stalled request ends at the token fetch timeout")
	tokens.release()
	_, err = read(10 * time.Second)
	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	assert.NotEqual(t, "Bearer token-1", bearers[len(bearers)-1], "the read uses the new token")
}

// TestCloseCancelsTokenFetch shows that Close ends a GCS token request in
// flight instead of leaving it to run until its own timeout.
func TestCloseCancelsTokenFetch(t *testing.T) {
	tokens := &stallingTokens{}
	tokens.stall()
	e := &Engine{tokens: tokens, tokenMargin: time.Hour, tokenFetchTimeout: time.Minute, handleTokens: map[*sql.DB]*handleToken{}}
	e.lifetime, e.endLifetime = context.WithCancel(context.Background())
	ctx, cancel := context.WithCancel(t.Context())
	waited := make(chan error, 1)
	go func() {
		_, err := e.currentToken(ctx, time.Minute)
		waited <- err
	}()
	require.Eventually(t, func() bool {
		requests, _ := tokens.counts()
		return requests == 1
	}, 5*time.Second, 10*time.Millisecond, "the caller starts a token request")
	cancel()
	require.ErrorIs(t, <-waited, context.Canceled, "the caller stops waiting when its context ends")
	_, abandoned := tokens.counts()
	require.Zero(t, abandoned, "the request outlives the caller")
	require.NoError(t, e.Close())
	assert.Eventually(t, func() bool {
		_, abandoned := tokens.counts()
		return abandoned == 1
	}, 5*time.Second, 10*time.Millisecond, "Close ends the token request")
}

// awaitToken waits until the engine's current token is value, which a
// background request installs.
func awaitToken(t *testing.T, e *Engine, value string) {
	t.Helper()
	require.Eventually(t, func() bool {
		e.tokenMu.Lock()
		defer e.tokenMu.Unlock()
		return e.token != nil && e.token.Value == value
	}, 10*time.Second, 10*time.Millisecond)
}

// TestRemoteStorageStatus reports an S3 archive without a capacity: the
// archive volume is null, while the working directory and catalog cache are
// measured locally.
func TestRemoteStorageStatus(t *testing.T) {
	r, ctx := newRemoteArchive(t)
	e := r.open(t, ctx, r.config("status", t.TempDir()))
	cloudStatus, err := e.StorageStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, "s3", cloudStatus.Backend)
	assert.Nil(t, cloudStatus.VolumeTotalBytes)
	assert.Nil(t, cloudStatus.VolumeAvailableBytes)
	assert.Positive(t, cloudStatus.LocalVolumeTotalBytes)
	assert.EqualValues(t, defaultCatalogCacheBytes, cloudStatus.CacheBudgetBytes)
	assert.False(t, cloudStatus.ReclamationPaused)
}
