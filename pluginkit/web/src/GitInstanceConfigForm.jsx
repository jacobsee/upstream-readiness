// Instance-config form for pluginkit's gitenv.InstanceConfig shape (git
// identity, fork template, default make targets, SSH key/known_hosts,
// env vars). Shared by every plugin whose instances use that shape so
// the form and the Go struct can't drift. The blob is persisted through
// pluginApi.saveInstance; the orchestrator stores it opaquely.
import { useEffect, useState } from "react";
import {
  ActionGroup,
  Alert,
  Button,
  Card,
  CardBody,
  CardTitle,
  Content,
  Form,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import { Field, MultilineField } from "./fields.jsx";

const EMPTY_INSTANCE = {
  display_name: "",
  git_user_name: "",
  git_user_email: "",
  fork_url_template: "",
  default_make_targets: "",
  ssh_private_key: "",
  ssh_known_hosts: "",
  env_vars: "",
};

// The env_vars field is authored as KEY=VALUE per line. This shape is
// forgiving of trailing whitespace and blank lines and lets us round-
// trip through a single TextArea without inventing a repeated-row UI.
function envVarsMapToText(m) {
  if (!m || typeof m !== "object") return "";
  return Object.entries(m)
    .map(([k, v]) => `${k}=${v}`)
    .join("\n");
}

function envVarsTextToMap(text) {
  const out = {};
  const errors = [];
  const lines = (text || "").split(/\r?\n/);
  lines.forEach((raw, idx) => {
    const line = raw.trim();
    if (!line || line.startsWith("#")) return;
    const eq = line.indexOf("=");
    if (eq <= 0) {
      errors.push(`line ${idx + 1}: expected KEY=VALUE`);
      return;
    }
    const key = line.slice(0, eq).trim();
    const value = line.slice(eq + 1);
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) {
      errors.push(`line ${idx + 1}: "${key}" is not a valid env var name`);
      return;
    }
    out[key] = value;
  });
  return { map: out, errors };
}

function existingToForm(existing) {
  if (!existing) return EMPTY_INSTANCE;
  const cfg = existing.config_blob || {};
  return {
    display_name: existing.display_name ?? "",
    git_user_name: cfg.git_user_name ?? "",
    git_user_email: cfg.git_user_email ?? "",
    fork_url_template: cfg.fork_url_template ?? "",
    default_make_targets: (cfg.default_make_targets ?? []).join(", "),
    ssh_private_key: cfg.ssh_private_key ?? "",
    ssh_known_hosts: cfg.ssh_known_hosts ?? "",
    env_vars: envVarsMapToText(cfg.env_vars),
  };
}

