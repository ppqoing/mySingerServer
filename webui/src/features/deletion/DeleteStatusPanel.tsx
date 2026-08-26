import { useCallback, useState } from "react";
import { appApi, type AppApi } from "../../api/appApi";
import type { DeleteTaskStatus } from "../../api/contracts";
import { AsyncState } from "../../components/AsyncState";
import { CopyButton } from "../../components/CopyButton";
import { usePolling } from "../../hooks/usePolling";
import { deriveDeleteRetryPlan, type DeleteReviewSnapshot } from "../groups/deleteReview";
import "../operational-pages.css";
import "./deletion.css";
import { errorCodeText } from "./errorCodes";
import { RecycledToList } from "./RecycledToList";

export interface DeleteStatusPanelProps {
  readonly api?: AppApi;
  readonly taskId?: string;
  /** 重复组删除流程留下的原始选择快照；存在且与当前任务匹配时支持一键重试。 */
  readonly deleteReviewSnapshot?: DeleteReviewSnapshot;
  /** 一键重试（P1-6 短期方案）：把可重试 fileIds 交回 App 级状态并导航到重复组页。 */
  readonly onRetryRequest?: (fileIds: number[]) => void;
}

function initialTaskFromHash() {
  const query = window.location.hash.includes("?") ? window.location.hash.split("?")[1] : "";
  return new URLSearchParams(query).get("task") ?? "";
}

function shortTaskId(taskId: string): string {
  return taskId.length > 11 ? `${taskId.slice(0, 8)}…` : taskId;
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
      {machines.map(machine => machine.recycledTo
        ? <RecycledToList key={machine.machineId} machineId={machine.machineId} recycledTo={machine.recycledTo} />
        : null)}
    </section>
    <section aria-label="错误代码" className="operational-surface"><h2>错误代码</h2>{codes.length === 0 ? <p>无</p> : <ul>{codes.map(([code, count], index) => <li key={`${code}-${index}`}>{errorCodeText(code)}：{count}</li>)}</ul>}</section>
    <section aria-label="问题项目" className="operational-surface"><h2>问题项目</h2>{status.problems.length === 0 ? <p>无</p> : <ul>{status.problems.map(problem => {
      const detail = problem.errorMessage || problem.stateSyncErr || "";
      return <li key={`${problem.machineId}-${problem.sequence}-${problem.path}`}>
        Agent：{problem.machineId}；路径：{problem.path} <CopyButton label="复制路径" text={problem.path} />；代码：{errorCodeText(problem.errorCode)}；消息：{detail || "—"}{detail ? <> <CopyButton label="复制错误" text={detail} /></> : null}
      </li>;
    })}</ul>}</section>
  </>;
}

interface DeleteTaskListProps {
  readonly api: AppApi;
  readonly onSelect: (taskId: string) => void;
}

/** 删除任务列表：进行中在前（后端排序），有进行中任务时跟随 usePolling 的 2s/10s 节奏刷新，全终态后停止。 */
function DeleteTaskList({ api, onSelect }: DeleteTaskListProps) {
  const [retryVersion, setRetryVersion] = useState(0);
  const request = useCallback((signal: AbortSignal) => api.listDeleteTasks(signal), [api]);
  const state = usePolling(request, {
    dependencies: [api, retryVersion],
    isTerminal: tasks => tasks.every(task => task.complete)
  });
  const tasks = state.data;
  const refresh = () => setRetryVersion(version => version + 1);
  return (
    <section aria-label="删除任务列表" className="operational-surface">
      <h2>删除任务</h2>
      <p><button disabled={state.loading} onClick={refresh} type="button">刷新</button></p>
      {state.error ? <AsyncState error={state.error.message} onRetry={refresh} state="error" /> : null}
      {!tasks && state.loading ? <AsyncState state="loading" /> : null}
      {tasks && tasks.length === 0 ? <p>暂无删除任务。</p> : null}
      {tasks && tasks.length > 0 ? (
        <div aria-label="删除任务表" className="operational-table-scroll" role="region" tabIndex={0}>
          <table><thead><tr><th scope="col">任务 ID</th><th scope="col">模式</th><th scope="col">成功/总数</th><th scope="col">失败</th><th scope="col">不确定</th><th scope="col">待处理</th><th scope="col">状态</th><th scope="col">创建时间</th><th scope="col">操作</th></tr></thead><tbody>
            {tasks.map(task => <tr data-testid="delete-task-row" key={task.taskId}>
              <td><span title={task.taskId}>{shortTaskId(task.taskId)}</span> <CopyButton label="复制任务 ID" text={task.taskId} /></td>
              <td>{task.mode}</td><td>{task.ok}/{task.total}</td><td>{task.failed}</td><td>{task.uncertain}</td><td>{task.pending}</td>
              <td>{task.complete ? "已完成" : "进行中"}</td><td>{task.createdAt}</td>
              <td><button onClick={() => onSelect(task.taskId)} type="button">查看</button></td>
            </tr>)}
          </tbody></table>
        </div>
      ) : null}
    </section>
  );
}

