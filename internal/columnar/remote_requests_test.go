package columnar

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
)

// s3Gateway fronts the S3 service for both Kit and DuckDB, so a test sees
// every archive request. It keeps the Host header, so SigV4 signatures made
// for the gateway still verify. It counts requests by object key and client,
// and it can hold the responses for chosen keys until the test releases them
// or the client gives up.
type s3Gateway struct {
	server *httptest.Server
	// prefix is the request path before an object key: /bucket/prefix/.
	prefix string
	mu     sync.Mutex
	seen   []gatewayRequest
	hold   func(key string) bool
	// fail answers the matching requests with 403 instead of forwarding them.
	fail func(method, key string) bool
	// held receives each key whose response is held; release ends every hold.
	held    chan string
	release chan struct{}
	// abandoned lists held keys whose client disconnected first.
	abandoned []string
}

type gatewayRequest struct {
	method, key string
	// duckdb is true for DuckDB's requests and false for Kit's.
	duckdb bool
}

func newS3Gateway(t *testing.T, s3 dbtest.S3, prefix string) *s3Gateway {
	t.Helper()
	target, err := url.Parse(s3.Endpoint)
	require.NoError(t, err)
	g := &s3Gateway{prefix: "/" + s3.Bucket + "/" + prefix + "/", held: make(chan string, 64), release: make(chan struct{})}
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(target)
		r.Out.Host = r.In.Host
	}}
	g.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutPrefix(r.URL.Path, g.prefix)
		if !ok {
			proxy.ServeHTTP(w, r)
			return
		}
		g.mu.Lock()
		g.seen = append(g.seen, gatewayRequest{method: r.Method, key: key, duckdb: strings.Contains(strings.ToLower(r.UserAgent()), "duckdb")})
		hold := g.hold != nil && r.Method == http.MethodGet && g.hold(key)
		fail := g.fail != nil && g.fail(r.Method, key)
		release := g.release
		g.mu.Unlock()
		if fail {
			http.Error(w, "injected failure", http.StatusForbidden)
			return
		}
		if hold {
			g.held <- key
			select {
			case <-release:
			case <-r.Context().Done():
				g.mu.Lock()
				g.abandoned = append(g.abandoned, key)
				g.mu.Unlock()
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		g.holdKeys(nil)
		g.server.Close()
	})
	return g
}

// holdKeys holds the GET responses of keys that hold selects; nil releases
// every held response and stops holding.
func (g *s3Gateway) holdKeys(hold func(key string) bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if hold == nil && g.hold != nil {
		close(g.release)
		g.release = make(chan struct{})
	}
	g.hold = hold
}

// failRequests fails the requests that fail selects; nil stops failing.
func (g *s3Gateway) failRequests(fail func(method, key string) bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fail = fail
}

func (g *s3Gateway) mark() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.seen)
}

// gets counts the GETs since mark for keys starting with prefix, by client.
func (g *s3Gateway) gets(mark int, prefix string, duckdb bool) (keys []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, request := range g.seen[mark:] {
		if request.method == http.MethodGet && request.duckdb == duckdb && strings.HasPrefix(request.key, prefix) {
			keys = append(keys, request.key)
		}
	}
	return keys
}

func (g *s3Gateway) abandonedKeys() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.abandoned...)
}

// newGatewayArchive is a database and an S3 service whose archive requests
// all pass through a gateway.
func newGatewayArchive(t *testing.T, prefix string) (remoteArchive, *s3Gateway, context.Context) {
	t.Helper()
	pool, ctx := dbtest.NewPool(t)
	s3 := dbtest.NewS3(t)
	dbtest.IssueAWSCredentials(t, s3.AccessKey, s3.SecretKey, time.Now().Add(time.Hour))
	return remoteArchive{pool: pool, s3: s3}, newS3Gateway(t, s3, prefix), ctx
}

