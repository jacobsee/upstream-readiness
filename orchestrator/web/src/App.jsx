import { useEffect, useState } from "react";
import {
  Alert,
  Bullseye,
  Content,
  Nav,
  NavExpandable,
  NavGroup,
  NavItem,
  NavList,
  Page,
  Masthead,
  MastheadBrand,
  MastheadContent,
  MastheadLogo,
  MastheadMain,
  PageSection,
  PageSidebar,
  PageSidebarBody,
  Spinner,
  Split,
  SplitItem,
  Toolbar,
  ToolbarContent,
  ToolbarItem,
} from "@patternfly/react-core";
import PluginPanel, { NoPluginSelected } from "./PluginPanel.jsx";
import WorkflowsView from "./WorkflowsView.jsx";
import InstancesView from "./InstancesView.jsx";
import RunsView from "./RunsView.jsx";

// SidebarPluginItem renders a NavItem for a plugin, with an optional
// live badge slot on the right. The badge is a plugin-owned MFE mounted
// in "sidebar-badge" mode - the orchestrator never inspects what it
// puts there.
function SidebarPluginItem({ manifest, isActive, onClick, kind, label }) {
  const hasBadge = (manifest.mfeSlots ?? []).some((s) => s.slot === "sidebar-badge");
  return (
    <NavItem
      isActive={isActive}
      onClick={onClick}
      component="button"
    >
      <Split hasGutter style={{ alignItems: "center" }}>
        <SplitItem isFilled>{label}</SplitItem>
        {hasBadge && (
          <SplitItem>
            <PluginPanel
              manifest={manifest}
              mode="sidebar-badge"
              slotContext={{ kind }}
            />
          </SplitItem>
        )}
      </Split>
    </NavItem>
  );
}

