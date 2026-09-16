package prodclone

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	benchdbclient "go.kenn.io/benchdb/sdk/go/benchdb"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"go.kenn.io/benchdb/internal/db"
)

const (
	defaultProfileWarmRuns    = 2
	profileSmallPageSize      = int64(5)
	profileMediumPageSize     = int64(10)
	profileLargePageSize      = int64(50)
	profileRecentPageSize     = int64(25)
	profileRecentMaxPageSize  = int64(100)
	profileExactQ             = "BM_ReadBinaryColumn"
	profileBroadQ             = "tpch"
	profileQRecentCommitLimit = int64(320)
)

type ProfileConfig struct {
	ServerURL string
	Samples   SampleManifest
	DB        ProfileDB
	Client    *http.Client
	WarmRuns  int
}

type ProfileDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

type ProfileResult struct {
	HTTPTimings   []HTTPProbeTiming
	SQLTimings    []SQLProfileTiming
	Plans         []ExplainPlanArtifact
	RelationSizes []RelationSize `json:"relation_sizes,omitempty"`
}

type SQLProfileTiming struct {
	Surface     string  `json:"surface"`
	Name        string  `json:"name"`
	Operation   string  `json:"operation"`
	DurationMS  float64 `json:"duration_ms"`
	RowCount    int64   `json:"row_count,omitempty"`
	ExplainFile string  `json:"explain_file,omitempty"`
	Passed      bool    `json:"passed"`
	Error       string  `json:"error,omitempty"`
}

type ExplainPlanArtifact struct {
	Name      string         `json:"name"`
	Operation string         `json:"operation"`
	Filename  string         `json:"filename"`
	PlanJSON  jsontext.Value `json:"plan_json"`
}

type RelationSize struct {
	Table      string `json:"table"`
	TotalBytes int64  `json:"total_bytes"`
	TableBytes int64  `json:"table_bytes"`
	IndexBytes int64  `json:"index_bytes"`
}

type profileSamples struct {
	recentResultID     string
	oldResultID        string
	longFingerprint    string
	shortFingerprint   string
	compareBaseline    string
	compareContender   string
	compareFingerprint string
	haveCompare        bool
	ciReportRepository string
	ciReportCommitSHA  string
	ciReportRunIDs     []string
	haveCIReport       bool
}

type profileHTTPCall struct {
	name      string
	operation string
	call      func(context.Context, *benchdbclient.Client, runtime.RequestEditorFn) (int, error)
}

type profileSQLQuery struct {
	name      string
	operation string
	sql       string
	args      []any
	explain   bool
}

func RunProfile(ctx context.Context, cfg ProfileConfig) (ProfileResult, error) {
	var result ProfileResult
	if cfg.DB == nil {
		return result, errors.New("profile database is required")
	}
	samples, err := selectProfileSamples(cfg.Samples)
	if err != nil {
		return result, err
	}

	httpTimings, httpErr := runProfileHTTP(ctx, cfg, samples)
	result.HTTPTimings = httpTimings

	sqlTimings, plans, relationSizes, sqlErr := runProfileSQL(ctx, cfg.DB, samples)
	result.SQLTimings = sqlTimings
	result.Plans = plans
	result.RelationSizes = relationSizes

	return result, errors.Join(httpErr, sqlErr)
}

func selectProfileSamples(manifest SampleManifest) (profileSamples, error) {
	var samples profileSamples
	recent := manifest.Categories[sampleCategoryRecentResult]
	longHistory := manifest.Categories[sampleCategoryLongHistory]
	if recent.ResultID == "" {
		return samples, errors.New("profile sample manifest missing recent_result.result_id")
	}
	samples.recentResultID = recent.ResultID
	if longHistory.HistoryFingerprint != "" {
		samples.longFingerprint = longHistory.HistoryFingerprint
	} else {
		historyMember := manifest.Categories[sampleCategoryHistoryMember]
		samples.longFingerprint = historyMember.HistoryFingerprint
	}
	if samples.longFingerprint == "" {
		return samples, errors.New("profile sample manifest missing long_history.history_fingerprint or history_member.history_fingerprint")
	}

	old := manifest.Categories[sampleCategoryOldResult]
	if old.ResultID != "" {
		samples.oldResultID = old.ResultID
	} else {
		samples.oldResultID = recent.ResultID
	}

	shortHistory := manifest.Categories[sampleCategoryShortHistory]
	if shortHistory.HistoryFingerprint != "" {
		samples.shortFingerprint = shortHistory.HistoryFingerprint
	} else {
		samples.shortFingerprint = samples.longFingerprint
	}

	if manifest.Compare != nil {
		samples.compareBaseline = manifest.Compare.BaselineResultID
		samples.compareContender = manifest.Compare.ContenderResultID
		samples.compareFingerprint = manifest.Compare.HistoryFingerprint
		samples.haveCompare = samples.compareBaseline != "" && samples.compareContender != ""
	}
	if manifest.CIReport != nil {
		samples.ciReportRepository = manifest.CIReport.Repository
		samples.ciReportCommitSHA = manifest.CIReport.CommitSHA
		samples.ciReportRunIDs = append([]string(nil), manifest.CIReport.RunIDs...)
		samples.haveCIReport = samples.ciReportRepository != "" && samples.ciReportCommitSHA != "" && len(samples.ciReportRunIDs) > 0
	}
	return samples, nil
}

