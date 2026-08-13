import { useCallback } from "react";
import { Link } from "react-router-dom";
import { appApi, type AppApi } from "../../api/appApi";
import { AsyncState } from "../../components/AsyncState";
import { usePolling } from "../../hooks/usePolling";
import "../operational-pages.css";

export interface OverviewPageProps {
  readonly api?: AppApi;
}

export function OverviewPage({ api = appApi }: OverviewPageProps) {
  const runtime = usePolling(useCallback((signal: AbortSignal) => api.getRuntimeStatus(signal), [api]), { dependencies: [api] });
  const databaseConnected = runtime.data?.databaseState === "connected";
  const agents = usePolling(useCallback((signal: AbortSignal) => api.listAgents(signal), [api]), {
    dependencies: [api], enabled: databaseConnected
  });
  const tasks = usePolling(useCallback((signal: AbortSignal) => api.listTasks(signal), [api]), {
    dependencies: [api], enabled: databaseConnected
  });
  const analysis = usePolling(useCallback((signal: AbortSignal) => api.getAnalysisStatus(signal), [api]), {
    dependencies: [api], enabled: databaseConnected,
    isTerminal: status => !status.running
  });
  const activeTasks = (tasks.data ?? []).filter(task => task.status !== "done" && task.status !== "failed").length;
  const onlineAgents = (agents.data ?? []).filter(agent => agent.online).length;

  return (
    <section aria-labelledby="overview-heading" className="operational-page">
      <header className="operational-page__header operational-surface"><h1 id="overview-heading">总览</h1><p>当前控制台已加载的运行状态；未加载的数据不会被推断为全局总量。</p></header>
      {runtime.data?.databaseState === "connecting" ? <section className="operational-surface"><p>正在连接 PostgreSQL…</p></section> : null}
      {runtime.data?.databaseState === "error" ? (
        <section className="operational-surface" role="alert">
          <h2>PostgreSQL 未连接</h2><p>业务数据暂不可用，Manager 会继续尝试恢复连接。</p><Link to="/settings">打开 GUI 设置</Link>
        </section>
      ) : null}
      {runtime.error ? <AsyncState error={runtime.error.message} state="error" /> : null}
      <div className="operational-grid">
      <section aria-label="Agent 概况" className="operational-surface"><h2>Agent</h2>
        {agents.error ? <AsyncState error={agents.error.message} state="error" /> : null}
        {!agents.data && agents.loading ? <AsyncState state="loading" /> : null}
        {agents.data ? <p>在线 {onlineAgents} / 共 {agents.data.length}</p> : null}
      </section>
      <section aria-label="扫描概况" className="operational-surface"><h2>扫描任务</h2>
        {tasks.error ? <AsyncState error={tasks.error.message} state="error" /> : null}
        {!tasks.data && tasks.loading ? <AsyncState state="loading" /> : null}
        {tasks.data ? <p>进行中 {activeTasks} / 当前已加载 {tasks.data.length}</p> : null}
      </section>
      <section aria-label="分析概况" className="operational-surface"><h2>一筛分析</h2>
        {analysis.error ? <AsyncState error={analysis.error.message} state="error" /> : null}
        {!analysis.data && analysis.loading ? <AsyncState state="loading" /> : null}
        {analysis.data ? <p>当前状态：{analysis.data.running ? "运行中" : "空闲"}</p> : null}
        {analysis.data?.last ? <p>当前已加载：写入重复组 {analysis.data.last.groupsWritten}，写入重复成员 {analysis.data.last.membersWritten}</p> : null}
        {analysis.data?.lastErr ? <p role="alert">{analysis.data.lastErr}</p> : null}
      </section>
      </div>
    </section>
  );
}
