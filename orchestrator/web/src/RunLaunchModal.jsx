import { useState } from "react";
import {
  Alert,
  Button,
  Form,
  FormGroup,
  FormHelperText,
  HelperText,
  HelperTextItem,
  Label,
  LabelGroup,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  Stack,
  StackItem,
  TextInput,
} from "@patternfly/react-core";

// Steps downstream of fromStep (inclusive) in declaration order, and
// every other step. Mirrors workflow.RerunPlan on the server, which stays
// authoritative.
export function rerunPlan(steps, fromStep) {
  const children = {};
  for (const s of steps) {
    for (const d of s.depends_on ?? []) (children[d] ??= []).push(s.id);
  }
  const down = new Set();
  const walk = (id) => {
    if (down.has(id)) return;
    down.add(id);
    (children[id] ?? []).forEach(walk);
  };
  walk(fromStep);
  return {
    rerun: steps.filter((s) => down.has(s.id)).map((s) => s.id),
    reuse: steps.filter((s) => !down.has(s.id)).map((s) => s.id),
  };
}

// Initial form values: carried-over values for inputs the workflow still
// declares, else each input's default.
export function initialInputs(decls, from) {
  return Object.fromEntries(
    (decls ?? []).map((d) => [d.name, from && d.name in from ? from[d.name] : d.default ?? ""]),
  );
}

// RunLaunchModal collects run inputs before starting a run, and for a
// rerun shows which steps run again and which are carried over.
// onSubmit(inputs) starts the run and rejects with an Error on failure.
export default function RunLaunchModal({ workflow, title, plan, sourceRunID, startValues, onSubmit, onClose }) {
  const decls = workflow.inputs ?? [];
  const [values, setValues] = useState(startValues);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);

  const missing = decls.filter((d) => d.required && !values[d.name]);

  const submit = async (e) => {
    e?.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await onSubmit(values);
    } catch (err) {
      setError(String(err.message ?? err));
      setBusy(false);
    }
  };

  return (
    <Modal variant={ModalVariant.medium} isOpen onClose={busy ? undefined : onClose} aria-label={title}>
      <ModalHeader title={title} />
      <ModalBody>
        <Stack hasGutter>
          {plan && (
            <>
              <StackItem>
                <strong>Runs again</strong>
                <LabelGroup numLabels={20} style={{ marginTop: 4 }}>
                  {plan.rerun.map((id) => <Label key={id} color="blue" isCompact>{id}</Label>)}
                </LabelGroup>
              </StackItem>
              {plan.reuse.length > 0 && (
                <StackItem>
                  <strong>Reused from <code style={{ fontSize: 12 }}>{sourceRunID}</code></strong>
                  <LabelGroup numLabels={20} style={{ marginTop: 4 }}>
                    {plan.reuse.map((id) => <Label key={id} color="teal" isCompact>{id}</Label>)}
                  </LabelGroup>
                  <div style={{ color: "#6a6e73", fontSize: 12, marginTop: 4 }}>
                    These steps are not executed; their recorded outputs feed the steps that run.
                  </div>
                </StackItem>
              )}
            </>
          )}
          {decls.length > 0 && (
            <StackItem>
              <Form onSubmit={submit}>
                {decls.map((d) => (
                  <FormGroup key={d.name} label={d.name} isRequired={d.required} fieldId={`input-${d.name}`}>
                    <TextInput
                      id={`input-${d.name}`}
                      value={values[d.name] ?? ""}
                      onChange={(_e, v) => setValues((prev) => ({ ...prev, [d.name]: v }))}
                      isDisabled={busy}
                    />
                    {d.description && (
                      <FormHelperText>
                        <HelperText>
                          <HelperTextItem>{d.description}</HelperTextItem>
                        </HelperText>
                      </FormHelperText>
                    )}
                  </FormGroup>
                ))}
              </Form>
            </StackItem>
          )}
          {error && (
            <StackItem>
              <Alert variant="danger" isInline title="Could not start the run">
                <pre style={{ margin: 0, whiteSpace: "pre-wrap", fontSize: 12 }}>{error}</pre>
              </Alert>
            </StackItem>
          )}
        </Stack>
      </ModalBody>
      <ModalFooter>
        <Button variant="primary" onClick={submit} isDisabled={busy || missing.length > 0} isLoading={busy}>
          {plan ? "Rerun" : "Run"}
        </Button>
        <Button variant="link" onClick={onClose} isDisabled={busy}>
          Cancel
        </Button>
      </ModalFooter>
    </Modal>
  );
}