func runProfileHTTP(ctx context.Context, cfg ProfileConfig, samples profileSamples) ([]HTTPProbeTiming, error) {
	_, err := url.Parse(cfg.ServerURL)
	if err != nil {
		return nil, fmt.Errorf("parse profile server URL: %w", err)
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	apiClient, err := benchdbclient.NewHTTPClient(cfg.ServerURL, client)
	if err != nil {
		return nil, fmt.Errorf("create profile client: %w", err)
	}
	warmRuns := cfg.WarmRuns
	if warmRuns < 1 {
		warmRuns = defaultProfileWarmRuns
	}

	calls := profileHTTPCalls(samples)
	timings := make([]HTTPProbeTiming, 0, len(calls)*(warmRuns+1))
	var failures int
	for _, call := range calls {
		for i := 0; i <= warmRuns; i++ {
			label := "cold"
			if i > 0 {
				label = fmt.Sprintf("warm-%d", i)
			}
			timing := runOneProfileHTTP(ctx, apiClient, call, label)
			timings = append(timings, timing)
			if !timing.Passed {
				failures++
			}
		}
	}
	if failures > 0 {
		return timings, fmt.Errorf("%d HTTP profile %s failed", failures, plural(failures, "request", "requests"))
	}
	return timings, nil
}

func profileHTTPCalls(samples profileSamples) []profileHTTPCall {
	calls := []profileHTTPCall{{name: "RecentRunsPage25", operation: "GET /api/runs/recent", call: func(ctx context.Context, client *benchdbclient.Client, editor runtime.RequestEditorFn) (int, error) {
		resp, err := client.ListRecentRunsWithResponse(ctx, &benchdbclient.ListRecentRunsRequestOptions{Query: &benchdbclient.ListRecentRunsQuery{PageSize: new(int64(profileRecentPageSize))}}, editor)
		return responseStatus(resp), err
	}},
		{name: "RecentRunsPage100", operation: "GET /api/runs/recent", call: func(ctx context.Context, client *benchdbclient.Client, editor runtime.RequestEditorFn) (int, error) {
			resp, err := client.ListRecentRunsWithResponse(ctx, &benchdbclient.ListRecentRunsRequestOptions{Query: &benchdbclient.ListRecentRunsQuery{PageSize: new(int64(profileRecentMaxPageSize))}}, editor)
			return responseStatus(resp), err
		}},
		{name: "RecentRunsPage2", operation: "GET /api/runs/recent", call: func(ctx context.Context, client *benchdbclient.Client, editor runtime.RequestEditorFn) (int, error) {
			resp, err := client.ListRecentRunsWithResponse(ctx, &benchdbclient.ListRecentRunsRequestOptions{Query: &benchdbclient.ListRecentRunsQuery{PageSize: new(int64(profileRecentPageSize)), Offset: new(int32(profileRecentPageSize))}}, editor)
			return responseStatus(resp), err
		}},
		{name: "SeriesBrowseDefaultPage5", operation: "GET /api/series", call: func(ctx context.Context, client *benchdbclient.Client, editor runtime.RequestEditorFn) (int, error) {
			resp, err := client.ListSeriesWithResponse(ctx, &benchdbclient.ListSeriesRequestOptions{Query: &benchdbclient.ListSeriesQuery{PageSize: new(int64(profileSmallPageSize))}}, editor)
			return responseStatus(resp), err
		}},
		{name: "SeriesBrowseDefaultPage10", operation: "GET /api/series", call: func(ctx context.Context, client *benchdbclient.Client, editor runtime.RequestEditorFn) (int, error) {
			resp, err := client.ListSeriesWithResponse(ctx, &benchdbclient.ListSeriesRequestOptions{Query: &benchdbclient.ListSeriesQuery{PageSize: new(int64(profileMediumPageSize))}}, editor)
			return responseStatus(resp), err
		}},
		{name: "SeriesBrowseDefaultPage50", operation: "GET /api/series", call: func(ctx context.Context, client *benchdbclient.Client, editor runtime.RequestEditorFn) (int, error) {
			resp, err := client.ListSeriesWithResponse(ctx, &benchdbclient.ListSeriesRequestOptions{Query: &benchdbclient.ListSeriesQuery{PageSize: new(int64(profileLargePageSize))}}, editor)
			return responseStatus(resp), err
		}},
		{name: "SeriesBrowseQExact", operation: "GET /api/series", call: func(ctx context.Context, client *benchdbclient.Client, editor runtime.RequestEditorFn) (int, error) {
			resp, err := client.ListSeriesWithResponse(ctx, &benchdbclient.ListSeriesRequestOptions{Query: &benchdbclient.ListSeriesQuery{PageSize: new(int64(profileMediumPageSize)), Q: new(profileExactQ)}}, editor)
			return responseStatus(resp), err
		}},
		{name: "SeriesBrowseQBroad", operation: "GET /api/series", call: func(ctx context.Context, client *benchdbclient.Client, editor runtime.RequestEditorFn) (int, error) {
			resp, err := client.ListSeriesWithResponse(ctx, &benchdbclient.ListSeriesRequestOptions{Query: &benchdbclient.ListSeriesQuery{PageSize: new(int64(profileMediumPageSize)), Q: new(profileBroadQ)}}, editor)
			return responseStatus(resp), err
		}},
		{name: "SeriesBrowseFingerprint", operation: "GET /api/series", call: func(ctx context.Context, client *benchdbclient.Client, editor runtime.RequestEditorFn) (int, error) {
			resp, err := client.ListSeriesWithResponse(ctx, &benchdbclient.ListSeriesRequestOptions{Query: &benchdbclient.ListSeriesQuery{PageSize: new(int64(profileSmallPageSize)), Fingerprint: new(samples.longFingerprint)}}, editor)
			return responseStatus(resp), err
		}},
		{name: "HistoryLong", operation: "GET /api/history", call: func(ctx context.Context, client *benchdbclient.Client, editor runtime.RequestEditorFn) (int, error) {
			resp, err := client.GetHistoryWithResponse(ctx, &benchdbclient.GetHistoryRequestOptions{Query: &benchdbclient.GetHistoryQuery{Fingerprint: samples.longFingerprint}}, editor)
			return responseStatus(resp), err
		}},
		{name: "HistoryShort", operation: "GET /api/history", call: func(ctx context.Context, client *benchdbclient.Client, editor runtime.RequestEditorFn) (int, error) {
			resp, err := client.GetHistoryWithResponse(ctx, &benchdbclient.GetHistoryRequestOptions{Query: &benchdbclient.GetHistoryQuery{Fingerprint: samples.shortFingerprint}}, editor)
			return responseStatus(resp), err
		}},
		{name: "ResultDetailRecent", operation: "GET /api/benchmark-results/{id}", call: func(ctx context.Context, client *benchdbclient.Client, editor runtime.RequestEditorFn) (int, error) {
			resp, err := client.GetBenchmarkResultWithResponse(ctx, &benchdbclient.GetBenchmarkResultRequestOptions{PathParams: &benchdbclient.GetBenchmarkResultPath{ID: samples.recentResultID}}, editor)
			return responseStatus(resp), err
		}},
		{name: "ResultDetailOld", operation: "GET /api/benchmark-results/{id}", call: func(ctx context.Context, client *benchdbclient.Client, editor runtime.RequestEditorFn) (int, error) {
			resp, err := client.GetBenchmarkResultWithResponse(ctx, &benchdbclient.GetBenchmarkResultRequestOptions{PathParams: &benchdbclient.GetBenchmarkResultPath{ID: samples.oldResultID}}, editor)
			return responseStatus(resp), err
		}},
		{name: "ResultListDefault", operation: "GET /api/benchmark-results", call: func(ctx context.Context, client *benchdbclient.Client, editor runtime.RequestEditorFn) (int, error) {
			resp, err := client.ListBenchmarkResultsWithResponse(ctx, &benchdbclient.ListBenchmarkResultsRequestOptions{Query: &benchdbclient.ListBenchmarkResultsQuery{PageSize: new(int64(profileSmallPageSize))}}, editor)
			return responseStatus(resp), err
		}},
		{name: "ResultListFilteredRecent", operation: "GET /api/benchmark-results?earliest_timestamp=...", call: func(ctx context.Context, client *benchdbclient.Client, editor runtime.RequestEditorFn) (int, error) {
			resp, err := client.ListBenchmarkResultsWithResponse(ctx, &benchdbclient.ListBenchmarkResultsRequestOptions{Query: &benchdbclient.ListBenchmarkResultsQuery{PageSize: new(int64(profileSmallPageSize)), EarliestTimestamp: new(time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339))}}, editor)
			return responseStatus(resp), err
		}},
	}
	if samples.haveCompare {
		calls = append(calls, profileHTTPCall{name: "CompareBenchmarkResults", operation: "GET /api/compare/benchmark-results", call: func(ctx context.Context, client *benchdbclient.Client, editor runtime.RequestEditorFn) (int, error) {
			resp, err := client.CompareBenchmarkResultsWithResponse(ctx, &benchdbclient.CompareBenchmarkResultsRequestOptions{Query: &benchdbclient.CompareBenchmarkResultsQuery{BaselineResultID: samples.compareBaseline, ContenderResultID: samples.compareContender}}, editor)
			return responseStatus(resp), err
		}})
	}
	if samples.haveCIReport {
		calls = append(calls, profileHTTPCall{name: "CIReportByCommitRun", operation: "GET /api/ci/report", call: func(ctx context.Context, client *benchdbclient.Client, editor runtime.RequestEditorFn) (int, error) {
			resp, err := client.GetCiReportWithResponse(ctx, &benchdbclient.GetCiReportRequestOptions{Query: &benchdbclient.GetCiReportQuery{Repository: new(samples.ciReportRepository), CommitSha: new(samples.ciReportCommitSHA), RunIds: new(strings.Join(samples.ciReportRunIDs, ","))}}, editor)
			return responseStatus(resp), err
		}})
	}
	if samples.ciReportCommitSHA != "" {
		ref := samples.ciReportCommitSHA[:min(6, len(samples.ciReportCommitSHA))]
		for _, offset := range []int32{0, int32(profileRecentPageSize)} {
			calls = append(calls, profileHTTPCall{name: fmt.Sprintf("RecentRunsSearchOffset%d", offset), operation: "GET /api/runs/recent by partial commit ref", call: func(ctx context.Context, client *benchdbclient.Client, editor runtime.RequestEditorFn) (int, error) {
				resp, err := client.ListRecentRunsWithResponse(ctx, &benchdbclient.ListRecentRunsRequestOptions{Query: &benchdbclient.ListRecentRunsQuery{PageSize: new(int64(profileRecentPageSize)), Q: new(ref), Offset: new(offset)}}, editor)
				return responseStatus(resp), err
			}})
		}
	}
	return calls
}

