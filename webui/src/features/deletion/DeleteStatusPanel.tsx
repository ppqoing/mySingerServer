import { useCallback, useState } from "react";
import { appApi, type AppApi } from "../../api/appApi";
import type { DeleteTaskStatus } from "../../api/contracts";
import { AsyncState } from "../../components/AsyncState";
import { usePolling } from "../../hooks/usePolling";
import "../operational-pages.css";

export interface DeleteStatusPanelProps {
  readonly api?: AppApi;
  readonly taskId?: string;
}

const errorCodeLabels: Readonly<Record<string, string>> = {
  E_NOT_FOUND: "文件不存在",
  E_BAD_PATH: "路径无效",
  E_PATH_DENIED: "路径被拒绝",
  E_NOT_CONFIRMED: "未确认",
  E_READONLY: "只读文件",
  E_ACCESS_DENIED: "访问被拒绝",
  E_DELETE_FAILED: "删除失败",
  E_RECYCLE_FAILED: "移入回收站失败",
  E_IN_USE: "文件正在使用",
  E_REPARSE: "重解析点被拒绝",
  E_BAD_MODE: "删除模式无效",
  E_HELPER_LOST: "Helper 连接丢失"
};

function errorCodeText(code: string | undefined) {
  if (!code) return "未提供";
  const label = errorCodeLabels[code];
  return label ? `${code}（${label}）` : code;
}

function initialTaskFromHash() {
  const query = window.location.hash.includes("?") ? window.location.hash.split("?")[1] : "";
  return new URLSearchParams(query).get("task") ?? "";
}

function StatusDetails({ status }: { status: DeleteTaskStatus }) {
  const machines = Object.values(status.byMachine).sort((left, right) => left.machineId.localeCompare(right.machineId));
  const codes = Object.entries(status.errorCodes).sort(([left], [right]) => left.localeCompare(right));
  return <>
    <dl aria-label="删除任务总览" className="operational-metrics operational-surface">
      <div><dt>模式</dt><dd>{status.mode}</dd></div><div><dt>总数</dt><dd>{status.total}</dd></div>
      <div><dt>成功</dt><dd>{status.ok}</dd></div><div><dt>失败</dt><dd>{status.failed}</dd></div>
      <div><dt>不确定</dt><dd>{status.uncertain}</dd></div><div><dt>待处理</dt><dd>{status.pending}</dd></div>
      <div><dt>状态同步失败</dt><dd>{status.stateSyncFailures}</dd></div><div><dt>状态</dt><dd>{status.complete ? "已完成" : "进行中"}</dd></div>
    </dl>
    <section aria-label="按 Agent 进度" className="operational-surface"><h2>按 Agent 进度</h2>
      <div aria-label="删除 Agent 进度表" className="operational-table-scroll" role="region" tabIndex={0}>
        <table><thead><tr><th scope="col">Agent</th><th scope="col">成功/总数</th><th scope="col">失败</th><th scope="col">不确定</th><th scope="col">待处理</th><th scope="col">状态同步失败</th><th scope="col">状态</th></tr></thead><tbody>
          {machines.map(machine => <tr data-testid="delete-machine-row" key={machine.machineId}><td>{machine.machineId}</td><td>{machine.ok}/{machine.total}</td><td>{machine.failed}</td><td>{machine.uncertain}</td><td>{machine.pending}</td><td>{machine.stateSyncFailures}</td><td>{machine.complete ? "已完成" : "进行中"}</td></tr>)}
        </tbody></table>
      </div>
    </section>
    <section aria-label="错误代码" className="operational-surface"><h2>错误代码</h2>{codes.length === 0 ? <p>无</p> : <ul>{codes.map(([code, count], index) => <li key={`${code}-${index}`}>{errorCodeText(code)}：{count}</li>)}</ul>}</section>
    <section aria-label="问题项目" className="operational-surface"><h2>问题项目</h2>{status.problems.length === 0 ? <p>无</p> : <ul>{status.problems.map(problem => <li key={`${problem.machineId}-${problem.sequence}-${problem.path}`}>
      Agent：{problem.machineId}；路径：{problem.path}；代码：{errorCodeText(problem.errorCode)}；消息：{problem.errorMessage || problem.stateSyncErr || "—"}
    </li>)}</ul>}</section>
  </>;
}

export function DeleteStatusPanel({ api = appApi, taskId }: DeleteStatusPanelProps) {
  const initialManualTaskId = initialTaskFromHash();
  const [inputTaskId, setInputTaskId] = useState(() => taskId ?? initialManualTaskId);
  const [manualActiveTaskId, setManualActiveTaskId] = useState(() => taskId === undefined ? initialManualTaskId : "");
  const [refreshVersion, setRefreshVersion] = useState(0);
  const [lookupError, setLookupError] = useState<string>();
  const activeTaskId = taskId ?? manualActiveTaskId;
  const hasActiveTaskId = activeTaskId.trim() !== "";
  const controlledTaskMissing = taskId !== undefined && !hasActiveTaskId;
  const request = useCallback((signal: AbortSignal) => api.getDeleteStatus(activeTaskId, signal), [activeTaskId, api]);
  const state = usePolling(request, {
    enabled: hasActiveTaskId,
    dependencies: [api, activeTaskId, refreshVersion],
    isTerminal: status => status.complete
  });
  const refresh = () => setRefreshVersion(version => version + 1);
  const lookup = () => {
    const next = inputTaskId.trim();
    if (next === "") {
      setLookupError("请输入删除任务 ID");
      return;
    }
    setLookupError(undefined);
    setManualActiveTaskId(next);
    setRefreshVersion(version => version + 1);
  };
  const status = state.data?.taskId === activeTaskId ? state.data : undefined;

  return (
    <section aria-labelledby="delete-status-heading" className="operational-page">
      <header className="operational-page__header operational-surface"><h1 id="delete-status-heading">删除审计</h1><p>只读查看已提交的删除任务，不会执行删除操作。</p></header>
      {taskId === undefined ? <section aria-label="查询删除任务" className="operational-form operational-surface"><label htmlFor="delete-task-id">删除任务 ID</label><input id="delete-task-id" onChange={event => setInputTaskId(event.target.value)} value={inputTaskId} /><button onClick={lookup} type="button">查询</button>{lookupError ? <p role="alert">{lookupError}</p> : null}</section> : null}
      {controlledTaskMissing ? <p className="operational-surface" role="alert">缺少删除任务 ID</p> : null}
      {hasActiveTaskId ? <p className="operational-surface">任务：{activeTaskId} <button disabled={state.loading} onClick={refresh} type="button">刷新</button></p> : null}
      {hasActiveTaskId && state.error ? <AsyncState error={state.error.message} onRetry={refresh} state="error" /> : null}
      {hasActiveTaskId && !status && state.loading ? <AsyncState state="loading" /> : null}
      {hasActiveTaskId && status ? <StatusDetails status={status} /> : null}
    </section>
  );
}