export default function App() {
  const [plugins, setPlugins] = useState(null);
  const [workflows, setWorkflows] = useState(null);
  // selection: {kind: 'plugin' | 'workflow' | 'instances', id}
  const [selection, setSelection] = useState(null);
  const [loadError, setLoadError] = useState(null);

  useEffect(() => {
    Promise.all([
      fetch("/api/plugins").then(r => { if (!r.ok) throw new Error(`/api/plugins → ${r.status}`); return r.json(); }),
      fetch("/api/workflows").then(r => { if (!r.ok) throw new Error(`/api/workflows → ${r.status}`); return r.json(); }),
    ])
      .then(([ps, wfs]) => {
        setPlugins(ps);
        setWorkflows(wfs ?? []);
        setSelection({ kind: "runs" });
      })
      .catch(e => setLoadError(String(e)));
  }, []);

  const activeWorkflows = (workflows ?? []).filter(wf => !wf.removed_at);
  const removedWorkflows = (workflows ?? []).filter(wf => wf.removed_at);

  // A purged workflow is gone from the server entirely: drop it here and
  // go back to the runs dashboard.
  const handlePurged = (id) => {
    setWorkflows(prev => (prev ?? []).filter(wf => wf.id !== id));
    setSelection({ kind: "runs" });
  };

  const masthead = (
    <Masthead>
      <MastheadMain>
        <MastheadBrand>
          <MastheadLogo component="div">Upstream Readiness</MastheadLogo>
        </MastheadBrand>
      </MastheadMain>
      <MastheadContent>
        <Toolbar>
          <ToolbarContent>
            <ToolbarItem>
              {/* <Content component="small"> */}
              {/*   orchestrator shell - plugin panes are owned by each plugin */}
              {/* </Content> */}
            </ToolbarItem>
          </ToolbarContent>
        </Toolbar>
      </MastheadContent>
    </Masthead>
  );

  const sidebar = (
    <PageSidebar>
      <PageSidebarBody>
        <Nav aria-label="Main">
          <NavGroup title="Dashboard">
            <NavList>
              <NavItem
                isActive={selection?.kind === "runs"}
                onClick={() => setSelection({ kind: "runs" })}
                component="button"
              >
                Runs
              </NavItem>
            </NavList>
          </NavGroup>
          {activeWorkflows.length > 0 && (
            <NavGroup title="Workflows">
              <NavList>
                {activeWorkflows.map(wf => (
                  <NavItem
                    key={wf.id}
                    isActive={selection?.kind === "workflow" && selection.id === wf.id}
                    onClick={() => setSelection({ kind: "workflow", id: wf.id })}
                    component="button"
                  >
                    {wf.display_name || wf.id}
                  </NavItem>
                ))}
              </NavList>
            </NavGroup>
          )}
          {removedWorkflows.length > 0 && (
            <NavList>
              {/* Workflows whose YAML file is gone: history kept until purged. */}
              <NavExpandable
                title={`Removed workflows (${removedWorkflows.length})`}
                isActive={removedWorkflows.some(wf => selection?.kind === "workflow" && selection.id === wf.id)}
              >
                {removedWorkflows.map(wf => (
                  <NavItem
                    key={wf.id}
                    isActive={selection?.kind === "workflow" && selection.id === wf.id}
                    onClick={() => setSelection({ kind: "workflow", id: wf.id })}
                    component="button"
                  >
                    <span style={{ color: "#6a6e73", textDecoration: "line-through" }}>
                      {wf.display_name || wf.id}
                    </span>
                  </NavItem>
                ))}
              </NavExpandable>
            </NavList>
          )}
          {(plugins ?? []).length > 0 && (
            <NavGroup title="Instances">
              <NavList>
                {(plugins ?? []).map(p => (
                  <SidebarPluginItem
                    key={`inst-${p.id}`}
                    manifest={p}
                    kind="instances"
                    label={p.displayName}
                    isActive={selection?.kind === "instances" && selection.id === p.id}
                    onClick={() => setSelection({ kind: "instances", id: p.id })}
                  />
                ))}
              </NavList>
            </NavGroup>
          )}
          {(plugins ?? []).length > 0 && (
            <NavGroup title="Plugins">
              <NavList>
                {(plugins ?? []).map(p => (
                  <SidebarPluginItem
                    key={p.id}
                    manifest={p}
                    kind="plugin"
                    label={p.displayName}
                    isActive={selection?.kind === "plugin" && selection.id === p.id}
                    onClick={() => setSelection({ kind: "plugin", id: p.id })}
                  />
                ))}
              </NavList>
            </NavGroup>
          )}
        </Nav>
      </PageSidebarBody>
    </PageSidebar>
  );

  let body;
  if (loadError) {
    body = <Alert variant="danger" title="Could not load data">{loadError}</Alert>;
  } else if (plugins === null || workflows === null) {
    body = <Bullseye><Spinner aria-label="Loading" /></Bullseye>;
  } else if (!selection) {
    body = <NoPluginSelected />;
  } else if (selection.kind === "plugin") {
    const manifest = plugins.find(p => p.id === selection.id);
    // Plugins that declare "plugin-home" replace the ad-hoc action panel
    // with their own landing surface. The orchestrator only picks the
    // mode; the plugin's MFE decides what to render.
    const hasHome = manifest && (manifest.mfeSlots ?? []).some(s => s.slot === "plugin-home");
    body = manifest ? (
      <PluginPanel manifest={manifest} mode={hasHome ? "plugin-home" : "action"} />
    ) : <NoPluginSelected />;
  } else if (selection.kind === "instances") {
    const manifest = plugins.find(p => p.id === selection.id);
    body = manifest ? <InstancesView manifest={manifest} /> : <NoPluginSelected />;
  } else if (selection.kind === "runs") {
    body = <RunsView workflows={workflows} onOpenWorkflow={(id) => setSelection({ kind: "workflow", id })} />;
  } else {
    const wf = workflows.find(w => w.id === selection.id);
    body = wf ? <WorkflowsView key={wf.id} workflow={wf} plugins={plugins} onPurged={handlePurged} /> : <NoPluginSelected />;
  }

  return (
    <Page masthead={masthead} sidebar={sidebar}>
      <PageSection style={{ height: "100%" }}>{body}</PageSection>
    </Page>
  );
}