func (r remoteArchive) gatewayConfig(g *s3Gateway, prefix string, t *testing.T) Config {
	cfg := r.config(prefix, t.TempDir())
	cfg.S3Endpoint = g.server.URL
	return cfg
}

// exportAll publishes every hot result and completes the lookup directory.
func exportAll(t *testing.T, ctx context.Context, e *Engine, pool *pgxpool.Pool) {
	t.Helper()
	for hotRows(t, ctx, pool) > 0 {
		require.NotEmpty(t, exportSettled(t, ctx, e, pool))
	}
	backfillUntilComplete(t, ctx, e)
}

// unrelatedFixtures are results in other months, runs, and series.
func unrelatedFixtures(months int) []lookupFixture {
	fixtures := []lookupFixture{}
	for month := range months {
		for i := range 3 {
			fixtures = append(fixtures, lookupFixture{
				id:          fmt.Sprintf("unrelated-%02d-%d", month, i),
				run:         fmt.Sprintf("unrelated-run-%d", month),
				fingerprint: fmt.Sprintf("unrelated-history-%d", i),
				at:          time.Date(2025+month/12, time.Month(month%12+1), 2, 0, 0, 0, 0, time.UTC),
			})
		}
	}
	return fixtures
}

// TestRemoteScopedRequests counts the archive requests that remote reads
// send. A read of one result, run, or history opens the same fact files and
// no summary catalog, however much unrelated history exists, and its lookup
// downloads stay within one partition's segments; an unknown ID opens no fact
// file. Summary catalogs are downloaded once into the cache and read from it,
// and a full cache makes DuckDB read them from the bucket instead.
func TestRemoteScopedRequests(t *testing.T) {
	r, gateway, ctx := newGatewayArchive(t, "scoped")
	fixtures := legacyFixtures(2, 6)
	insertLookupResults(t, ctx, r.pool, fixtures...)
	e := r.open(t, ctx, r.gatewayConfig(gateway, "scoped", t))
	exportAll(t, ctx, e, r.pool)
	require.NoError(t, e.Close())
	files := len(manifestPaths(t, ctx, r.pool))

	// Each measurement uses a fresh engine: DuckDB caches remote file data
	// per handle, which would hide a repeated read's requests.
	type scopedRead struct {
		ids, factGETs, summaryGETs, lookupGETs []string
	}
	read := func(scope queryScope, where string) scopedRead {
		e := r.open(t, ctx, r.gatewayConfig(gateway, "scoped", t))
		defer func() { require.NoError(t, e.Close()) }()
		mark := gateway.mark()
		var got scopedRead
		var err error
		got.ids, err = scopedIDs(ctx, e, scope, where)
		require.NoError(t, err)
		for _, kind := range []string{"catalog/runs-", "catalog/series-"} {
			got.summaryGETs = append(got.summaryGETs, gateway.gets(mark, kind, true)...)
			got.summaryGETs = append(got.summaryGETs, gateway.gets(mark, kind, false)...)
		}
		got.factGETs = gateway.gets(mark, "data/", true)
		got.lookupGETs = append(gateway.gets(mark, "catalog/lookup-", false), gateway.gets(mark, "catalog/lookup-", true)...)
		return got
	}
	pointRead := func(id string) (ids, factGETs, summaryGETs []string) {
		got := read(queryScope{ResultIDs: []string{id}}, "id="+literal(id))
		return got.ids, got.factGETs, got.summaryGETs
	}
	targeted := map[string]struct {
		scope queryScope
		where string
	}{
		"result":  {queryScope{ResultIDs: []string{fixtures[0].id}}, "id=" + literal(fixtures[0].id)},
		"run":     {queryScope{RunIDs: []string{fixtures[1].run}}, "run_id=" + literal(fixtures[1].run)},
		"history": {queryScope{Fingerprints: []string{fixtures[2].fingerprint}}, "history_fingerprint=" + literal(fixtures[2].fingerprint)},
	}
	before := map[string]scopedRead{}
	for name, query := range targeted {
		before[name] = read(query.scope, query.where)
		require.NotEmpty(t, before[name].ids, name)
		require.NotEmpty(t, before[name].factGETs, name)
		assert.Empty(t, before[name].summaryGETs, "a %s read opens no summary catalog", name)
	}
	ids, factGETsBefore, _ := pointRead(fixtures[0].id)
	assert.Equal(t, []string{fixtures[0].id}, ids)

	unrelatedMonths := 10 * files
	insertLookupResults(t, ctx, r.pool, unrelatedFixtures(unrelatedMonths)...)
	e = r.open(t, ctx, r.gatewayConfig(gateway, "scoped", t))
	exportAll(t, ctx, e, r.pool)
	require.NoError(t, e.Close())
	require.GreaterOrEqual(t, len(manifestPaths(t, ctx, r.pool)), 10*files, "unrelated history grows tenfold")

	ids, factGETsAfterUnrelatedHistory, summaryGETs := pointRead(fixtures[0].id)
	assert.Equal(t, []string{fixtures[0].id}, ids)
	assert.Equal(t, factGETsBefore, factGETsAfterUnrelatedHistory)
	assert.Empty(t, summaryGETs)
	for name, query := range targeted {
		after := read(query.scope, query.where)
		assert.Equal(t, before[name].ids, after.ids, name)
		assert.Equal(t, before[name].factGETs, after.factGETs, "a %s read opens the same fact files", name)
		assert.Empty(t, after.summaryGETs, name)
		// A key's lookup reads one partition, which has at most
		// lookupSegmentCap segments however the directory grows.
		assert.LessOrEqual(t, len(after.lookupGETs), lookupSegmentCap, name)
		t.Logf("%s read: %d fact GETs; lookup downloads %d before and %d after tenfold unrelated history",
			name, len(after.factGETs), len(before[name].lookupGETs), len(after.lookupGETs))
	}
	ids, factGETsForUnknownID, _ := pointRead("unknown-result")
	assert.Empty(t, ids)
	assert.Zero(t, factGETsForUnknownID)

	summaryRead := func(cfg Config) (runs []string, kitGETs, duckGETs, seriesGETs []string) {
		e := r.open(t, ctx, cfg)
		defer func() { require.NoError(t, e.Close()) }()
		store := NewStore(db.NewStore(r.pool), e)
		mark := gateway.mark()
		for range 2 {
			var err error
			runs, err = store.SelectRepositoryVerdictRuns(ctx, "https://example.com/lookup", time.Time{}, nil)
			require.NoError(t, err)
		}
		seriesGETs = append(gateway.gets(mark, "catalog/series-", false), gateway.gets(mark, "catalog/series-", true)...)
		return runs, gateway.gets(mark, "catalog/runs-", false), gateway.gets(mark, "catalog/runs-", true), seriesGETs
	}
	files = len(manifestPaths(t, ctx, r.pool))
	expectedRuns := []string{"legacy-run-0", "legacy-run-1", "legacy-run-2"}
	for month := range unrelatedMonths {
		expectedRuns = append(expectedRuns, fmt.Sprintf("unrelated-run-%d", month))
	}
	cached, kitGETs, duckGETs, seriesGETs := summaryRead(r.gatewayConfig(gateway, "scoped", t))
	assert.ElementsMatch(t, expectedRuns, cached)
	assert.Len(t, kitGETs, files, "two reads download each run summary once, into the cache")
	assert.Empty(t, duckGETs, "DuckDB reads cached summaries from local files")
	assert.Empty(t, seriesGETs, "a run query neither downloads nor reads series summaries")

	full := r.gatewayConfig(gateway, "scoped", t)
	full.CatalogCacheBytes = 1
	bypassed, kitGETs, duckGETs, _ := summaryRead(full)
	assert.ElementsMatch(t, cached, bypassed)
	assert.Empty(t, kitGETs, "a full cache downloads nothing")
	assert.NotEmpty(t, duckGETs, "a full cache reads summaries from the bucket")
}
