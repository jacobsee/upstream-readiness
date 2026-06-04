// Small PatternFly form helpers shared by the repo's plugin MFEs.
// Consumed through each plugin's Vite `@pluginkit` alias; bundled into
// the plugin's own MFE (React stays external, PatternFly is deduped to
// the consuming plugin's copy).
import { FormGroup, FormHelperText, HelperText, HelperTextItem, TextArea, TextInput } from "@patternfly/react-core";

export function Field({ label, id, value, onChange, helper, placeholder, required }) {
  return (
    <FormGroup label={label} isRequired={required} fieldId={id}>
      <TextInput
        id={id}
        value={value ?? ""}
        onChange={(_, v) => onChange(v)}
        placeholder={placeholder}
        aria-label={label}
      />
      {helper && (
        <FormHelperText>
          <HelperText>
            <HelperTextItem>{helper}</HelperTextItem>
          </HelperText>
        </FormHelperText>
      )}
    </FormGroup>
  );
}

export function MultilineField({ label, id, value, onChange, helper, placeholder, required, rows = 8 }) {
  return (
    <FormGroup label={label} isRequired={required} fieldId={id}>
      <TextArea
        id={id}
        value={value ?? ""}
        onChange={(_, v) => onChange(v)}
        placeholder={placeholder}
        aria-label={label}
        rows={rows}
        resizeOrientation="vertical"
        style={{ fontFamily: "monospace", fontSize: 12 }}
      />
      {helper && (
        <FormHelperText>
          <HelperText>
            <HelperTextItem>{helper}</HelperTextItem>
          </HelperText>
        </FormHelperText>
      )}
    </FormGroup>
  );
}

// LogTail renders the bounded command-log tail plugins put in their
// progress payloads.
export function LogTail({ log, maxHeight = 300 }) {
  if (!log) return null;
  return (
    <pre style={{
      margin: 0, fontSize: 11, background: "#1b1d21", color: "#d4d4d4",
      padding: 12, borderRadius: 4, overflow: "auto", maxHeight,
      whiteSpace: "pre-wrap", wordBreak: "break-all",
    }}>{log}</pre>
  );
}
