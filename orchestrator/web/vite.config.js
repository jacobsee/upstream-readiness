import { createRequire } from "node:module";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// One React for the shell and every plugin MFE, served by the
// orchestrator itself (no CDN). Plugin bundles leave these specifiers
// as bare imports; index.html's import map points each one at a
// /vendor/<file>.js entry built here. The shell's own code imports
// React normally, so Rollup puts React in a shared chunk that both the
// shell and the vendor entries import - one module instance per page.
//
// Keys are the bare specifiers plugins import; values are the file
// names under /vendor/ (must match the import map in index.html).
const VENDOR = {
  react: "react",
  "react/jsx-runtime": "react-jsx-runtime",
  "react-dom": "react-dom",
  "react-dom/client": "react-dom-client",
};

const require = createRequire(import.meta.url);
const VIRTUAL = "vendor:";

// vendorShims turns `vendor:<specifier>` into an ES module re-exporting
// every export of that package. React ships CommonJS, so the export
// list is read from the installed package rather than `export *`,
// which can't enumerate CommonJS names.
function vendorShims() {
  return {
    name: "vendor-shims",
    resolveId(id) {
      if (id.startsWith(VIRTUAL)) return "\0" + id;
    },
    load(id) {
      if (!id.startsWith("\0" + VIRTUAL)) return;
      const spec = id.slice(VIRTUAL.length + 1);
      const names = Object.keys(require(spec)).filter(
        (n) => n !== "default" && /^[A-Za-z_$][\w$]*$/.test(n),
      );
      const from = JSON.stringify(spec);
      return `export { ${names.join(", ")} } from ${from};\nexport { default } from ${from};\n`;
    },
    // Dev server: answer /vendor/<file>.js with the same shim, resolved
    // against Vite's pre-bundled deps, so plugin MFEs share the dev
    // shell's React too.
    configureServer(server) {
      server.middlewares.use((req, _res, next) => {
        const m = req.url?.match(/^\/vendor\/([\w-]+)\.js$/);
        const spec = m && Object.keys(VENDOR).find((k) => VENDOR[k] === m[1]);
        if (spec) req.url = `/@id/__x00__${VIRTUAL}${spec}`;
        next();
      });
    },
  };
}

export default defineConfig({
  plugins: [react(), vendorShims()],
  build: {
    outDir: "dist",
    emptyOutDir: true,
    rollupOptions: {
      input: {
        index: "index.html",
        ...Object.fromEntries(
          Object.entries(VENDOR).map(([spec, file]) => [`vendor/${file}`, VIRTUAL + spec]),
        ),
      },
      // Vendor entries are an import contract with plugin bundles: keep
      // their exports and give them stable, unhashed names.
      preserveEntrySignatures: "strict",
      output: {
        entryFileNames: (chunk) =>
          chunk.name.startsWith("vendor/") ? "[name].js" : "assets/[name]-[hash].js",
      },
    },
  },
  server: {
    proxy: {
      "/api": "http://localhost:8080",
      "/plugins": "http://localhost:8080",
    },
  },
});