func runOneProfileHTTP(ctx context.Context, client *benchdbclient.Client, call profileHTTPCall, label string) HTTPProbeTiming {
	timing := HTTPProbeTiming{Surface: "HTTP profile", Name: call.name + " " + label, Operation: call.operation, Method: http.MethodGet}
	start := time.Now()
	status, err := call.call(ctx, client, func(_ context.Context, req *http.Request) error {
		timing.Path = requestPath(req.URL)
		return nil
	})
	timing.DurationMS = float64(time.Since(start).Microseconds()) / 1000.0
	timing.StatusCode = status
	if status != 0 && status != http.StatusOK {
		timing.Error = fmt.Sprintf("expected 200, got %d", status)
		return timing
	}
	if err != nil {
		timing.Error = err.Error()
		return timing
	}
	timing.Passed = true
	return timing
}

func requestPath(u *url.URL) string {
	if u == nil {
		return ""
	}
	if u.RawQuery == "" {
		return u.Path
	}
	return u.Path + "?" + u.RawQuery
}

func runProfileSQL(ctx context.Context, db ProfileDB, samples profileSamples) ([]SQLProfileTiming, []ExplainPlanArtifact, []RelationSize, error) {
	queries := profileSQLQueries(samples)
	timings := make([]SQLProfileTiming, 0, len(queries)+1)
	plans := make([]ExplainPlanArtifact, 0, len(queries))
	var failures int
	for _, query := range queries {
		timing := timeProfileSQLQuery(ctx, db, query)
		timings = append(timings, timing)
		if !timing.Passed {
			failures++
			continue
		}
		if !query.explain {
			continue
		}
		plan, explainTiming := explainProfileSQLQuery(ctx, db, query)
		if explainTiming != nil {
			timings = append(timings, *explainTiming)
			failures++
			continue
		}
		timings[len(timings)-1].ExplainFile = plan.Filename
		plans = append(plans, plan)
	}

	sizes, sizeTiming := profileRelationSizes(ctx, db)
	timings = append(timings, sizeTiming)
	if !sizeTiming.Passed {
		failures++
	}

	if failures > 0 {
		return timings, plans, sizes, fmt.Errorf("%d SQL profile %s failed", failures, plural(failures, "query", "queries"))
	}
	return timings, plans, sizes, nil
}

