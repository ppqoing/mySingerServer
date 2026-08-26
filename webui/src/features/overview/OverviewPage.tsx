import { useCallback, useState } from "react";
import { Link } from "react-router-dom";
import { appApi, type AppApi } from "../../api/appApi";
import { databaseErrorText } from "../../api/errorText";
import { AsyncState } from "../../components/AsyncState";
import { usePolling } from "../../hooks/usePolling";
import { byteText } from "../groups/format";
import "../operational-pages.css";
import "./overview.css";

export interface OverviewPageProps {
  readonly api?: AppApi;
}

export function OverviewPage({ api = appApi }: OverviewPageProps) {
  const [refreshVersion, setRefreshVersion] = useState(0);
  const refresh = useCallback(() => setRefreshVersion(version => version + 1), []);
  const runtime = usePolling(useCallback((signal: AbortSignal) => api.getRuntimeStatus(signal), [api]), { dependencies: [api, refreshVersion] });
  const databaseConnected = runtime.data?.databaseState === "connected";
  const agents = usePolling(useCallback((signal: AbortSignal) => api.listAgents(signal), [api]), {
    dependencies: [api, refreshVersion], enabled: databaseConnected
  });
  const tasks = usePolling(useCallback((signal: AbortSignal) => api.listTasks(signal), [api]), {
    dependencies: [api, refreshVersion], enabled: databaseConnected
  });
  const analysis = usePolling(useCallback((signal: AbortSignal) => api.getAnalysisStatus(signal), [api]), {
    dependencies: [api, refreshVersion], enabled: databaseConnected
  });
  // 一次聚合统计查询（无筛选，三类汇总），替代此前按类各发一次 size=1 列表请求。
  const groupStats = usePolling(useCallback((signal: AbortSignal) => api.getGroupsStats(undefined, signal), [api]), { dependencies: [api, refreshVersion], enabled: databaseConnected });

  const activeTasks = (tasks.data ?? []).filter(task => task.status !== "done" && task.status !== "failed").length;
  const failedTasks = (tasks.data ?? []).filter(task => task.status === "failed").length;
  const scanErrors = (tasks.data ?? []).reduce((sum, task) => sum + task.scanErrors, 0);
  const onlineAgents = (agents.data ?? []).filter(agent => agent.online && agent.identityState === "claimed").length;
  const pendingAgents = (agents.data ?? []).filter(agent => agent.identityState === "pending").length;
  const conflictAgents = (agents.data ?? []).filter(agent => agent.identityState === "conflict").length;
  // AnalysisStatus.last 没有完成时间字段，无法精确比较"最近扫描完成 vs 最近分析"；
  // 采用启发：运行中→"分析中"；无 last 或仍有扫描在进行（完成后需重新分析）→"待分析"；否则"已分析"。
  const analysisStage = analysis.data === undefined
    ? "分析状态未知"
    : analysis.data.running ? "分析中"
    : !analysis.data.last || activeTasks > 0 ? "待分析"
    : "已分析";
  const stale = runtime.data !== undefined && runtime.data.databaseState !== "connected"
    && (agents.data !== undefined || tasks.data !== undefined || analysis.data !== undefined || groupStats.data !== undefined);

  return (
    <section aria-labelledby="overview-heading" className="operational-page">
      <header className="operational-page__header operational-surface">
        <h1 id="overview-heading">总览</h1>
        <p>当前控制台已加载的运行状态；未加载的数据不会被推断为全局总量。</p>
        <button onClick={refresh} type="button">刷新</button>
      </header>
      {runtime.data?.restarting ? (
        <section className="operational-surface overview-banner" role="status">
          <p>Manager 正在重启，稍后自动恢复。</p>
        </section>
      ) : null}
      {runtime.data?.databaseState === "connecting" ? <section className="operational-surface"><p>正在连接 PostgreSQL…</p></section> : null}
      {runtime.data?.databaseState === "error" ? (
        <section className="operational-surface" role="alert">
          <h2>PostgreSQL 未连接</h2><p>{databaseErrorText(runtime.data.databaseErrorCode)}</p><Link to="/settings">打开 GUI 设置</Link>
        </section>
      ) : null}
      {runtime.error ? <AsyncState error={runtime.error.message} state="error" /> : null}
      {databaseConnected ? (
        <section aria-label="主流程状态" className="operational-surface overview-flow">
          <span>扫描中 {activeTasks}</span>
          <span aria-hidden="true" className="overview-flow__arrow">→</span>
          <span>{analysisStage}</span>
          <span aria-hidden="true" className="overview-flow__arrow">→</span>
          <span>待处理组 {groupStats.data?.groups ?? "—"}</span>
        </section>
      ) : null}
      {stale ? <p className="overview-stale" role="note">以下为断开前最后数据</p> : null}
      <div className="operational-grid">
      <Link aria-label="Agent 概况" className="operational-surface overview-card" to="/agents">
        <h2>Agent</h2>
        {agents.error ? <AsyncState error={agents.error.message} state="error" /> : null}
        {!agents.data && agents.loading ? <AsyncState state="loading" /> : null}
        {agents.data ? (
          <>
            <p>在线 {onlineAgents} / 共 {agents.data.length}</p>
            <p>待识别 {pendingAgents} / 身份冲突 {conflictAgents}</p>
          </>
        ) : null}
      </Link>
      <Link aria-label="扫描概况" className="operational-surface overview-card" to="/scans">
        <h2>扫描任务</h2>
        {tasks.error ? <AsyncState error={tasks.error.message} state="error" /> : null}
        {!tasks.data && tasks.loading ? <AsyncState state="loading" /> : null}
        {tasks.data ? (
          <>
            <p>进行中 {activeTasks} / 当前已加载 {tasks.data.length}</p>
            <p>失败 {failedTasks} · 扫描错误 {scanErrors}</p>
          </>
        ) : null}
      </Link>
      <Link aria-label="分析概况" className="operational-surface overview-card" to="/analysis">
        <h2>一筛分析</h2>
        {analysis.error ? <AsyncState error={analysis.error.message} state="error" /> : null}
        {!analysis.data && analysis.loading ? <AsyncState state="loading" /> : null}
        {analysis.data ? <p>当前状态：{analysis.data.running ? "运行中" : "空闲"}</p> : null}
        {analysis.data?.last ? (
          <>
            <p>已扫描文件 {analysis.data.last.filesScanned}</p>
            <p>当前已加载：写入重复组 {analysis.data.last.groupsWritten}，写入重复成员 {analysis.data.last.membersWritten}</p>
          </>
        ) : null}
        {analysis.data?.lastErr ? <p role="alert">{analysis.data.lastErr}</p> : null}
      </Link>
      <Link aria-label="重复组概况" className="operational-surface overview-card" to="/groups">
        <h2>重复组</h2>
        {groupStats.error ? <AsyncState error={groupStats.error.message} state="error" /> : null}
        {groupStats.data === undefined && groupStats.loading ? <AsyncState state="loading" /> : null}
        {groupStats.data !== undefined ? <p>可回收空间 {byteText(groupStats.data.wastedBytes)}（共 {groupStats.data.groups} 组）</p> : null}
      </Link>
      </div>
    </section>
  );
}
