import { defineConfig } from "orval";

export default defineConfig({
  benchdb: {
    input: "../api/openapi.yaml",
    output: {
      target: "src/lib/api/benchdb.ts",
      // Axios generation does not apply Orval's urlEncodeParameters option.
      client: (generators) => ({
        ...generators.axios,
        client: (verb, options, ...rest) => generators.axios.client(verb, {
          ...options,
          route: options.route.replace(/\$\{([^}]+)\}/g, "${encodeURIComponent($1)}"),
        }, ...rest),
      }),
    },
  },
});
