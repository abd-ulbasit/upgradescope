/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Dev proxy: `npm run dev` against a local `upgradescope serve` on :8080.
// Production never proxies — the SPA is embedded in the Go binary and
// served same-origin.
//
// base "./" makes index.html reference its assets relatively, so the
// dashboard also works under a path prefix (https://host/upgradescope/)
// behind a proxy that strips it. The API client uses relative URLs too.
export default defineConfig({
  base: "./",
  plugins: [react()],
  server: {
    proxy: {
      "/api": "http://localhost:8080",
      "/healthz": "http://localhost:8080",
    },
  },
  test: {
    // Plain node by default; component tests opt into a DOM with a
    // `// @vitest-environment happy-dom` docblock.
    environment: "node",
    include: ["src/**/*.test.{ts,tsx}"],
  },
});
