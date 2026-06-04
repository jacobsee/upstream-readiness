// Greeter MFE. One component, multiple slots - the orchestrator mounts
// it with a `mode` prop telling it which slot it lives in:
//
//   mode="action"           per-action runtime panel (default)
//   mode="instance-config"  create/edit a plugin instance
//
// The orchestrator stores whatever blob `saveInstance` is called with;
// the plugin's gRPC handler reads the same blob back on Execute.

import { useEffect, useState } from "react";
import {
  Alert,
  Button,
  Card,
  CardBody,
  CardTitle,
  Content,
  Form,
  FormGroup,
  FormHelperText,
  HelperText,
  HelperTextItem,
  Split,
  SplitItem,
  Stack,
  StackItem,
  TextInput,
  Title,
} from "@patternfly/react-core";

export default function GreeterPanel(props) {
  const mode = props.mode || "action";
  if (mode === "instance-config") return <InstanceConfigPanel {...props} />;
  if (mode === "step-runtime") return <StepRuntimePanel {...props} />;
  if (mode === "sidebar-badge") return <SidebarBadgePanel {...props} />;
  if (mode === "plugin-home") return <HomePanel {...props} />;
  return <ActionPanel {...props} />;
}

// ── step-runtime slot ─────────────────────────────────────────────────────────
// Rendered inside the WorkflowsView's per-running-step card. slotContext
// carries the latest opaque progress payload the plugin emitted.
function StepRuntimePanel({ slotContext }) {
  const progress = slotContext?.progress;
  if (!progress) {
    return <em style={{ color: "#6a6e73" }}>Composing greeting…</em>;
  }
  return (
    <Content component="small">
      Greeting <code>{progress.name}</code> · phase <code>{progress.phase}</code>
    </Content>
  );
}

// ── sidebar-badge slot ────────────────────────────────────────────────────────
// A tiny live indicator next to the sidebar entry. This plugin shows the
// number of registered instances.
function SidebarBadgePanel({ pluginApi }) {
  const [count, setCount] = useState(null);
  useEffect(() => {
    let cancelled = false;
    pluginApi.listInstances().then((rows) => { if (!cancelled) setCount(rows.length); }).catch(() => {});
    return () => { cancelled = true; };
  }, [pluginApi]);
  if (count == null) return null;
  return <span style={{ fontSize: 11, color: "#6a6e73" }}>{count}</span>;
}

// ── plugin-home slot ──────────────────────────────────────────────────────────
// Replaces the ad-hoc action panel when the user picks the plugin in
// the sidebar. Simple overview: instance list + link into the ad-hoc
// action flow via the existing panel would double-render, so this is
// intentionally minimal.
function HomePanel({ manifest, pluginApi }) {
  const [instances, setInstances] = useState([]);
  useEffect(() => { pluginApi.listInstances().then(setInstances).catch(() => {}); }, [pluginApi]);
  return (
    <Stack hasGutter>
      <StackItem>
        <Title headingLevel="h1" size="2xl">{manifest.displayName}</Title>
        <Content component="small">Trivial greeter. Plugin id <code>{manifest.id}</code>, version {manifest.version}.</Content>
      </StackItem>
      <StackItem>
        <Card>
          <CardTitle>Registered instances ({instances.length})</CardTitle>
          <CardBody>
            {instances.length === 0 ? (
              <em style={{ color: "#6a6e73" }}>None yet. Create one from the Instances tab.</em>
            ) : (
              <ul style={{ margin: 0, paddingLeft: 20 }}>
                {instances.map((i) => (
                  <li key={i.id}><strong>{i.display_name || i.id}</strong> - suffix <code>{i.config_blob?.suffix || "(none)"}</code></li>
                ))}
              </ul>
            )}
          </CardBody>
        </Card>
      </StackItem>
    </Stack>
  );
}

// ── instance config slot ──────────────────────────────────────────────────────