func timeProfileSQLQuery(ctx context.Context, db ProfileDB, query profileSQLQuery) SQLProfileTiming {
	start := time.Now()
	rows, err := db.Query(ctx, query.sql, query.args...)
	durationMS := float64(time.Since(start).Microseconds()) / 1000.0
	timing := SQLProfileTiming{
		Surface:    "SQL profile",
		Name:       query.name,
		Operation:  query.operation,
		DurationMS: durationMS,
		Passed:     false,
	}
	if err != nil {
		timing.Error = err.Error()
		return timing
	}
	defer rows.Close()

	var rowCount int64
	for rows.Next() {
		rowCount++
		_, _ = rows.Values()
	}
	durationMS = float64(time.Since(start).Microseconds()) / 1000.0
	timing.DurationMS = durationMS
	timing.RowCount = rowCount
	if err := rows.Err(); err != nil {
		timing.Error = err.Error()
		return timing
	}
	timing.Passed = true
	return timing
}

func explainProfileSQLQuery(ctx context.Context, db ProfileDB, query profileSQLQuery) (ExplainPlanArtifact, *SQLProfileTiming) {
	explainSQL := "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) " + query.sql
	start := time.Now()
	rows, err := db.Query(ctx, explainSQL, query.args...)
	durationMS := float64(time.Since(start).Microseconds()) / 1000.0
	timing := &SQLProfileTiming{
		Surface:    "SQL profile",
		Name:       query.name + " Explain",
		Operation:  "EXPLAIN " + query.operation,
		DurationMS: durationMS,
		Passed:     false,
	}
	if err != nil {
		timing.Error = err.Error()
		return ExplainPlanArtifact{}, timing
	}
	defer rows.Close()

	var raw []byte
	if rows.Next() {
		if err := rows.Scan(&raw); err != nil {
			timing.Error = err.Error()
			return ExplainPlanArtifact{}, timing
		}
	}
	durationMS = float64(time.Since(start).Microseconds()) / 1000.0
	timing.DurationMS = durationMS
	if err := rows.Err(); err != nil {
		timing.Error = err.Error()
		return ExplainPlanArtifact{}, timing
	}
	if len(raw) == 0 {
		timing.Error = "EXPLAIN returned no plan"
		return ExplainPlanArtifact{}, timing
	}
	if !jsontext.Value(raw).IsValid() {
		timing.Error = "EXPLAIN returned invalid JSON"
		return ExplainPlanArtifact{}, timing
	}
	return ExplainPlanArtifact{
		Name:      query.name,
		Operation: query.operation,
		Filename:  ExplainPlanFilename(query.name),
		PlanJSON:  append(jsontext.Value(nil), raw...),
	}, nil
}

