import { expect, test } from "@playwright/test";

test("saved reports reevaluate without hiding numerical comparisons", async ({ page }) => {
  const comparison = (name: string, index: number) => ({
    status: "insufficient", name, tags: { name }, context: {}, info: {},
    hardware: { id: "fixture", type: "machine", name: "benchmark-host", hash: "fixture" },
    history_fingerprint: `series-${index}`, unit: "s", less_is_better: true,
    history_points: 2, history_required: 10, reason: "History contains 2 results; at least 10 required",
    contender: { result_id: `result-${index}`, run_id: "pull", result_timestamp: "2026-10-24T12:00:00Z", commit_sha: "cccccccc", commit_timestamp: "2026-10-24T12:00:00Z", error: null, single_value_summary: 12, single_value_summary_type: "mean" },
    baseline: { result_id: `base-${index}`, run_id: "main", result_timestamp: "2026-10-23T12:00:00Z", commit_sha: "bbbbbbbb", commit_timestamp: "2026-10-23T12:00:00Z", single_value_summary: 10, single_value_summary_type: "mean", unit: "s" },
    analysis: { pairwise: { percent_change: -20, percent_threshold: 5, regression_indicated: true, improvement_indicated: false }, lookback_z_score: null as { z_score: number; z_threshold: number; regression_indicated: boolean; improvement_indicated: boolean } | null },
    error: null, links: { result: `/results/result-${index}`, compare: `/compare?baseline=base-${index}&contender=result-${index}`, series: `/series/series-${index}` },
  });
  const original = {
    repository: "https://github.com/example/project", commit_sha: "cccccccc", selected_run_ids: ["pull"], missing_run_ids: [],
    baseline: "explicit_run", baseline_run_id: "main", status: "skipped", status_reason: "Statistical analysis unavailable",
    threshold: 5, threshold_z: 2, report_url: "/ci/report?run_ids=pull&saved=true", evaluated_at: "2026-10-24T12:00:00Z", analysis_version: 1, evaluation: 1,
    summary: { runs: 1, missing_runs: 0, contender_results: 2, compared: 2, analyzed: 0, regressions: 0, improvements: 0, benchmark_errors: 0, missing_baseline: 0, not_comparable: 0 },
    runs: [{ run_id: "pull", run_reason: "pull-request", run_tags: {}, commit: null, baseline_run_id: "main", baseline_commit: null, commits_skipped: [], baseline_error: null, comparisons: [comparison("parser/startup", 1), comparison("parser/recent", 2)] }],
  };
  const revised = structuredClone(original);
  revised.evaluation = 2; revised.status = "failure"; revised.status_reason = "1 regression detected";
  revised.summary.analyzed = 1; revised.summary.regressions = 1;
  revised.runs[0].comparisons[0].status = "regressed";
  revised.runs[0].comparisons[0].history_points = 10;
  revised.runs[0].comparisons[0].reason = "Regression detected";
  revised.runs[0].comparisons[0].analysis.lookback_z_score = { z_score: -4, z_threshold: 2, regression_indicated: true, improvement_indicated: false };
  let reevaluations = 0;
  await page.route("**/api/**", (route) => route.fulfill({ status: 404, json: {} }));
  await page.route("**/api/auth/capabilities", (route) => route.fulfill({ json: { can_write_results: true } }));
  await page.route((url) => url.pathname.startsWith("/api/ci/reports/pull"), (route) => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith("/reevaluate")) { reevaluations++; return route.fulfill({ json: { evaluation: 2 } }); }
    return route.fulfill({ json: url.searchParams.get("evaluation") === "2" ? revised : original });
  });
  await page.goto("/ci/report?run_ids=pull&saved=true");
  await expect(page.getByText("0 of 2 analyzed", { exact: true })).toBeVisible();
  await expect(page.getByRole("row", { name: /parser\/recent/ })).toContainText("+20.0%");
  await page.getByRole("button", { name: "Reevaluate", exact: true }).click();
  await expect(page).toHaveURL(/evaluation=2/);
  await expect(page.getByText("1 of 2 analyzed", { exact: true })).toBeVisible();
  expect(reevaluations).toBe(1);
  for (const [name, width, height] of [["desktop", 1440, 1000], ["phone", 390, 844]] as const) {
    await page.setViewportSize({ width, height });
    if (name === "phone") {
      const close = page.getByRole("button", { name: "Close navigation", exact: true }).last();
      await expect(close).toBeHidden();
    }
    await page.evaluate(() => document.fonts.ready);
    await page.screenshot({ path: `../.cache/artifacts/report-reevaluation-${name}.png`, fullPage: true });
  }
  await page.goto("/ci/report?run_ids=pull&saved=true");
  await expect(page.getByText("0 of 2 analyzed", { exact: true })).toBeVisible();
});
