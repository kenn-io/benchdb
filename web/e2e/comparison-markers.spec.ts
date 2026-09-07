import { test, expect } from "@playwright/test";

for (const hasHistory of [true, false]) {
  test(`shows selected comparison results with ${hasHistory ? "main history" : "no history"}`, async ({ page }, testInfo) => {
    const baselineDate = "2026-01-06T12:00:00Z";
    const contenderDate = "2026-01-08T12:00:00Z";
    const result = (id: string) => ({
      id, benchmark_id: "demo-benchmark", batch_id: null, run_id: `run-${id}`,
      run_reason: id === "baseline" ? "commit" : "pull-request", run_tags: {},
      tags: { name: "index-build-time" }, context: {}, info: {},
      hardware: { id: "host", type: "machine", name: "runner-a", hash: "hardware" },
      commit: { id: `commit-${id}`, sha: `sha-${id}`, message: id === "baseline" ? "Build search index" : "Change index builder",
        repository: "https://github.com/example/project", timestamp: id === "baseline" ? baselineDate : contenderDate },
      commit_repo_url: "https://github.com/example/project", unit: "s", less_is_better: true,
      single_value_summary: id === "baseline" ? 100 : 140, single_value_summary_type: "mean", iterations: 1,
      data: [id === "baseline" ? 100 : 140], times: null, time_unit: null, error: null,
      stats: { min: null, max: null, mean: 100, median: null, q1: null, q3: null, stdev: null, iqr: null },
      history_fingerprint: "demo-history", timestamp: id === "baseline" ? baselineDate : contenderDate,
    });
    await page.route("**/api/**", async (route) => {
      const path = new URL(route.request().url()).pathname;
      if (path === "/api/compare/benchmark-results") {
        await route.fulfill({ json: {
          unit: "s", less_is_better: true,
          baseline: { benchmark_result_id: "baseline", single_value_summary: 100, run_id: "run-baseline" },
          contender: { benchmark_result_id: "contender", single_value_summary: 140, run_id: "run-contender" },
          analysis: { pairwise: null, lookback_z_score: { z_score: -3, z_threshold: 2, regression_indicated: true, improvement_indicated: false } },
        } });
      } else if (path.startsWith("/api/benchmark-results/")) {
        await route.fulfill({ json: result(path.split("/").at(-1)!) });
      } else if (path.startsWith("/api/history/")) {
        await route.fulfill({ json: { history_fingerprint: "demo-history", samples: hasHistory ? [1, 2, 3, 4, 5, 6].map(day => ({
          benchmark_result_id: day === 6 ? "baseline" : `history-${day}`, commit_hash: `commit-${day}`, commit_message: "Build search index",
          commit_repository: "https://github.com/example/project", commit_timestamp: `2026-01-0${day}T12:00:00Z`,
          result_timestamp: `2026-01-0${day}T12:00:00Z`, single_value_summary: 100, single_value_summary_type: "mean",
          unit: "s", data: [100], mean: 100, hardware_hash: "hardware", run_tags: {}, info: {}, change_annotations: {},
          zscorestats: { begins_distribution_change: false, is_step: false, is_outlier: false, segment_id: 0,
            rolling_mean: 100, rolling_mean_excluding_this_commit: 100, rolling_stddev: 5, residual: 0 },
        })) : [] } });
      } else {
        await route.fulfill({ status: 404, json: {} });
      }
    });
    await page.setViewportSize({ width: 1280, height: 1000 });
    await page.goto("/compare?baseline=baseline&contender=contender");
    const baseline = page.getByRole("button", { name: "Baseline: 100 s", exact: true });
    const contender = page.getByRole("button", { name: "Contender: 140 s", exact: true });
    await expect(baseline).toBeVisible();
    await expect(contender).toBeVisible();
    for (const width of [1280, 390]) {
      await page.setViewportSize({ width, height: 1000 });
      await expect.poll(async () => {
        const plot = await page.locator(".u-over").boundingBox();
        const marks = await Promise.all([baseline.boundingBox(), contender.boundingBox()]);
        return !!plot && marks.every(mark => mark && mark.x >= plot.x && mark.x + mark.width <= plot.x + plot.width
          && mark.y >= plot.y && mark.y + mark.height <= plot.y + plot.height);
      }).toBe(true);
    }
    await page.setViewportSize({ width: 1280, height: 1000 });
    await page.screenshot({ path: testInfo.outputPath("comparison.png"), fullPage: true });
    const plot = (await page.locator(".u-over").boundingBox())!;
    await page.mouse.move(plot.x + plot.width * 0.1, plot.y + plot.height / 2);
    await page.mouse.down();
    await page.mouse.move(plot.x + plot.width * 0.4, plot.y + plot.height / 2, { steps: 8 });
    await page.mouse.up();
    await expect(page.getByRole("button", { name: "Reset zoom" })).toBeVisible();
    await page.getByRole("button", { name: "Reset zoom" }).click();
    await expect(contender).toBeVisible();
    await contender.click();
    await expect(page).toHaveURL(/\/results\/contender$/);
  });
}