func profileRelationSizes(ctx context.Context, db ProfileDB) ([]RelationSize, SQLProfileTiming) {
	start := time.Now()
	rows, err := db.Query(ctx, relationSizesSQL, profileRelationTables)
	durationMS := float64(time.Since(start).Microseconds()) / 1000.0
	timing := SQLProfileTiming{
		Surface:    "SQL profile",
		Name:       "RelationSizes",
		Operation:  "relation size scan",
		DurationMS: durationMS,
		Passed:     false,
	}
	if err != nil {
		timing.Error = err.Error()
		return nil, timing
	}
	defer rows.Close()

	var sizes []RelationSize
	for rows.Next() {
		var size RelationSize
		if err := rows.Scan(&size.Table, &size.TotalBytes, &size.TableBytes, &size.IndexBytes); err != nil {
			timing.Error = err.Error()
			return sizes, timing
		}
		sizes = append(sizes, size)
	}
	durationMS = float64(time.Since(start).Microseconds()) / 1000.0
	timing.DurationMS = durationMS
	timing.RowCount = int64(len(sizes))
	if err := rows.Err(); err != nil {
		timing.Error = err.Error()
		return sizes, timing
	}
	timing.Passed = true
	return sizes, timing
}

func profileSQLQueries(samples profileSamples) []profileSQLQuery {
	queries := []profileSQLQuery{
		{
			name:      "RecentRunsPage25",
			operation: "recent runs page 25",
			sql:       db.RecentRunsSQL,
			args:      []any{nil, "", int32(0), int32(profileRecentPageSize + 1)},
			explain:   true,
		},
		{
			name:      "RecentRunsPage100",
			operation: "recent runs page 100",
			sql:       db.RecentRunsSQL,
			args:      []any{nil, "", int32(0), int32(profileRecentMaxPageSize + 1)},
			explain:   true,
		},
		{
			name:      "RecentRunsPage2",
			operation: "recent runs second page",
			sql:       db.RecentRunsSQL,
			args:      []any{nil, "", int32(profileRecentPageSize), int32(profileRecentPageSize + 1)},
			explain:   true,
		},
		{
			name:      "SeriesBrowseDefaultPage5",
			operation: "series browse default page 5",
			sql:       seriesProfileDefaultSQL,
			args:      []any{profileSmallPageSize},
			explain:   true,
		},
		{
			name:      "SeriesBrowseDefaultPage50",
			operation: "series browse default page 50",
			sql:       seriesProfileDefaultSQL,
			args:      []any{profileLargePageSize},
			explain:   true,
		},
		{
			name:      "SeriesMembersForPage50",
			operation: "series member enrichment for page 50",
			sql:       seriesMembersForPageSQL,
			args:      []any{profileLargePageSize},
			explain:   true,
		},
		{
			name:      "SeriesQBroadCaseMatches",
			operation: "series q broad case cardinality",
			sql:       seriesQCaseMatchesSQL,
			args:      []any{profileBroadQ},
			explain:   false,
		},
		{
			name:      "SeriesQBroadRecentMembers",
			operation: "series q broad bounded recent members",
			sql:       seriesQRecentMembersSQL,
			args:      []any{profileBroadQ, profileQRecentCommitLimit, profileMediumPageSize},
			explain:   true,
		},
		{
			name:      "SeriesBrowseFingerprint",
			operation: "series browse fingerprint",
			sql:       seriesProfileFingerprintSQL,
			args:      []any{profileSmallPageSize, samples.longFingerprint},
			explain:   true,
		},
		{
			name:      "HistoryLong",
			operation: "history long",
			sql:       historyProfileSQL,
			args:      []any{samples.longFingerprint},
			explain:   true,
		},
		{
			name:      "HistoryShort",
			operation: "history short",
			sql:       historyProfileSQL,
			args:      []any{samples.shortFingerprint},
			explain:   true,
		},
		{
			name:      "ResultDetailRecent",
			operation: "result detail recent",
			sql:       resultDetailProfileSQL,
			args:      []any{samples.recentResultID},
			explain:   true,
		},
		{
			name:      "ResultDetailOld",
			operation: "result detail old",
			sql:       resultDetailProfileSQL,
			args:      []any{samples.oldResultID},
			explain:   true,
		},
		{
			name:      "ResultListDefault",
			operation: "result list default",
			sql:       resultListDefaultProfileSQL,
			args:      []any{profileSmallPageSize},
			explain:   true,
		},
		{
			name:      "ResultListFilteredRecent",
			operation: "result list filtered recent",
			sql:       resultListFilteredRecentProfileSQL,
			args:      []any{profileSmallPageSize, time.Now().UTC().AddDate(0, 0, -30)},
			explain:   true,
		},
	}
	if samples.haveCompare {
		queries = append(queries,
			profileSQLQuery{
				name:      "CompareBaselineLookup",
				operation: "compare baseline lookup",
				sql:       compareResultLookupProfileSQL,
				args:      []any{samples.compareBaseline},
				explain:   true,
			},
			profileSQLQuery{
				name:      "CompareContenderLookup",
				operation: "compare contender lookup",
				sql:       compareResultLookupProfileSQL,
				args:      []any{samples.compareContender},
				explain:   true,
			},
			profileSQLQuery{
				name:      "CompareHistoryAsOf",
				operation: "compare history as-of",
				sql:       compareHistoryAsOfProfileSQL,
				args:      []any{samples.compareFingerprint, samples.compareBaseline},
				explain:   true,
			},
		)
	}
	if samples.ciReportCommitSHA != "" {
		ref := samples.ciReportCommitSHA[:min(6, len(samples.ciReportCommitSHA))]
		for _, offset := range []int32{0, int32(profileRecentPageSize)} {
			queries = append(queries, profileSQLQuery{
				name:      fmt.Sprintf("RecentRunsSearchOffset%d", offset),
				operation: "recent runs by partial commit ref",
				sql:       db.RecentRunsSQL,
				args:      []any{nil, ref, offset, int32(profileRecentPageSize + 1)},
				explain:   true,
			})
		}
	}

	return queries
}

