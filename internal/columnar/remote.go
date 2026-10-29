package columnar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"uuid"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.kenn.io/kit/objectstore"
)

// Backends as archive_storage records them.
const (
	backendFile = "file"
	backendS3   = "s3"
	backendGCS  = "gs"
)

const (
	// gcsStorageScope lets the archive credential read, create, and delete
	// objects without managing buckets or their policies.
	gcsStorageScope = "https://www.googleapis.com/auth/devstorage.read_write"
	// renewalPadding extends the GCS token renewal margin beyond the longest
	// DuckDB operation. Inside the margin, BenchDB starts asking for a newer
	// token.
	renewalPadding = 5 * time.Minute
	// tokenSkew is how long a token must outlive an operation's deadline.
	// Token expiry comes from a lifetime this host measures, so it covers
	// request latency and clock steps, not clock disagreement between hosts.
	// It stays below the five minutes before expiry when a metadata server
	// starts issuing a new token, so reads never wait for one.
	tokenSkew = time.Minute
	// tokenRefetch spaces requests for a newer token while the current one
	// is inside the renewal margin but outlives the operation.
	tokenRefetch = time.Minute
	// tokenFetchTimeout bounds one request for a GCS token, so a stalled
	// token endpoint cannot hold the engine's only request for a
	// maintenance unit's lifetime.
	tokenFetchTimeout = 30 * time.Second
	// remoteCheckTimeout bounds each startup request to the remote archive.
	remoteCheckTimeout = time.Minute
	// datasetMarker is the key that binds an archive root to one database.
	datasetMarker = "dataset"
	// archiveSecret names the DuckDB secret for the remote archive's scope.
	archiveSecret = "benchdb_archive"
	// readOnlyTransaction is PostgreSQL's read_only_sql_transaction code.
	readOnlyTransaction = "25006"
)

