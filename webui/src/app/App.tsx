import { useState } from "react";
import { HashRouter, Link, Navigate, Route, Routes } from "react-router-dom";
import { appApi, type AppApi } from "../api/appApi";
import { AppShell } from "../components/AppShell";
import { AgentsPage } from "../features/agents/AgentsPage";
import { AnalysisPage } from "../features/analysis/AnalysisPage";
import { DeleteStatusPanel } from "../features/deletion/DeleteStatusPanel";
import { GroupsPage } from "../features/groups/GroupsPage";
import type { DeleteReviewSnapshot } from "../features/groups/deleteReview";
import "../features/operational-pages.css";
import { OverviewPage } from "../features/overview/OverviewPage";
import { ScansPage } from "../features/scans/ScansPage";
import { GUISettingsPage } from "../features/settings/GUISettingsPage";

export interface AppProps {
  readonly api?: AppApi;
}

function NotFound() {
  return (
    <section aria-labelledby="not-found-heading" className="operational-page">
      <header className="operational-page__header operational-surface">
        <h1 id="not-found-heading">页面不存在</h1>
        <p>地址有误或页面已被移动。</p>
        <Link to="/overview">返回总览</Link>
      </header>
    </section>
  );
}

export function App({ api = appApi }: AppProps) {
  // 进行中的删除任务仅在会话内跟踪；刷新后通过审计页任务列表找回，不再写 sessionStorage。
  const [activeDeleteTaskId, setActiveDeleteTaskId] = useState<string | undefined>();
  const [deleteExecutionPending, setDeleteExecutionPending] = useState(false);
  const [deleteReviewSnapshot, setDeleteReviewSnapshot] = useState<DeleteReviewSnapshot>();
  // 审计页一键重试（P1-6）：DeleteStatusPanel 交办 fileIds，GroupsPage 消费后恢复选择并打开删除准备。
  const [retryFileIds, setRetryFileIds] = useState<readonly number[]>();

  return (
    <HashRouter>
      <AppShell api={api}>
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
                onRetryFileIdsConsumed={() => setRetryFileIds(undefined)}
                retryFileIds={retryFileIds}
              />
            }
          />
          <Route
            path="/audit"
            element={
              <DeleteStatusPanel
                api={api}
                deleteReviewSnapshot={deleteReviewSnapshot}
                onRetryRequest={fileIds => {
                  setRetryFileIds(fileIds);
                  window.location.hash = "#/groups";
                }}
                taskId={activeDeleteTaskId}
              />
            }
          />
          <Route path="/settings" element={<GUISettingsPage api={api} />} />
          <Route path="/" element={<Navigate replace to="/overview" />} />
          <Route path="*" element={<NotFound />} />
        </Routes>
      </AppShell>
    </HashRouter>
  );
}