var explainFilenamePattern = regexp.MustCompile(`[^a-z0-9]+`)

func ExplainPlanFilename(name string) string {
	normalized := strings.ToLower(strings.TrimSpace(name))
	normalized = explainFilenamePattern.ReplaceAllString(normalized, "-")
	normalized = strings.Trim(normalized, "-")
	if normalized == "" {
		normalized = "plan"
	}
	return normalized + ".json"
}

var profileRelationTables = []string{
	"benchmark_result",
	"case",
	"context",
	"info",
	"hardware",
	"commit",
	"api_token",
	"user",
}

const relationSizesSQL = `
SELECT
  n.nspname || '.' || quote_ident(c.relname) AS table_name,
  pg_total_relation_size(c.oid) AS total_bytes,
  pg_relation_size(c.oid) AS table_bytes,
  pg_indexes_size(c.oid) AS index_bytes
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public'
  AND c.relkind IN ('r', 'p')
  AND c.relname = ANY($1::text[])
ORDER BY table_name
`

const seriesProfileDefaultSQL = `
WITH recent_commit AS MATERIALIZED (
  SELECT id, sha AS commit_sha, "timestamp" AS commit_timestamp
  FROM commit
  WHERE sha = fork_point_sha
    AND "timestamp" IS NOT NULL
  ORDER BY "timestamp" DESC, id DESC
  LIMIT GREATEST($1::integer, 32)
),
members AS MATERIALIZED (
  SELECT
    br.history_fingerprint, br.id, br."timestamp" AS result_timestamp,
    br.unit, br.data, br.case_id, br.context_id, br.hardware_id,
    br.commit_repo_url, rc.commit_sha, rc.commit_timestamp
  FROM recent_commit rc
  JOIN benchmark_result br ON br.commit_id = rc.id
  WHERE br.error IS NULL
),
latest AS (
  SELECT DISTINCT ON (history_fingerprint)
    history_fingerprint, id, result_timestamp, unit, data,
    case_id, context_id, hardware_id, commit_repo_url, commit_sha, commit_timestamp
  FROM members
  ORDER BY history_fingerprint, commit_timestamp DESC, id DESC
),
page AS MATERIALIZED (
  SELECT
    l.history_fingerprint,
    l.id AS latest_result_id,
    l.result_timestamp AS latest_result_timestamp,
    l.commit_sha AS latest_commit_sha,
    l.commit_timestamp AS latest_commit_timestamp,
    l.commit_repo_url,
    l.unit AS latest_unit,
    l.data AS latest_data,
    cs.name AS case_name, cs.tags AS case_tags, ctx.tags AS context_tags,
    hw.id AS hardware_id, hw.name AS hardware_name, hw.type AS hardware_type, hw.hash AS hardware_hash
  FROM latest l
  JOIN "case" cs ON cs.id = l.case_id
  JOIN context ctx ON ctx.id = l.context_id
  JOIN hardware hw ON hw.id = l.hardware_id
  ORDER BY l.commit_timestamp DESC, l.history_fingerprint DESC
  LIMIT $1
),
counts AS (
  SELECT br.history_fingerprint, count(*)::bigint AS point_count
  FROM benchmark_result br
  JOIN commit c ON c.id = br.commit_id
  JOIN page p ON p.history_fingerprint = br.history_fingerprint
  WHERE br.error IS NULL
    AND c.sha = c.fork_point_sha
    AND c."timestamp" IS NOT NULL
  GROUP BY br.history_fingerprint
)
SELECT
  p.history_fingerprint, p.latest_result_id, p.latest_result_timestamp,
  p.latest_commit_sha, p.latest_commit_timestamp,
  p.commit_repo_url, p.latest_unit, p.latest_data, cnt.point_count,
  p.case_name, p.case_tags, p.context_tags,
  p.hardware_id, p.hardware_name, p.hardware_type, p.hardware_hash
FROM page p
JOIN counts cnt ON cnt.history_fingerprint = p.history_fingerprint
ORDER BY p.latest_commit_timestamp DESC, p.history_fingerprint DESC
`

