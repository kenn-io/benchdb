import axios from "axios";
import { expect, it } from "vitest";

import { getBenchDB } from "../api/benchdb";
import { parseCIReportQuery } from "../router";
import { loadCIReport } from "./loader";

it("preserves reserved characters in saved report reads and finalization", async () => {
  const requests: string[] = [];
  const report = { status: "success", selected_run_ids: ["ci/run?part=a#b%2F"], baseline: "explicit_run", baseline_run_id: "recorded/baseline" };
  const client = getBenchDB(axios.create({
    adapter: async (config) => {
      requests.push(`${config.method} ${axios.getUri(config)}`);
      return { status: 200, statusText: "OK", headers: {}, config, data: report };
    },
  }));
  const query = parseCIReportQuery("?run_ids=ci%2Frun%3Fpart%3Da%23b%252F&baseline_run_ids=recorded%2Fbaseline&saved=true");

  expect(await loadCIReport(client, query)).toEqual(report);
  await client.finalizeRunReport(query.runIDs, { result_ids: ["result-a"], baseline: "explicit_run", baseline_run_id: "baseline-run" });

  expect(requests).toEqual([
    "get /api/ci/reports/ci%2Frun%3Fpart%3Da%23b%252F",
    "post /api/ci/reports/ci%2Frun%3Fpart%3Da%23b%252F",
  ]);
});

it.each([
  ["different exact baseline", { baseline: "explicit_run", baseline_run_id: "other-baseline" }],
  ["automatic baseline", { baseline: "latest_default", baseline_run_id: "recorded-baseline" }],
  ["legacy snapshot", { baseline: "latest_default" }],
  ["missing resolved identity", { baseline: "explicit_run" }],
])("rejects a saved report with %s without requesting a live comparison", async (_name, selection) => {
  const requests: string[] = [];
  const client = getBenchDB(axios.create({
    adapter: async (config) => {
      requests.push(`${config.method} ${axios.getUri(config)}`);
      return { status: 200, statusText: "OK", headers: {}, config, data: { ...selection, status: "success" } };
    },
  }));
  const query = parseCIReportQuery("?run_ids=contender&baseline_run_ids=recorded-baseline&saved=true");

  await expect(loadCIReport(client, query)).rejects.toThrow("Saved report does not match the requested baseline.");
  expect(requests).toEqual(["get /api/ci/reports/contender"]);
});
