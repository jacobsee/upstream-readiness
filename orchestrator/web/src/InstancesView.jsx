// InstancesView renders the management UI for a plugin's instances.
// The list, "new", and "edit" panels are all owned by the orchestrator;
// the actual form is mounted from the plugin's MFE in instance-config
// mode. The orchestrator never sees inside the config blob.

import { useEffect, useState } from "react";
import {
  Alert,
  Button,
  Card,
  CardBody,
  CardTitle,
  DataList,
  DataListCell,
  DataListItem,
  DataListItemCells,
  DataListItemRow,
  EmptyState,
  EmptyStateBody,
  Split,
  SplitItem,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import PluginPanel from "./PluginPanel.jsx";

export default function InstancesView({ manifest }) {
  const [instances, setInstances] = useState(null);
  const [error, setError] = useState(null);
  const [editing, setEditing] = useState(null); // null | "new" | instance.id

  const load = () =>
    fetch(`/api/instances?plugin=${encodeURIComponent(manifest.id)}`)
      .then((r) => {
        if (!r.ok) throw new Error(`/api/instances → ${r.status}`);
        return r.json();
      })
      .then((rows) => setInstances(rows ?? []))
      .catch((e) => setError(String(e)));

  useEffect(() => {
    load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [manifest.id]);

  if (error) {
    return (
      <Alert variant="danger" title="Could not load instances" isInline>
        {error}
      </Alert>
    );
  }
  if (instances === null) {
    return null;
  }

  if (editing) {
    const existing = editing === "new" ? null : instances.find((i) => i.id === editing);
    return (
      <Stack hasGutter>
        <StackItem>
          <Split hasGutter style={{ alignItems: "center" }}>
            <SplitItem isFilled>
              <strong style={{ fontSize: 18 }}>
                {existing ? `Edit ${existing.display_name || existing.id}` : "New instance"}
              </strong>
            </SplitItem>
            <SplitItem>
              <Button variant="link" onClick={() => setEditing(null)}>
                Back to list
              </Button>
            </SplitItem>
          </Split>
        </StackItem>
        <StackItem isFilled>
          <PluginPanel
            manifest={manifest}
            mode="instance-config"
            slotContext={{
              existing,
              onSaved: async () => {
                await load();
                setEditing(null);
              },
            }}
          />
        </StackItem>
      </Stack>
    );
  }

  return (
    <Stack hasGutter>
      <StackItem>
        <Split hasGutter style={{ alignItems: "center" }}>
          <SplitItem isFilled>
            <strong style={{ fontSize: 18 }}>
              {manifest.displayName} - instances
            </strong>
          </SplitItem>
          <SplitItem>
            <Button variant="primary" onClick={() => setEditing("new")}>
              New instance
            </Button>
          </SplitItem>
        </Split>
      </StackItem>
      <StackItem isFilled>
        {instances.length === 0 ? (
          <Card>
            <CardBody>
              <EmptyState titleText="No instances yet" headingLevel="h4">
                <EmptyStateBody>
                  Create one to use this plugin from workflows or run actions ad-hoc.
                </EmptyStateBody>
              </EmptyState>
            </CardBody>
          </Card>
        ) : (
          <Card>
            <CardTitle>Registered instances</CardTitle>
            <CardBody>
              <DataList aria-label="instances" isCompact>
                {instances.map((inst) => {
                  const hasCustomRow = (manifest.mfeSlots ?? []).some(
                    (s) => s.slot === "instance-list-item",
                  );
                  return (
                    <DataListItem key={inst.id} aria-labelledby={`inst-${inst.id}`}>
                      <DataListItemRow>
                        <DataListItemCells
                          dataListCells={[
                            <DataListCell key="name" isFilled>
                              <strong id={`inst-${inst.id}`}>
                                {inst.display_name || inst.id}
                              </strong>
                              <div style={{ fontSize: 12, color: "#6a6e73" }}>
                                <code>{inst.id}</code>
                              </div>
                              {hasCustomRow && (
                                <div style={{ marginTop: 6 }}>
                                  <PluginPanel
                                    manifest={manifest}
                                    mode="instance-list-item"
                                    slotContext={{ instance: inst }}
                                  />
                                </div>
                              )}
                            </DataListCell>,
                            <DataListCell key="actions">
                              <Split hasGutter>
                                <SplitItem>
                                  <Button variant="secondary" onClick={() => setEditing(inst.id)}>
                                    Edit
                                  </Button>
                                </SplitItem>
                                <SplitItem>
                                  <Button
                                    variant="link"
                                    onClick={async () => {
                                      if (!confirm(`Delete instance ${inst.display_name || inst.id}?`)) return;
                                      const r = await fetch(`/api/instances/${encodeURIComponent(inst.id)}`, { method: "DELETE" });
                                      if (r.ok) load();
                                      else setError(`delete → ${r.status}`);
                                    }}
                                  >
                                    Delete
                                  </Button>
                                </SplitItem>
                              </Split>
                            </DataListCell>,
                          ]}
                        />
                      </DataListItemRow>
                    </DataListItem>
                  );
                })}
              </DataList>
            </CardBody>
          </Card>
        )}
      </StackItem>
    </Stack>
  );
}
