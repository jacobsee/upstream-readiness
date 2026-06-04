import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Library build: produce a single ESM module the orchestrator can
// dynamic-import (served through the orchestrator via GetAsset). React
// and ReactDOM are externalized so the plugin runs against the shell's
// React instance at runtime: the shell's import map points these
// specifiers at its own /vendor/ entries. PatternFly's JS is bundled
// in; its stylesheet is loaded once by the shell.
const SHARED_EXTERNALS = [
  "react",
  "react/jsx-runtime",
  "react-dom",
  "react-dom/client",
];

export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "dist",
    emptyOutDir: true,
    lib: {
      entry: "src/GreeterPanel.jsx",
      formats: ["es"],
      fileName: () => "greeter-mfe.js",
    },
    rollupOptions: {
      external: SHARED_EXTERNALS,
    },
  },
});
