import { defineConfig } from "vitest/config";

// The shape the family's dashboards use: lcov for ship's gates, every src file counted (a file no test imports
// is uncovered, not invisible).
export default defineConfig({
  test: {
    coverage: {
      provider: "v8",
      reporter: ["lcov"],
      reportsDirectory: "../coverage/ui",
      include: ["src/**/*.{ts,tsx}"],
      exclude: ["src/main.tsx", "src/**/*.test.{ts,tsx}"],
    },
  },
});
