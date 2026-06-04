import { fileURLToPath } from "node:url";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

const SHARED_EXTERNALS = [
  "react",
  "react/jsx-runtime",
  "react-dom",
  "react-dom/client",
];

export default defineConfig({
  plugins: [react()],
  resolve: {
    // Shared plugin UI (instance form, patch-queue editor) lives in
    // pluginkit/web/src and is bundled into this MFE.
    alias: {
      "@pluginkit": fileURLToPath(new URL("../../pluginkit/web/src", import.meta.url)),
    },
    // pluginkit/web has no node_modules of its own: resolve its bare
    // imports from this package so there's exactly one PatternFly copy.
    dedupe: ["react", "react-dom", "@patternfly/react-core"],
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    lib: {
      entry: "src/GoDepPanel.jsx",
      formats: ["es"],
      fileName: () => "go-dep-mfe.js",
    },
    rollupOptions: {
      external: SHARED_EXTERNALS,
    },
  },
});
