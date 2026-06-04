// Full PatternFly stylesheet (fonts and images included), loaded once by
// the shell for itself and every plugin MFE.
import "@patternfly/patternfly/patternfly.css";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import App from "./App.jsx";

createRoot(document.getElementById("root")).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