const seriesMembersForPageSQL = `
WITH recent_commit AS MATERIALIZED (
  SELECT id, sha AS commit_sha, "timestamp" AS commit_timestamp
  FROM commit
  WHERE sha = fork_point_sha
    AND "timestamp" IS NOT NULL
  ORDER BY "timestamp" DESC, id DESC
  LIMIT GREATEST($1::integer, 32)
),
members AS MATERIALIZED (
  SELECT br.history_fingerprint, br.id, br.case_id, br.context_id, br.hardware_id,
         br.commit_repo_url, rc.commit_sha, rc.commit_timestamp
  FROM recent_commit rc
  JOIN benchmark_result br ON br.commit_id = rc.id
  WHERE br.error IS NULL
),
latest AS (
  SELECT DISTINCT ON (history_fingerprint)
    history_fingerprint, id, case_id, context_id, hardware_id, commit_repo_url,
    commit_sha, commit_timestamp
  FROM members
  ORDER BY history_fingerprint, commit_timestamp DESC, id DESC
),
page AS MATERIALIZED (
  SELECT history_fingerprint
  FROM latest
  ORDER BY commit_timestamp DESC, history_fingerprint DESC
  LIMIT $1
)
SELECT br.history_fingerprint, br.id
FROM page p
JOIN benchmark_result br ON br.history_fingerprint = p.history_fingerprint
JOIN commit c ON c.id = br.commit_id
WHERE br.error IS NULL
  AND c.sha = c.fork_point_sha
  AND c."timestamp" IS NOT NULL
ORDER BY br.history_fingerprint, c."timestamp", br.id
`

const seriesQCaseMatchesSQL = `
SELECT count(*)::bigint
FROM "case"
WHERE name ILIKE '%' || $1::text || '%'
   OR tags::text ILIKE '%' || $1::text || '%'
`

const seriesQRecentMembersSQL = `
WITH matched_case(id) AS MATERIALIZED (
  SELECT id
  FROM "case"
  WHERE name ILIKE '%' || $1::text || '%'
     OR tags::text ILIKE '%' || $1::text || '%'
),
recent_commit_seed AS MATERIALIZED (
  SELECT id, sha AS commit_sha, "timestamp" AS commit_timestamp
  FROM commit
  WHERE sha = fork_point_sha
    AND "timestamp" IS NOT NULL
  ORDER BY "timestamp" DESC, id DESC
  LIMIT $2
),
recent_commit_boundary AS (
  SELECT min(commit_timestamp) AS min_commit_timestamp
  FROM recent_commit_seed
),
recent_commit AS MATERIALIZED (
  SELECT id, sha AS commit_sha, "timestamp" AS commit_timestamp
  FROM commit
  WHERE sha = fork_point_sha
    AND "timestamp" IS NOT NULL
    AND (
      (SELECT min_commit_timestamp FROM recent_commit_boundary) IS NULL
      OR "timestamp" >= (SELECT min_commit_timestamp FROM recent_commit_boundary)
    )
),
members AS MATERIALIZED (
  SELECT
    br.history_fingerprint, br.id, br.case_id, br.context_id, br.hardware_id,
    br.commit_repo_url, rc.commit_sha, rc.commit_timestamp
  FROM recent_commit rc
  JOIN benchmark_result br ON br.commit_id = rc.id
  JOIN matched_case mc ON mc.id = br.case_id
  JOIN hardware hw ON hw.id = br.hardware_id
  WHERE br.error IS NULL
),
latest AS (
  SELECT DISTINCT ON (history_fingerprint)
    history_fingerprint, id, case_id, context_id, hardware_id, commit_repo_url,
    commit_sha, commit_timestamp
  FROM members
  ORDER BY history_fingerprint, commit_timestamp DESC, id DESC
),
page AS MATERIALIZED (
  SELECT
    l.history_fingerprint, l.id, l.case_id, l.context_id, l.hardware_id,
    l.commit_repo_url, l.commit_sha, l.commit_timestamp
  FROM latest l
  JOIN "case" cs ON cs.id = l.case_id
  JOIN context ctx ON ctx.id = l.context_id
  JOIN hardware hw ON hw.id = l.hardware_id
  ORDER BY l.commit_timestamp DESC, l.history_fingerprint DESC
  LIMIT $3
),
counts AS (
  SELECT br.history_fingerprint, count(*)::bigint AS point_count
  FROM benchmark_result br
  JOIN commit c ON c.id = br.commit_id
  JOIN page p ON p.history_fingerprint = br.history_fingerprint
  WHERE br.error IS NULL
    AND c.sha = c.fork_point_sha
    AND c."timestamp" IS NOT NULL
  GROUP BY br.history_fingerprint
)
SELECT p.history_fingerprint
FROM page p
JOIN counts cnt ON cnt.history_fingerprint = p.history_fingerprint
ORDER BY p.commit_timestamp DESC, p.history_fingerprint DESC
`