// neverExpires records a token that its source issued without an expiry.
var neverExpires = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// bucketName accepts the bucket names that S3 and GCS share.
var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,220}[a-z0-9]$`)

// archiveLocation is the configured archive root. A remote prefix is empty or
// ends in "/". It never holds credentials.
type archiveLocation struct {
	backend string
	// root is the local archive directory.
	root           string
	bucket, prefix string
	// endpoint is the normalized origin of an S3-compatible service; empty
	// selects AWS or GCS.
	endpoint  string
	region    string
	pathStyle bool
}

// parseArchiveLocation validates BENCHDB_ARCHIVE_URL and its provider
// settings. Without a URL, the working directory is the local archive.
// Messages never repeat the URL, which may contain mistyped credentials.
func parseArchiveLocation(cfg Config) (archiveLocation, error) {
	if cfg.ArchiveURL == "" {
		if err := rejectProviderSettings(cfg, ""); err != nil {
			return archiveLocation{}, err
		}
		return archiveLocation{backend: backendFile, root: cfg.Dir}, nil
	}
	u, err := url.Parse(cfg.ArchiveURL)
	if err != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return archiveLocation{}, errors.New("BENCHDB_ARCHIVE_URL must be a file://, s3://, or gs:// URL without a query or fragment")
	}
	if u.User != nil {
		return archiveLocation{}, errors.New("BENCHDB_ARCHIVE_URL must not contain credentials; S3 uses the AWS credential chain and GCS uses BENCHDB_ARCHIVE_GCS_CREDENTIALS_FILE")
	}
	if err := rejectProviderSettings(cfg, u.Scheme); err != nil {
		return archiveLocation{}, err
	}
	switch u.Scheme {
	case backendFile:
		if u.Host != "" || !filepath.IsAbs(u.Path) {
			return archiveLocation{}, errors.New("BENCHDB_ARCHIVE_URL file:// archives need an absolute local path, such as file:///var/lib/benchdb-archive")
		}
		return archiveLocation{backend: backendFile, root: filepath.Clean(u.Path)}, nil
	case backendS3, backendGCS:
		return parseBucketLocation(cfg, u)
	default:
		return archiveLocation{}, errors.New("BENCHDB_ARCHIVE_URL must use the file, s3, or gs scheme")
	}
}

// rejectProviderSettings fails when a setting belongs to another provider.
func rejectProviderSettings(cfg Config, scheme string) error {
	for _, setting := range []struct {
		name, value, scheme string
	}{
		{"BENCHDB_ARCHIVE_S3_ENDPOINT", cfg.S3Endpoint, backendS3},
		{"BENCHDB_ARCHIVE_S3_REGION", cfg.S3Region, backendS3},
		{"BENCHDB_ARCHIVE_S3_URL_STYLE", cfg.S3URLStyle, backendS3},
		{"BENCHDB_ARCHIVE_GCS_CREDENTIALS_FILE", cfg.GCSCredentialsFile, backendGCS},
	} {
		if setting.value != "" && scheme != setting.scheme {
			return fmt.Errorf("%s applies only to a %s:// BENCHDB_ARCHIVE_URL", setting.name, setting.scheme)
		}
	}
	return nil
}

func parseBucketLocation(cfg Config, u *url.URL) (archiveLocation, error) {
	if !bucketName.MatchString(u.Host) {
		return archiveLocation{}, errors.New("BENCHDB_ARCHIVE_URL must name a bucket of lowercase letters, digits, dots, hyphens, or underscores, without a port")
	}
	location := archiveLocation{backend: u.Scheme, bucket: u.Host}
	if prefix := strings.Trim(u.Path, "/"); prefix != "" {
		if err := objectstore.ValidatePrefix(prefix); err != nil {
			return archiveLocation{}, errors.New("BENCHDB_ARCHIVE_URL has an invalid object prefix")
		}
		location.prefix = prefix + "/"
	}
	if u.Scheme == backendGCS {
		return location, nil
	}
	location.region = cfg.S3Region
	if location.region == "" {
		location.region = "us-east-1"
	}
	switch cfg.S3URLStyle {
	case "", "vhost":
	case "path":
		location.pathStyle = true
	default:
		return archiveLocation{}, errors.New("BENCHDB_ARCHIVE_S3_URL_STYLE must be path or vhost")
	}
	if cfg.S3Endpoint != "" {
		endpoint, err := normalizeEndpoint(cfg.S3Endpoint)
		if err != nil {
			return archiveLocation{}, err
		}
		location.endpoint = endpoint
	}
	return location, nil
}

// normalizeEndpoint returns the lowercase origin of an S3-compatible service,
// without a default port, so equivalent spellings bind the same archive.
func normalizeEndpoint(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("BENCHDB_ARCHIVE_S3_ENDPOINT must be an http or https origin without credentials, path, query, or fragment")
	}
	host := strings.ToLower(u.Host)
	if port := u.Port(); (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		host = strings.ToLower(u.Hostname())
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
	}
	return u.Scheme + "://" + host, nil
}

func (l archiveLocation) remote() bool { return l.backend != backendFile }

// namespace is the binding archive_storage records: empty for a local
// archive, whose mount path may differ between processes, and otherwise the
// bucket and prefix.
func (l archiveLocation) namespace() string {
	if !l.remote() {
		return ""
	}
	return l.bucket + "/" + l.prefix
}

// objectURL is the location DuckDB reads for a validated key: a path under
// the local root, or the exact provider URL, never a pattern.
func (l archiveLocation) objectURL(key string) string {
	if !l.remote() {
		return filepath.Join(l.root, filepath.FromSlash(key))
	}
	return l.backend + "://" + l.bucket + "/" + l.prefix + key
}

// describeBinding names a binding in operator messages.
func describeBinding(backend, namespace, endpoint string) string {
	if backend == backendFile {
		return "a local archive directory"
	}
	described := backend + "://" + namespace
	if endpoint != "" {
		described += " at " + endpoint
	}
	return described
}

// googleCredentials builds the one OAuth credential that Kit and DuckDB share.
// Its cache refreshes a token, while callers wait, once the token expires
// within margin, so a token it returns outlives any operation the margin covers.
func googleCredentials(file string, margin time.Duration) (*auth.Credentials, error) {
	opts := &credentials.DetectOptions{Scopes: []string{gcsStorageScope}, EarlyTokenRefresh: margin, DisableAsyncRefresh: true}
	if file == "" {
		creds, err := credentials.DetectDefault(opts)
		if err != nil {
			return nil, fmt.Errorf("detect Google application default credentials for the GCS archive: %w", err)
		}
		return creds, nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read BENCHDB_ARCHIVE_GCS_CREDENTIALS_FILE: %w", err)
	}
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &header); err != nil || header.Type == "" {
		return nil, errors.New("BENCHDB_ARCHIVE_GCS_CREDENTIALS_FILE must be a Google credentials JSON file with a type")
	}
	creds, err := credentials.NewCredentialsFromJSON(credentials.CredType(header.Type), data, opts)
	if err != nil {
		return nil, fmt.Errorf("load BENCHDB_ARCHIVE_GCS_CREDENTIALS_FILE: %w", err)
	}
	return creds, nil
}

// remoteExtensions loads DuckDB's remote file support and, for S3, a secret
// that resolves the AWS credential chain itself. REFRESH auto makes DuckDB
// resolve the chain again when the provider rejects expired credentials. GCS
// secrets carry a bearer token instead; see renewArchiveToken.
func (e *Engine) remoteExtensions(ctx context.Context, handle *sql.DB) error {
	if !e.location.remote() {
		return nil
	}
	statements := []string{"LOAD httpfs"}
	if e.location.backend == backendS3 {
		statements = append(statements, "LOAD aws", e.s3Secret())
	}
	for _, statement := range statements {
		if _, err := handle.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure DuckDB remote archive access (prepare its httpfs and aws extensions and AWS credentials): %w", err)
		}
	}
	return nil
}

// attachProxy sends the handle's remote requests through the engine's proxy
// under the handle's own credentials. A handle without a proxy, which only
// tests build, keeps DuckDB's direct connections.
func (e *Engine) attachProxy(ctx context.Context, handle *sql.DB) error {
	if e.proxy == nil {
		return nil
	}
	client, err := e.proxy.NewClient()
	if err != nil {
		return fmt.Errorf("register a DuckDB handle with the archive proxy: %w", err)
	}
	proxy := client.URL()
	password, _ := proxy.User.Password()
	for _, statement := range []string{
		"SET GLOBAL http_proxy=" + literal(proxy.Host),
		"SET GLOBAL http_proxy_username=" + literal(proxy.User.Username()),
		"SET GLOBAL http_proxy_password=" + literal(password),
	} {
		if _, err := handle.ExecContext(ctx, statement); err != nil {
			// DuckDB's error can repeat the statement, which holds the password.
			err = errors.New("route DuckDB remote archive requests through the archive proxy")
			if ctx.Err() != nil {
				err = fmt.Errorf("route DuckDB remote archive requests through the archive proxy: %w", ctx.Err())
			}
			return errors.Join(err, client.Close())
		}
	}
	e.proxyClients[handle] = client
	return nil
}

// bindProxy lets the handle's remote requests run until ctx ends, when the
// proxy closes their connections. Call stop after the handle's DuckDB work
// has ended, before the handle's next binding.
func (e *Engine) bindProxy(ctx context.Context, handle *sql.DB) (stop func(), err error) {
	client := e.proxyClients[handle]
	if client == nil {
		return func() {}, nil
	}
	return client.Bind(ctx)
}

// duckdbUpstreams lists the host:port addresses DuckDB's httpfs contacts for
// this archive, the only addresses its proxy reaches: the S3 endpoint, with
// the bucket's virtual host unless URL style is path, or Google's storage
// endpoint.
func (l archiveLocation) duckdbUpstreams() ([]string, error) {
	if l.backend == backendGCS {
		return []string{"storage.googleapis.com:443"}, nil
	}
	host, port := l.awsEndpoint(), "443"
	if l.endpoint != "" {
		u, err := url.Parse(l.endpoint)
		if err != nil {
			return nil, fmt.Errorf("parse the S3 endpoint: %w", err)
		}
		host, port = u.Hostname(), u.Port()
		if port == "" {
			port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
		}
	}
	upstreams := []string{net.JoinHostPort(host, port)}
	if !l.pathStyle {
		upstreams = append(upstreams, net.JoinHostPort(l.bucket+"."+host, port))
	}
	return upstreams, nil
}

// awsEndpoint is the regional AWS S3 endpoint. Naming it in DuckDB's secret
// fixes the host the archive proxy must allow. China regions are a separate
// AWS partition with their own domain.
func (l archiveLocation) awsEndpoint() string {
	domain := ".amazonaws.com"
	if strings.HasPrefix(l.region, "cn-") {
		domain += ".cn"
	}
	return "s3." + l.region + domain
}

func (e *Engine) s3Secret() string {
	l := e.location
	style := "vhost"
	if l.pathStyle {
		style = "path"
	}
	options := []string{"TYPE s3", "PROVIDER credential_chain", "REFRESH auto",
		"SCOPE " + literal("s3://"+l.bucket+"/"+l.prefix), "REGION " + literal(l.region), "URL_STYLE " + literal(style)}
	if l.endpoint != "" {
		host, tls := strings.CutPrefix(l.endpoint, "https://")
		if !tls {
			host = strings.TrimPrefix(l.endpoint, "http://")
		}
		options = append(options, "ENDPOINT "+literal(host), fmt.Sprintf("USE_SSL %t", tls))
	} else {
		options = append(options, "ENDPOINT "+literal(l.awsEndpoint()))
	}
	return "CREATE OR REPLACE SECRET " + archiveSecret + " (" + strings.Join(options, ", ") + ")"
}

// handleToken tracks the bearer token in one DuckDB handle's GCS secret.
// Only the handle's current holder uses it. As the builder's statement
// guard, it checks the token before each builder statement against the
// same floor as a read: the read deadline plus skew. A maintenance unit can
// outlast its token; a statement that does fails its unit, and the next
// cycle retries it. Requiring the whole unit deadline instead would fail
// units up front whenever a metadata server's token is between five and
// sixteen minutes from expiry.
type handleToken struct {
	e *Engine
	// value is the token the secret holds; empty forces the next check to
	// install one.
	value string
	// ready is set once the handle has loaded httpfs.
	ready bool
}

func (h *handleToken) beforeStatement(ctx context.Context, exec func(context.Context, string) error) error {
	if h.e.tokens == nil || !h.ready {
		return nil
	}
	return h.e.installToken(ctx, h, h.e.cfg.ReadTimeout+h.e.tokenSkew, exec)
}

// rolledBack forgets the installed token: a rollback or failed commit also
// undoes a secret that a statement in the transaction replaced.
func (h *handleToken) rolledBack() { h.value = "" }

// tokenNeed is how long a token must remain valid for a read bounded by
// ctx: its remaining deadline plus clock skew.
func (e *Engine) tokenNeed(ctx context.Context) time.Duration {
	need := e.tokenSkew
	if deadline, ok := ctx.Deadline(); ok {
		need += time.Until(deadline)
	}
	return need
}

// renewArchiveToken makes handle's GCS secret hold a token that outlives the
// read bounded by ctx, before the read starts. The builder checks before
// each statement; see handleToken. The caller holds handle exclusively.
func (e *Engine) renewArchiveToken(ctx context.Context, handle *sql.DB) error {
	h := e.handleTokens[handle]
	if e.tokens == nil || h == nil || !h.ready {
		return nil
	}
	return e.installToken(ctx, h, e.tokenNeed(ctx), func(ctx context.Context, query string) error {
		_, err := handle.ExecContext(ctx, query)
		return err
	})
}

// installToken replaces h's secret when the engine's current token differs
// from the one it holds.
func (e *Engine) installToken(ctx context.Context, h *handleToken, need time.Duration, exec func(context.Context, string) error) error {
	token, err := e.currentToken(ctx, need)
	if err != nil {
		return err
	}
	if h.value == token.Value {
		return nil
	}
	options := []string{"TYPE gcs", "BEARER_TOKEN " + literal(token.Value), "SCOPE " + literal("gs://"+e.location.bucket+"/"+e.location.prefix)}
	if e.gcsEndpoint != "" {
		options = append(options, "ENDPOINT "+literal(e.gcsEndpoint), "URL_STYLE 'path'", "USE_SSL false")
	}
	if err := exec(ctx, "CREATE OR REPLACE SECRET "+archiveSecret+" ("+strings.Join(options, ", ")+")"); err != nil {
		// DuckDB's error can repeat the statement, which holds the token.
		if ctx.Err() != nil {
			return fmt.Errorf("configure DuckDB GCS archive credentials: %w", ctx.Err())
		}
		return errors.New("configure DuckDB GCS archive credentials")
	}
	h.value = token.Value
	return nil
}

// currentToken returns a token that remains valid for need. A token outside
// the renewal margin is used as it is. Inside the margin, BenchDB asks the
// source for a newer one in the background, at most once per tokenRefetch,
// and uses the cached token while it outlives need: a metadata server
// returns the same token until a few minutes before it expires. Only a
// caller whose cached token cannot outlive need waits for a request, and
// only until its own deadline. A token that cannot outlive need after
// renewal fails the operation.
func (e *Engine) currentToken(ctx context.Context, need time.Duration) (*auth.Token, error) {
	e.tokenMu.Lock()
	current, fetch := e.token, e.tokenFetch
	if current != nil && time.Until(current.Expiry) > need {
		if time.Until(current.Expiry) <= e.tokenMargin && fetch == nil && time.Since(e.tokenFetched) >= e.tokenRefetch {
			e.startTokenFetch()
		}
		e.tokenMu.Unlock()
		return current, nil
	}
	if fetch == nil {
		fetch = e.startTokenFetch()
	}
	e.tokenMu.Unlock()
	select {
	case <-fetch:
	case <-ctx.Done():
		return nil, fmt.Errorf("renew the GCS archive access token: %w", ctx.Err())
	}
	e.tokenMu.Lock()
	current, err := e.token, e.tokenErr
	e.tokenMu.Unlock()
	if current != nil && time.Until(current.Expiry) > need {
		return current, nil
	}
	if err != nil {
		return nil, fmt.Errorf("renew the GCS archive access token: %w", err)
	}
	remaining := time.Duration(0)
	if current != nil {
		remaining = time.Until(current.Expiry)
	}
	return nil, fmt.Errorf("the GCS archive access token expires in %s, before this operation's deadline and %s clock skew", remaining.Round(time.Second), e.tokenSkew)
}

// startTokenFetch requests a new token in the background, bounded by
// tokenFetchTimeout and the engine's lifetime rather than by any caller's
// deadline, and returns a channel closed when the request ends. The caller
// holds tokenMu.
func (e *Engine) startTokenFetch() chan struct{} {
	fetch := make(chan struct{})
	e.tokenFetch, e.tokenFetched = fetch, time.Now()
	go func() {
		ctx, cancel := context.WithTimeout(e.lifetime, e.tokenFetchTimeout)
		defer cancel()
		fetched, err := e.tokens.Token(ctx)
		e.tokenMu.Lock()
		defer e.tokenMu.Unlock()
		e.tokenFetch, e.tokenErr = nil, err
		if err == nil {
			e.token = &auth.Token{Value: fetched.Value, Expiry: fetched.Expiry}
			if e.token.Expiry.IsZero() {
				e.token.Expiry = neverExpires
			}
		} else if e.lifetime.Err() == nil {
			slog.Warn("renew the GCS archive access token", "error", err)
		}
		close(fetch)
	}()
	return fetch
}

// claimNamespace binds a remote archive root to the database. A root without
// a marker is claimed only while its namespace is empty, by publishing the
// marker without replacing an existing key, so a mistyped URL cannot adopt or
// mix with unrelated objects. Startup checks the marker, not every object.
func (e *Engine) claimNamespace(ctx context.Context, id string, used bool) error {
	ctx, cancel := context.WithTimeout(ctx, remoteCheckTimeout)
	defer cancel()
	claimed, err := e.readMarker(ctx)
	if err == nil {
		return checkMarker(claimed, id)
	}
	if !errors.Is(err, objectstore.ErrNotFound) {
		return err
	}
	if used {
		return errors.New("the archive database references objects, but the configured archive namespace has no dataset marker; check BENCHDB_ARCHIVE_URL and its endpoint")
	}
	page, err := e.objects.ListPage(ctx, "", "", 1)
	if err != nil {
		return fmt.Errorf("list the configured archive namespace: %w", err)
	}
	// A concurrent first startup of this database can publish the marker after
	// the first read, so it is read again before the namespace is refused.
	if len(page.Objects) > 0 {
		claimed, err := e.readMarker(ctx)
		if err == nil {
			return checkMarker(claimed, id)
		}
		if !errors.Is(err, objectstore.ErrNotFound) {
			return err
		}
		return errors.New("the configured archive namespace is not empty and has no BenchDB dataset marker; choose an empty bucket prefix in BENCHDB_ARCHIVE_URL")
	}
	marker := []byte(id + "\n")
	_, err = e.objects.Publish(ctx, datasetMarker, bytes.NewReader(marker), int64(len(marker)), sha256.Sum256(marker))
	if errors.Is(err, objectstore.ErrExists) {
		// Another database claimed the namespace first.
		return errors.New("archive namespace belongs to a different database")
	}
	if err != nil {
		return fmt.Errorf("publish the archive dataset marker: %w", err)
	}
	return nil
}

func (e *Engine) readMarker(ctx context.Context) (string, error) {
	return readSmallObject(ctx, e.objects, datasetMarker)
}

// readSmallObject reads a marker object of at most 128 bytes at its observed
// revision. A missing object returns objectstore.ErrNotFound unwrapped.
func readSmallObject(ctx context.Context, objects archiveObjects, key string) (string, error) {
	meta, err := objects.Inspect(ctx, key)
	if err != nil {
		if errors.Is(err, objectstore.ErrNotFound) {
			return "", err
		}
		return "", fmt.Errorf("inspect the archive %s marker: %w", key, err)
	}
	if meta.Size <= 0 || meta.Size > 128 {
		return "", fmt.Errorf("archive namespace has an invalid %s marker", key)
	}
	body, err := objects.OpenRange(ctx, key, meta.Revision, 0, meta.Size)
	if err != nil {
		return "", fmt.Errorf("read the archive %s marker: %w", key, err)
	}
	data, readErr := io.ReadAll(io.LimitReader(body, meta.Size+1))
	if err := errors.Join(readErr, body.Close()); err != nil {
		return "", fmt.Errorf("read the archive %s marker: %w", key, err)
	}
	if int64(len(data)) != meta.Size {
		return "", fmt.Errorf("archive %s marker changed while it was read", key)
	}
	return string(data), nil
}

func checkMarker(claimed, id string) error {
	if strings.TrimSpace(claimed) != id {
		return errors.New("archive namespace belongs to a different database")
	}
	return nil
}

// storageBinding is an archive_storage row's identity. Claimed means an
// engine has bound the row to its archive root by the dataset marker.
type storageBinding struct {
	id                           uuid.UUID
	backend, namespace, endpoint string
	claimed                      bool
}

func (l archiveLocation) binds(b storageBinding) bool {
	return b.backend == l.backend && b.namespace == l.namespace() && b.endpoint == l.endpoint
}

func (b storageBinding) String() string { return describeBinding(b.backend, b.namespace, b.endpoint) }

// activeStorage reads the active binding. lock is empty or a row-locking
// clause for a transaction.
func activeStorage(ctx context.Context, q rowQueryer, lock string) (storageBinding, error) {
	var b storageBinding
	err := q.QueryRow(ctx, `SELECT id,backend,namespace,endpoint,claimed_at IS NOT NULL FROM archive_storage WHERE active`+lock).
		Scan(&b.id, &b.backend, &b.namespace, &b.endpoint, &b.claimed)
	if err != nil {
		return b, fmt.Errorf("read active archive storage: %w", err)
	}
	return b, nil
}

// errBindingChanged means another process moved the active binding while
// this one started.
var errBindingChanged = errors.New("the archive storage binding changed during startup; start the process again")

// checkRebind allows moving the active binding only away from the initial
// implicit local binding, before any engine claimed it and while it holds no
// archive state, as in a new database. Every other mismatch is refused: a
// running process may still publish to the bound storage, and changing the
// URL must never start a second archive beside the first.
func (e *Engine) checkRebind(ctx context.Context, q rowQueryer, active storageBinding) error {
	want := e.location
	refuse := fmt.Errorf("the archive database is bound to %s, but BENCHDB_ARCHIVE_URL selects %s. Give every server and admin process the BENCHDB_ARCHIVE_* settings the database uses; moving an archive needs the transfer procedure",
		active, describeBinding(want.backend, want.namespace(), want.endpoint))
	if active.backend != backendFile || active.claimed || !want.remote() {
		return refuse
	}
	used, err := storageUsed(ctx, q, active.id)
	if err != nil {
		return err
	}
	if used {
		return refuse
	}
	return nil
}

// rebindStorage makes the configured remote location the active binding,
// after its namespace is claimed, if the binding it replaces is still the
// one checkRebind allowed. Publication and upload intents share-lock the
// active row, so the move waits for any that are in flight.
func (e *Engine) rebindStorage(ctx context.Context, conn *pgxpool.Conn, replaced storageBinding) error {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("bind archive storage: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	active, err := activeStorage(ctx, tx, " FOR UPDATE")
	if err == nil && active != replaced && e.location.binds(active) {
		// A concurrent startup already bound this location.
		e.archive.storageID = active.id
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && active != replaced) {
		return errBindingChanged
	}
	if err != nil {
		return err
	}
	if err := e.checkRebind(ctx, tx, active); err != nil {
		return err
	}
	want := e.location
	if _, err := tx.Exec(ctx, `UPDATE archive_storage SET active=false WHERE id=$1`, active.id); err != nil {
		return fmt.Errorf("bind archive storage: %w", err)
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `INSERT INTO archive_storage(dataset_id,backend,namespace,endpoint,active,claimed_at)
SELECT id,$1,$2,$3,true,clock_timestamp() FROM archive_dataset WHERE singleton
ON CONFLICT (dataset_id,backend,namespace,endpoint) DO UPDATE SET active=true,claimed_at=coalesce(archive_storage.claimed_at,excluded.claimed_at)
RETURNING id`, want.backend, want.namespace(), want.endpoint).Scan(&id); err != nil {
		return fmt.Errorf("bind archive storage: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("bind archive storage: %w", err)
	}
	e.archive.storageID = id
	slog.Info("bound the archive database to its configured storage", "from", active.String(),
		"to", describeBinding(want.backend, want.namespace(), want.endpoint))
	return nil
}

// recordClaim marks the active binding claimed after this engine verified or
// published its marker. Read-only processes need an already claimed binding:
// otherwise a subsequent remote startup could move it underneath their reads.
func (e *Engine) recordClaim(ctx context.Context, conn *pgxpool.Conn, binding storageBinding) error {
	if binding.claimed {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	// Only an unclaimed row is updated. Another startup can claim the binding
	// first and start maintenance, which share-locks the row; updating it again
	// would wait for that maintenance to finish.
	tag, err := conn.Exec(ctx, `UPDATE archive_storage SET claimed_at=clock_timestamp() WHERE id=$1 AND active AND claimed_at IS NULL`, binding.id)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == readOnlyTransaction {
		return errors.New("the archive storage binding has not been claimed; start a writable BenchDB process before opening it read-only")
	}
	if err != nil {
		return fmt.Errorf("record the archive storage claim: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var claimed bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM archive_storage WHERE id=$1 AND active AND claimed_at IS NOT NULL)`, binding.id).Scan(&claimed); err != nil {
		return fmt.Errorf("read the archive storage claim: %w", err)
	}
	if !claimed {
		return errBindingChanged
	}
	return nil
}

// storageUsed reports whether the database has published, retired, queued,
// or attempted archive objects in storage id, or any manifest at all.
func storageUsed(ctx context.Context, q rowQueryer, id uuid.UUID) (bool, error) {
	var used bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM archive_file)
OR EXISTS(SELECT 1 FROM archive_object WHERE storage_id=$1)
OR EXISTS(SELECT 1 FROM archive_upload WHERE storage_id=$1)
OR EXISTS(SELECT 1 FROM archive_lookup_segment WHERE storage_id=$1)
OR EXISTS(SELECT 1 FROM archive_retirement WHERE storage_id=$1)
OR EXISTS(SELECT 1 FROM archive_orphan WHERE storage_id=$1)`, id).Scan(&used)
	if err != nil {
		return false, fmt.Errorf("read archive storage use: %w", err)
	}
	return used, nil
}