// newTitle is the card title when creating (e.g. "New Go Dependency
// Updater instance").
export function GitInstanceConfigForm({ pluginApi, slotContext, newTitle }) {
  const existing = slotContext?.existing;
  const onSaved = slotContext?.onSaved;
  const [form, setForm] = useState(() => existingToForm(existing));
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState(null);

  useEffect(() => {
    setForm(existingToForm(existing));
  }, [existing?.id]);

  const set = (k) => (v) => setForm((f) => ({ ...f, [k]: v }));

  const handleSubmit = async (e) => {
    e.preventDefault();
    setSaving(true);
    setError(null);
    try {
      const parsedEnv = envVarsTextToMap(form.env_vars);
      if (parsedEnv.errors.length > 0) {
        throw new Error("environment variables: " + parsedEnv.errors.join("; "));
      }
      const config = {
        git_user_name: form.git_user_name,
        git_user_email: form.git_user_email,
        fork_url_template: form.fork_url_template,
        default_make_targets: form.default_make_targets
          ? form.default_make_targets.split(",").map((s) => s.trim()).filter(Boolean)
          : [],
        ssh_private_key: form.ssh_private_key,
        ssh_known_hosts: form.ssh_known_hosts,
        env_vars: parsedEnv.map,
      };
      const saved = await pluginApi.saveInstance({
        id: existing?.id,
        display_name: form.display_name,
        config_blob: config,
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
            {existing ? `Edit instance - ${existing.display_name || existing.id}` : newTitle}
          </CardTitle>
          <CardBody>
            <Form onSubmit={handleSubmit}>
              <Field
                label="Display name"
                id="display-name"
                value={form.display_name}
                onChange={set("display_name")}
                placeholder="e.g. acme-corp-bot"
                helper="Shown in the workflow editor and run history."
                required
              />
              <Field
                label="Git user name"
                id="git-user-name"
                value={form.git_user_name}
                onChange={set("git_user_name")}
                placeholder="Upstream Readiness Bot"
                required
              />
              <Field
                label="Git user email"
                id="git-user-email"
                value={form.git_user_email}
                onChange={set("git_user_email")}
                placeholder="bot@example.com"
                required
              />
              <Field
                label="Fork URL template"
                id="fork-url-template"
                value={form.fork_url_template}
                onChange={set("fork_url_template")}
                placeholder="git@github.com:acme-bot/{repo}.git"
                helper="A template with {repo} substituted from the upstream URL's basename. Each step can override by setting push_url explicitly."
              />
              <Field
                label="Default make targets"
                id="make-targets"
                value={form.default_make_targets}
                onChange={set("default_make_targets")}
                placeholder="test, lint"
                helper="Comma-separated. Each step can override these."
              />
              <MultilineField
                label="SSH private key"
                id="ssh-private-key"
                value={form.ssh_private_key}
                onChange={set("ssh_private_key")}
                placeholder={"-----BEGIN OPENSSH PRIVATE KEY-----\n...\n-----END OPENSSH PRIVATE KEY-----"}
                helper="PEM contents. Written to a per-run scratch file at mode 0600; the host's ~/.ssh is never consulted."
                required
                rows={10}
              />
              <MultilineField
                label="SSH known_hosts"
                id="ssh-known-hosts"
                value={form.ssh_known_hosts}
                onChange={set("ssh_known_hosts")}
                placeholder={"github.com ssh-ed25519 AAAA...\ngitlab.com ssh-ed25519 AAAA..."}
                helper="Optional. When present, host-key checking is strict; otherwise the plugin uses accept-new for the run."
                rows={5}
              />
              <MultilineField
                label="Environment variables"
                id="env-vars"
                value={form.env_vars}
                onChange={set("env_vars")}
                placeholder={"GOPROXY=direct\nCGO_ENABLED=0\nMAKEFLAGS=-j4"}
                helper="Optional. One KEY=VALUE per line. Set for every git / go / go mod vendor / make invocation. Keys reserved by the plugin's isolation (GIT_*, HOME, GOPATH, GOSUMDB, GONOSUMDB, GOFLAGS) cannot be overridden."
                rows={5}
              />
              <ActionGroup>
                <Button
                  type="submit"
                  variant="primary"
                  isLoading={saving}
                  isDisabled={saving || !form.display_name || !form.git_user_name || !form.git_user_email || !form.ssh_private_key}
                >
                  {existing ? "Save changes" : "Create instance"}
                </Button>
              </ActionGroup>
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

// GitInstanceSummary is the instance-list-item line for the same shape.
export function GitInstanceSummary({ slotContext }) {
  const cfg = slotContext?.instance?.config_blob ?? {};
  const bits = [];
  if (cfg.git_user_email) bits.push(`commits as ${cfg.git_user_email}`);
  if (cfg.fork_url_template) bits.push(`fork ${cfg.fork_url_template}`);
  if ((cfg.default_make_targets ?? []).length > 0) bits.push(`make ${cfg.default_make_targets.join(", ")}`);
  if (cfg.ssh_known_hosts) bits.push("known_hosts pinned");
  const envCount = cfg.env_vars ? Object.keys(cfg.env_vars).length : 0;
  if (envCount > 0) bits.push(`${envCount} env var${envCount === 1 ? "" : "s"}`);
  return (
    <Content component="small">
      {bits.length > 0 ? bits.join(" · ") : <em>no configuration recorded</em>}
    </Content>
  );
}
