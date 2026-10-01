import axios from "axios";
import { expect, it } from "vitest";

import { getBenchDB } from "../api/benchdb";
import { parseCIReportQuery } from "../router";
import { loadCIReport } from "./loader";

it("preserves reserved characters in saved report reads and finalization", async () => {
  const requests: string[] = [];
  const report = { status: "success", selected_run_ids: ["ci/run?part=a#b%2F"] };
  const client = getBenchDB(axios.create({
    adapter: async (config) => {
      requests.push(`${config.method} ${axios.getUri(config)}`);
      return { status: 200, statusText: "OK", headers: {}, config, data: report };
    },
  }));
  const query = parseCIReportQuery("?run_ids=ci%2Frun%3Fpart%3Da%23b%252F&saved=true");

  expect(await loadCIReport(client, query)).toEqual(report);
  await client.finalizeRunReport(query.runIDs, { result_ids: ["result-a"] });

  expect(requests).toEqual([
    "get /api/ci/reports/ci%2Frun%3Fpart%3Da%23b%252F",
    "post /api/ci/reports/ci%2Frun%3Fpart%3Da%23b%252F",
  ]);
});