export function DeleteStatusPanel({ api = appApi, deleteReviewSnapshot, onRetryRequest, taskId }: DeleteStatusPanelProps) {
  const initialManualTaskId = initialTaskFromHash();
  const [inputTaskId, setInputTaskId] = useState(() => taskId ?? initialManualTaskId);
  const [manualActiveTaskId, setManualActiveTaskId] = useState(() => taskId === undefined ? initialManualTaskId : "");
  const [lastControlledTaskId, setLastControlledTaskId] = useState(taskId);
  const [refreshVersion, setRefreshVersion] = useState(0);
  const [lookupError, setLookupError] = useState<string>();
  if (taskId !== lastControlledTaskId) {
    // 受控 taskId 变化（含变为空）时回到受控任务并重填表单（渲染期派生状态）
    setLastControlledTaskId(taskId);
    setInputTaskId(taskId ?? "");
    setManualActiveTaskId("");
    setLookupError(undefined);
  }
  // 手动查询优先，受控任务兜底；受控模式下轮询目标跟随表单查询值
  const activeTaskId = manualActiveTaskId !== "" ? manualActiveTaskId : (taskId ?? "");
  const hasActiveTaskId = activeTaskId.trim() !== "";
  const controlledTaskMissing = taskId !== undefined && !hasActiveTaskId;
  const showTaskList = taskId === undefined && !hasActiveTaskId;
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
  const selectTask = (nextTaskId: string) => {
    setLookupError(undefined);
    setInputTaskId(nextTaskId);
    setManualActiveTaskId(nextTaskId);
    setRefreshVersion(version => version + 1);
  };
  const status = state.data?.taskId === activeTaskId ? state.data : undefined;
  // 一键重试（P1-6 短期）：存在不确定或 E_HELPER_LOST 项时展示入口；可重试集合由
  // deriveDeleteRetryPlan 按快照推导，快照缺失/不属于当前任务/无法映射时禁用并说明。
  const hasRetryableItems = status !== undefined && (
    status.uncertain > 0 ||
    (status.errorCodes.E_HELPER_LOST ?? 0) > 0 ||
    status.problems.some(problem => problem.uncertain || problem.errorCode === "E_HELPER_LOST")
  );
  const retryPlan = status !== undefined &&
    deleteReviewSnapshot?.terminalStatus?.taskId === status.taskId
    ? deriveDeleteRetryPlan(status, deleteReviewSnapshot)
    : undefined;
  const retryAvailable = onRetryRequest !== undefined &&
    retryPlan !== undefined && retryPlan.retryMembers.length > 0;

  return (
    <section aria-labelledby="delete-status-heading" className="operational-page">
      <header className="operational-page__header operational-surface"><h1 id="delete-status-heading">删除审计</h1><p>只读查看已提交的删除任务，不会执行删除操作。</p></header>
      <section aria-label="查询删除任务" className="operational-form operational-surface"><label htmlFor="delete-task-id">删除任务 ID</label><input id="delete-task-id" onChange={event => setInputTaskId(event.target.value)} value={inputTaskId} /><button onClick={lookup} type="button">查询</button>{lookupError ? <p role="alert">{lookupError}</p> : null}</section>
      {controlledTaskMissing ? <p className="operational-surface" role="alert">缺少删除任务 ID</p> : null}
      {showTaskList ? <DeleteTaskList api={api} onSelect={selectTask} /> : null}
      {hasActiveTaskId ? <p className="operational-surface"><span>任务：{activeTaskId}</span> <CopyButton text={activeTaskId} /> <button disabled={state.loading} onClick={refresh} type="button">刷新</button></p> : null}
      {hasActiveTaskId && state.error ? <AsyncState error={state.error.message} onRetry={refresh} state="error" /> : null}
      {hasActiveTaskId && !status && state.loading ? <AsyncState state="loading" /> : null}
      {hasActiveTaskId && status ? <StatusDetails status={status} /> : null}
      {status && hasRetryableItems ? (
        <section aria-label="一键重试" className="operational-surface">
          <h2>一键重试</h2>
          <p>存在不确定或 Helper 连接丢失的删除项，可回到重复组页按原始选择重新发起删除。</p>
          <button
            disabled={!retryAvailable}
            onClick={() => {
              if (retryPlan && onRetryRequest) {
                onRetryRequest(retryPlan.retryMembers.map(member => member.fileId));
              }
            }}
            type="button"
          >
            重试这些项
          </button>
          {!retryAvailable ? <p role="status">缺少可核对的原始选择，无法一键重试。</p> : null}
        </section>
      ) : null}
    </section>
  );
}