const seriesProfileFingerprintSQL = `
WITH members AS MATERIALIZED (
  SELECT
    br.history_fingerprint, br.id, br."timestamp" AS result_timestamp,
    br.unit, br.data, br.case_id, br.context_id, br.hardware_id,
    br.commit_repo_url, c.sha AS commit_sha, c."timestamp" AS commit_timestamp
  FROM benchmark_result br
  JOIN commit c ON c.id = br.commit_id
  WHERE br.error IS NULL
    AND br.history_fingerprint = $2
    AND c.sha = c.fork_point_sha
    AND c."timestamp" IS NOT NULL
),
latest AS (
  SELECT DISTINCT ON (history_fingerprint)
    history_fingerprint, id, result_timestamp, unit, data,
    case_id, context_id, hardware_id, commit_repo_url, commit_sha, commit_timestamp
  FROM members
  ORDER BY history_fingerprint, commit_timestamp DESC, id DESC
),
counts AS (
  SELECT history_fingerprint, count(*)::bigint AS point_count FROM members GROUP BY history_fingerprint
)
SELECT
  l.history_fingerprint, l.id AS latest_result_id, l.result_timestamp AS latest_result_timestamp,
  l.commit_sha AS latest_commit_sha, l.commit_timestamp AS latest_commit_timestamp,
  l.commit_repo_url, l.unit AS latest_unit, l.data AS latest_data, cnt.point_count,
  cs.name AS case_name, cs.tags AS case_tags, ctx.tags AS context_tags,
  hw.id AS hardware_id, hw.name AS hardware_name, hw.type AS hardware_type, hw.hash AS hardware_hash
FROM latest l
JOIN counts cnt ON cnt.history_fingerprint = l.history_fingerprint
JOIN "case" cs ON cs.id = l.case_id
JOIN context ctx ON ctx.id = l.context_id
JOIN hardware hw ON hw.id = l.hardware_id
ORDER BY l.commit_timestamp DESC, l.history_fingerprint DESC
LIMIT $1
`

const historyProfileSQL = `
SELECT
  br.id, br.history_fingerprint, br."timestamp", br.unit, br.mean, br.data,
  br.change_annotations, hw.hash AS hardware_hash, c.sha AS commit_sha,
  c.repository AS commit_repository, c.message AS commit_message,
  c."timestamp" AS commit_timestamp
FROM benchmark_result br
JOIN hardware hw ON hw.id = br.hardware_id
JOIN commit c ON c.id = br.commit_id
WHERE br.error IS NULL
  AND br.history_fingerprint = $1
  AND c.sha = c.fork_point_sha
  AND c."timestamp" IS NOT NULL
ORDER BY c."timestamp", br.id
`

const resultDetailProfileSQL = `
SELECT
  br.id, br.run_id, br.run_tags, br.run_reason, br.batch_id,
  br."timestamp", br.commit_repo_url, br.history_fingerprint,
  br.unit, br.time_unit, br.iterations, br.error, br.data, br.times,
  br.mean, br.min, br.max, br.median, br.q1, br.q3, br.stdev, br.iqr,
  br.validation, br.optional_benchmark_info, br.change_annotations,
  cs.name AS case_name, cs.tags AS case_tags,
  ctx.tags AS context_tags, inf.tags AS info_tags,
  hw.id AS hardware_id, hw.type AS hardware_type, hw.name AS hardware_name, hw.hash AS hardware_hash,
  c.id AS commit_id, c.sha AS commit_sha, c.repository AS commit_repository,
  c.message AS commit_message, c."timestamp" AS commit_timestamp
FROM benchmark_result br
JOIN "case" cs ON cs.id = br.case_id
JOIN context ctx ON ctx.id = br.context_id
JOIN info inf ON inf.id = br.info_id
JOIN hardware hw ON hw.id = br.hardware_id
LEFT JOIN commit c ON c.id = br.commit_id
WHERE br.id = $1
`

const resultListDefaultProfileSQL = `
SELECT
  br.id, br.run_id, br.run_tags, br."timestamp", br.unit, br.data, br.error,
  br.history_fingerprint, c.sha AS commit_sha, c.repository AS commit_repository,
  c."timestamp" AS commit_timestamp
FROM benchmark_result br
LEFT JOIN commit c ON c.id = br.commit_id
ORDER BY br.id DESC
LIMIT $1
`

const resultListFilteredRecentProfileSQL = `
SELECT
  br.id, br.run_id, br.run_tags, br."timestamp", br.unit, br.data, br.error,
  br.history_fingerprint, c.sha AS commit_sha, c.repository AS commit_repository,
  c."timestamp" AS commit_timestamp
FROM benchmark_result br
LEFT JOIN commit c ON c.id = br.commit_id
WHERE br."timestamp" >= $2::timestamp
ORDER BY br.id DESC
LIMIT $1
`

const compareResultLookupProfileSQL = `
SELECT
  br.id, br.run_id, br.history_fingerprint, br.unit, br.data, br.error,
  br.commit_id, c."timestamp" AS commit_timestamp
FROM benchmark_result br
LEFT JOIN commit c ON c.id = br.commit_id
WHERE br.id = $1
`

const compareHistoryAsOfProfileSQL = `
SELECT
  br.id, br.history_fingerprint, br."timestamp", br.unit, br.mean, br.data,
  br.change_annotations, hw.hash AS hardware_hash, c.sha AS commit_sha,
  c.repository AS commit_repository, c.message AS commit_message,
  c."timestamp" AS commit_timestamp
FROM benchmark_result br
JOIN hardware hw ON hw.id = br.hardware_id
JOIN commit c ON c.id = br.commit_id
WHERE br.error IS NULL
  AND br.history_fingerprint = $1
  AND c.sha = c.fork_point_sha
  AND c."timestamp" IS NOT NULL
  AND c."timestamp" <= (
    SELECT c2."timestamp"
    FROM benchmark_result br2
    LEFT JOIN commit c2 ON c2.id = br2.commit_id
    WHERE br2.id = $2
  )
ORDER BY c."timestamp", br.id
`
