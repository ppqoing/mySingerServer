import { useState } from "react";
import { HashRouter, Navigate, Route, Routes } from "react-router-dom";
import { appApi, type AppApi } from "../api/appApi";
import { AppShell } from "../components/AppShell";
import { AgentsPage } from "../features/agents/AgentsPage";
import { AnalysisPage } from "../features/analysis/AnalysisPage";
import { DeleteStatusPanel } from "../features/deletion/DeleteStatusPanel";
import { GroupsPage } from "../features/groups/GroupsPage";
import type { DeleteReviewSnapshot } from "../features/groups/deleteReview";
import { OverviewPage } from "../features/overview/OverviewPage";
import { ScansPage } from "../features/scans/ScansPage";
import { GUISettingsPage } from "../features/settings/GUISettingsPage";

export interface AppProps {
  readonly api?: AppApi;
}

export function App({ api = appApi }: AppProps) {
  const [activeDeleteTaskId, setActiveDeleteTaskId] = useState<string>();
  const [deleteExecutionPending, setDeleteExecutionPending] = useState(false);
  const [deleteReviewSnapshot, setDeleteReviewSnapshot] = useState<DeleteReviewSnapshot>();

  return (
    <HashRouter>
      <AppShell>
        <Routes>
          <Route path="/overview" element={<OverviewPage api={api} />} />
          <Route path="/agents" element={<AgentsPage api={api} />} />
          <Route path="/scans" element={<ScansPage api={api} />} />
          <Route path="/analysis" element={<AnalysisPage api={api} />} />
          <Route
            path="/groups"
            element={
              <GroupsPage
                activeDeleteTaskId={activeDeleteTaskId}
                api={api}
                deleteExecutionPending={deleteExecutionPending}
                deleteReviewSnapshot={deleteReviewSnapshot}
                onActiveDeleteTaskIdChange={setActiveDeleteTaskId}
                onDeleteExecutionPendingChange={setDeleteExecutionPending}
                onDeleteReviewSnapshotChange={setDeleteReviewSnapshot}
              />
            }
          />
          <Route path="/audit" element={<DeleteStatusPanel api={api} taskId={activeDeleteTaskId} />} />
          <Route path="/settings" element={<GUISettingsPage api={api} />} />
          <Route path="/" element={<Navigate replace to="/overview" />} />
          <Route path="*" element={<Navigate replace to="/overview" />} />
        </Routes>
      </AppShell>
    </HashRouter>
  );
}
