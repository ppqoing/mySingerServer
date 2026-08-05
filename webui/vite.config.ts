import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "../internal/gui/web",
    emptyOutDir: true,
    rollupOptions: {
      input: {
        index: "index.html",
        groups: "groups.html"
      }
    }
  },
  test: {
    environment: "jsdom",
    setupFiles: "./src/test/setup.ts",
    restoreMocks: true,
    globals: true
  }
});
