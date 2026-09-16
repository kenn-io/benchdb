import { defineConfig } from "orval";

export default defineConfig({
  benchdb: {
    input: "../api/openapi.yaml",
    output: {
      target: "src/lib/api/benchdb.ts",
      client: "axios",
      urlEncodeParameters: true,
    },
  },
});