function InstanceConfigPanel({ pluginApi, slotContext }) {
  const existing = slotContext?.existing;
  const onSaved = slotContext?.onSaved;
  const [displayName, setDisplayName] = useState(existing?.display_name ?? "");
  const [suffix, setSuffix] = useState(existing?.config_blob?.suffix ?? "");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState(null);

  useEffect(() => {
    setDisplayName(existing?.display_name ?? "");
    setSuffix(existing?.config_blob?.suffix ?? "");
  }, [existing?.id]);

  const handleSubmit = async (e) => {
    e.preventDefault();
    setSaving(true);
    setError(null);
    try {
      const saved = await pluginApi.saveInstance({
        id: existing?.id,
        display_name: displayName,
        config_blob: { suffix },
      });
      if (onSaved) onSaved(saved);
    } catch (err) {
      setError(String(err));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Stack hasGutter>
      <StackItem>
        <Card>
          <CardTitle>
            {existing ? `Edit instance - ${existing.display_name || existing.id}` : "New Greeter instance"}
          </CardTitle>
          <CardBody>
            <Form onSubmit={handleSubmit}>
              <FormGroup label="Display name" isRequired fieldId="display-name">
                <TextInput
                  id="display-name"
                  value={displayName}
                  onChange={(_, v) => setDisplayName(v)}
                  placeholder="e.g. friendly-greeter"
                />
              </FormGroup>
              <FormGroup label="Suffix" fieldId="suffix">
                <TextInput
                  id="suffix"
                  value={suffix}
                  onChange={(_, v) => setSuffix(v)}
                  placeholder="e.g. Have a great day!"
                />
                <FormHelperText>
                  <HelperText>
                    <HelperTextItem>
                      Appended to every greeting this instance produces.
                    </HelperTextItem>
                  </HelperText>
                </FormHelperText>
              </FormGroup>
              <Button type="submit" variant="primary" isLoading={saving} isDisabled={saving || !displayName}>
                {existing ? "Save changes" : "Create instance"}
              </Button>
            </Form>
          </CardBody>
        </Card>
      </StackItem>
      {error && (
        <StackItem>
          <Alert variant="danger" title="Save failed" isInline>
            <pre style={{ margin: 0, fontSize: 12, whiteSpace: "pre-wrap" }}>{error}</pre>
          </Alert>
        </StackItem>
      )}
    </Stack>
  );
}

// ── action runtime slot ───────────────────────────────────────────────────────

function ActionPanel({ manifest, pluginApi }) {
  const [instances, setInstances] = useState([]);
  const [instanceID, setInstanceID] = useState("");
  const [name, setName] = useState("world");
  const [message, setMessage] = useState(null);
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    pluginApi
      .listInstances()
      .then((rows) => {
        setInstances(rows);
        if (rows.length > 0) setInstanceID(rows[0].id);
      })
      .catch((e) => setError(String(e)));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  async function onSubmit(event) {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const result = await pluginApi.execute("greet", { name }, { instanceId: instanceID });
      setMessage(result.message);
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }

  if (instances.length === 0) {
    return (
      <Alert variant="info" title="No instances registered" isInline>
        Create a Greeter instance under the Instances tab before invoking actions ad-hoc.
      </Alert>
    );
  }

  return (
    <Stack hasGutter>
      <StackItem>
        <Title headingLevel="h1" size="2xl">{manifest.displayName}</Title>
        <Content component="small">
          plugin <code>{manifest.id}</code> · v{manifest.version}
        </Content>
      </StackItem>
      <StackItem>
        <Card>
          <CardTitle>Send a greeting</CardTitle>
          <CardBody>
            <Form onSubmit={onSubmit}>
              <FormGroup label="Instance" isRequired fieldId="instance-id">
                <select
                  id="instance-id"
                  value={instanceID}
                  onChange={(e) => setInstanceID(e.target.value)}
                  style={{ width: "100%", padding: "6px 8px", borderRadius: 4 }}
                >
                  {instances.map((i) => (
                    <option key={i.id} value={i.id}>{i.display_name || i.id}</option>
                  ))}
                </select>
              </FormGroup>
              <FormGroup label="Recipient" fieldId="greeter-name" isRequired>
                <Split hasGutter>
                  <SplitItem isFilled>
                    <TextInput
                      id="greeter-name"
                      value={name}
                      onChange={(_e, v) => setName(v)}
                      isRequired
                    />
                  </SplitItem>
                  <SplitItem>
                    <Button type="submit" isLoading={busy} isDisabled={busy || !instanceID}>
                      Greet
                    </Button>
                  </SplitItem>
                </Split>
              </FormGroup>
            </Form>
          </CardBody>
        </Card>
      </StackItem>
      {message && (
        <StackItem>
          <Alert variant="success" title={message} isInline />
        </StackItem>
      )}
      {error && (
        <StackItem>
          <Alert variant="danger" title="Plugin call failed" isInline>
            {error}
          </Alert>
        </StackItem>
      )}
    </Stack>
  );
}
