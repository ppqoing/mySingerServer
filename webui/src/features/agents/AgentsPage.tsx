import { useCallback, useState } from "react";
import { appApi, type AppApi } from "../../api/appApi";
import type { AgentStatus } from "../../api/contracts";
import { AsyncState } from "../../components/AsyncState";
import { usePolling } from "../../hooks/usePolling";
import "../operational-pages.css";
import "./agents.css";

export interface AgentsPageProps {
  readonly api?: AppApi;
}

function compareAgents(left: AgentStatus, right: AgentStatus) {
  if (left.online !== right.online) return left.online ? -1 : 1;
  const byMachine = left.machineId.localeCompare(right.machineId);
  return byMachine || left.addr.localeCompare(right.addr);
}

function statusLabel(agent: AgentStatus): string {
  if (agent.identityState === "conflict") return "身份冲突";
  if (agent.identityState === "pending") return "待识别";
  return agent.online ? "在线" : "离线";
}

export function AgentsPage({ api = appApi }: AgentsPageProps) {
  const [refreshVersion, setRefreshVersion] = useState(0);
  // 点击刷新时记录轮询基线；本轮轮询出结果（成功或失败）后基线失配，refreshing 自动结束。
  // 不跟随 state.loading，避免随 2 秒后台轮询抖动。
  const [refreshBaseline, setRefreshBaseline] = useState<{ revision: number; error: Error | undefined }>();
  const request = useCallback((signal: AbortSignal) => api.listAgents(signal), [api]);
  const state = usePolling(request, { dependencies: [api, refreshVersion] });
  const refreshing = refreshBaseline !== undefined
    && state.successRevision === refreshBaseline.revision
    && state.error === refreshBaseline.error;
  const agents = state.data ? [...state.data].sort(compareAgents) : [];
  const online = agents.filter(agent => agent.online && agent.identityState === "claimed").length;
  const hasConflict = agents.some(agent => agent.identityState === "conflict");
  const refresh = () => {
    setRefreshBaseline({ revision: state.successRevision, error: state.error });
    setRefreshVersion(version => version + 1);
  };

  return (
    <section aria-labelledby="agents-heading" className="operational-page">
      <header className="operational-page__header operational-surface">
        <h1 id="agents-heading">Agent</h1>
        <p>查看各采集机的连接状态与最近错误。</p>
        <button aria-label="刷新 Agent 列表" disabled={refreshing} onClick={refresh} type="button">
          {refreshing ? "正在刷新…" : "刷新"}
        </button>
      </header>
      {state.error ? <AsyncState error={state.error.message} onRetry={refresh} state="error" /> : null}
      {!state.data && state.loading ? <AsyncState state="loading" /> : null}
      {state.data && agents.length === 0 ? <AsyncState message="当前没有 Agent。" state="empty" /> : null}
      {agents.length > 0 ? (
        <section aria-label="Agent 列表" className="operational-surface">
          <p>在线 {online} / 共 {agents.length}</p>
          {hasConflict ? (
            <p className="agents-note">身份冲突：同一机器可能被重复部署或配置冲突，请检查 Agent 安装。</p>
          ) : null}
          <div aria-label="Agent 状态表" className="operational-table-scroll" role="region" tabIndex={0}>
            <table>
              <thead><tr><th scope="col">机器</th><th scope="col">地址</th><th scope="col">状态</th><th scope="col">最近错误</th></tr></thead>
              <tbody>
                {agents.map(agent => (
                  <tr data-testid="agent-row" key={agent.addr}>
                    <td>{agent.machineId || "待识别"}</td>
                    <td>{agent.addr}</td>
                    <td>{statusLabel(agent)}</td>
                    <td>{agent.lastErr || "—"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      ) : null}
    </section>
  );
}
