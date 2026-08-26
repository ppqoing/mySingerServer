import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { appApi, type AppApi } from "../../api/appApi";
import { ApiError } from "../../api/client";
import { apiErrorText } from "../../api/errorText";
import type { AgentStatus, FeatureItem, ScanTask } from "../../api/contracts";
import { AsyncState } from "../../components/AsyncState";
import { usePolling } from "../../hooks/usePolling";
import "../operational-pages.css";
import { RemotePathBrowser } from "./RemotePathBrowser";
import { addTaskRoot, normalizeTaskRoot } from "./taskRoots";

export interface ScansPageProps {
  readonly api?: AppApi;
}

function isTerminal(task: ScanTask) {
  return task.status === "done" || task.status === "failed";
}

function formatDuration(milliseconds: number) {
  return `${(milliseconds / 1_000).toFixed(1)} 秒`;
}

const taskStatusText: Record<string, string> = {
  sent: "已下发",
  acked: "已受理",
  running: "运行中",
  cancelling: "正在停止",
  done: "已完成",
  failed: "失败"
};

function statusText(task: ScanTask) {
  // 取消完成的任务终态为 failed + ackReason=cancelled，对用户展示为"已取消"。
  if (task.status === "failed" && task.ackReason === "cancelled") return "已取消";
  return taskStatusText[task.status] ?? task.status;
}

function phaseText(phase: number) {
  if (phase === 1) return "一筛";
  if (phase === 2) return "二筛";
  return `phase ${phase}`;
}

/** done 任务区分"真扫完"与"已被去重跳过"；非 done 任务无完成方式可言。 */
function outcomeText(task: ScanTask) {
  if (task.status !== "done") return "—";
  return task.ackReason === "already_done" ? "已跳过：already_done" : "已完成";
}

function rootsText(roots: readonly string[]) {
  if (roots.length === 0) return "—";
  return roots.length === 1 ? roots[0] : `${roots[0]} 等 ${roots.length} 个`;
}

function updatedAtText(updatedAt: string) {
  const parsed = new Date(updatedAt);
  return Number.isNaN(parsed.getTime()) ? updatedAt : parsed.toLocaleString();
}

const stalledThresholdMs = 5 * 60_000;

/** 非终态任务超过 5 分钟无更新视为停滞（Agent 掉线等），行高亮提示。 */
function stalledTask(task: ScanTask, now = Date.now()) {
  if (isTerminal(task)) return false;
  const updated = Date.parse(task.updatedAt);
  return !Number.isNaN(updated) && now - updated > stalledThresholdMs;
}

/** recent 里需要展示的异常条目：显式报错或状态非 done（partial/failed/crash）。 */
function recentErrorItems(task: ScanTask): FeatureItem[] {
  return task.recent.filter(item => item.err !== undefined || item.status !== "done");
}

/** 规范化后比较根目录集合（大小写/分隔符/尾部分隔符不敏感），用于重复提交拦截。 */
function sameRootSet(left: readonly string[], right: readonly string[]) {
  const normalize = (values: readonly string[]) =>
    values.map(value => (normalizeTaskRoot(value) ?? value.trim()).toLowerCase()).sort();
  const a = normalize(left);
  const b = normalize(right);
  return a.length === b.length && a.every((value, index) => value === b[index]);
}

function dispatchableAgents(values: readonly AgentStatus[]): AgentStatus[] {
  const byMachine = new Map<string, AgentStatus>();
  for (const agent of values) {
    if (!agent.machineId) continue;
    const current = byMachine.get(agent.machineId);
    const preferred = agent.online && agent.identityState === "claimed";
    const currentPreferred = Boolean(current?.online && current.identityState === "claimed");
    if (!current || (preferred && !currentPreferred)) byMachine.set(agent.machineId, agent);
  }
  return [...byMachine.values()];
}

type TaskFilter = "all" | "active" | "finished";

export function ScansPage({ api = appApi }: ScansPageProps) {
  const [selectedMachine, setSelectedMachine] = useState("");
  const [roots, setRoots] = useState<string[]>([]);
  const [manualRoot, setManualRoot] = useState("");
  const [browserOpen, setBrowserOpen] = useState(false);
  const [phase, setPhase] = useState<1 | 2>(1);
  const [rescan, setRescan] = useState(false);
  const [formError, setFormError] = useState<string>();
  const [formNotice, setFormNotice] = useState<string>();
  const [createdTaskId, setCreatedTaskId] = useState<string>();
  const [submitting, setSubmitting] = useState(false);
  const [taskRefreshVersion, setTaskRefreshVersion] = useState(0);
  const [statusFilter, setStatusFilter] = useState<TaskFilter>("all");
  // 本地"正在停止"占位（按 taskId）。条目只在取消失败时移除；服务端 cancelling/终态接管展示后
  // 残留条目对渲染无影响（终态行不渲染按钮，服务端 cancelling 自身即显示"正在停止"），故不做清理。
  const [stoppingIds, setStoppingIds] = useState<ReadonlySet<string>>(new Set());
  const [taskActionError, setTaskActionError] = useState<string>();
  const [phase2AutoDispatch, setPhase2AutoDispatch] = useState<boolean>();
  const submitController = useRef<AbortController | undefined>(undefined);

  const agentsRequest = useCallback((signal: AbortSignal) => api.listAgents(signal), [api]);
  const tasksRequest = useCallback((signal: AbortSignal) => api.listTasks(signal), [api]);
  const agentsState = usePolling(agentsRequest, { dependencies: [api] });
  const tasksState = usePolling(tasksRequest, {
    dependencies: [api, taskRefreshVersion],
    isTerminal: tasks => tasks.length > 0 && tasks.every(isTerminal)
  });
  const agents = dispatchableAgents(agentsState.data ?? []);
  const selectedAgent = agents.find(agent => agent.machineId === selectedMachine);

  useEffect(() => () => submitController.current?.abort(), []);

  // 只读展示 phase2.autoDispatch 配置状态；配置不可读时省略该提示。
  useEffect(() => {
    const controller = new AbortController();
    api.loadGUIConfig(controller.signal).then(
      snapshot => {
        if (!controller.signal.aborted) setPhase2AutoDispatch(snapshot.config.phase2.autoDispatch);
      },
      () => undefined
    );
    return () => controller.abort();
  }, [api]);

  const tasks = tasksState.data ?? [];

  const submit = async () => {
    if (!selectedMachine) {
      setFormError("请选择在线 Agent。");
      return;
    }
    if (roots.length === 0) {
      setFormError("至少输入一个扫描根目录。");
      return;
    }
    if (!selectedAgent?.online) {
      setFormError("所选 Agent 当前离线，请重新选择。");
      return;
    }
    if (tasks.some(task => !isTerminal(task) && task.machineId === selectedMachine && sameRootSet(task.roots, roots))) {
      setFormError("该 Agent 已有相同根目录的进行中任务，请等待其完成或调整根目录。");
      return;
    }
    submitController.current?.abort();
    const controller = new AbortController();
    submitController.current = controller;
    setSubmitting(true);
    setFormError(undefined);
    setCreatedTaskId(undefined);
    try {
      const result = await api.startScan({ machineId: selectedMachine, roots, phase, rescan }, controller.signal);
      if (!controller.signal.aborted) {
        setCreatedTaskId(result.taskId);
        setFormError(undefined);
        setTaskRefreshVersion(version => version + 1);
      }
    } catch (error) {
      if (!controller.signal.aborted) {
        setFormError(apiErrorText(error, "创建扫描任务失败。"));
      }
    } finally {
      if (submitController.current === controller) setSubmitting(false);
    }
  };

  const canBrowse = Boolean(selectedAgent?.online && selectedAgent.identityState === "claimed");

  const stopTask = async (task: ScanTask) => {
    if (isTerminal(task) || stoppingIds.has(task.taskId)) return;
    setTaskActionError(undefined);
    setStoppingIds(ids => new Set(ids).add(task.taskId));
    try {
      await api.cancelTask(task.taskId);
      // 乐观中间态由 stoppingIds 维持，刷新后由服务端 cancelling/终态接管。
      setTaskRefreshVersion(version => version + 1);
    } catch (error) {
      setStoppingIds(ids => {
        const next = new Set(ids);
        next.delete(task.taskId);
        return next;
      });
      if (error instanceof ApiError && error.status === 404) {
        setTaskActionError("任务不存在或已被清除。");
      } else if (error instanceof ApiError && error.status === 409) {
        setTaskActionError("任务已结束，无法停止。");
      } else {
        setTaskActionError(apiErrorText(error, "停止任务失败。"));
      }
      setTaskRefreshVersion(version => version + 1);
    }
  };

  const addRoot = (candidate: string) => {
    const change = addTaskRoot(roots, candidate);
    if (change.kind === "add") {
      setRoots(change.roots);
      setManualRoot("");
      setFormError(undefined);
      setFormNotice(undefined);
      return true;
    }
    if (change.kind === "replace") {
      if (!window.confirm(`添加 ${candidate} 会覆盖已选子目录：${change.covered.join("、")}。是否继续？`)) return false;
      setRoots(change.roots);
      setManualRoot("");
      setFormError(undefined);
      setFormNotice(undefined);
      return true;
    }
    setFormError(change.kind === "invalid" ? "请输入绝对 Windows 或 UNC 路径。" : change.kind === "duplicate"
      ? "该扫描根目录已添加。"
      : "该目录已被现有扫描根目录覆盖。");
    return false;
  };

  const changeMachine = (machineID: string) => {
    if (selectedMachine && machineID !== selectedMachine && (roots.length > 0 || manualRoot)) {
      setRoots([]);
      setManualRoot("");
      setFormNotice("切换 Agent 后已清空待选根目录。");
    }
    setBrowserOpen(false);
    setSelectedMachine(machineID);
  };

  const activeTasks = tasks.filter(task => !isTerminal(task));
  const finishedTasks = tasks.filter(isTerminal);

  const taskTable = (subset: ScanTask[], label: string) => (
    <div aria-label={label} className="operational-table-scroll" role="region" tabIndex={0}>
      <table>
        <thead><tr>
          <th scope="col">任务 ID</th><th scope="col">Agent</th><th scope="col">状态</th><th scope="col">阶段</th><th scope="col">根目录</th>
          <th scope="col">进度</th><th scope="col">跳过</th><th scope="col">失败</th><th scope="col">扫描错误</th>
          <th scope="col">完成方式</th><th scope="col">耗时</th><th scope="col">速度</th><th scope="col">更新时间</th>
          <th scope="col">最近错误</th><th scope="col">操作</th>
        </tr></thead>
        <tbody>{subset.map(task => {
          const errorItems = recentErrorItems(task);
          const stopping = task.status === "cancelling" || stoppingIds.has(task.taskId);
          return (
            <tr className={stalledTask(task) ? "scan-task-row--stalled" : undefined} key={task.taskId}>
              <td>{task.taskId}</td>
              <td>{task.machineId}</td>
              <td>
                {statusText(task)}
                {task.status === "done" ? (
                  <Link className="scan-task-next" to="/analysis">扫描完成，下一步：运行一筛分析 →</Link>
                ) : null}
              </td>
              <td>{phaseText(task.phase)}</td>
              <td title={task.roots.join("\n")}>{rootsText(task.roots)}</td>
              <td>{task.done}/{task.total}</td>
              <td>{task.skipped}</td>
              <td>{task.failed}</td>
              <td>{task.scanErrors > 0 ? <span className="scan-task-errors">{task.scanErrors}</span> : task.scanErrors}</td>
              <td>{outcomeText(task)}</td>
              <td>{isTerminal(task) ? formatDuration(task.elapsedMs) : "—"}</td>
              <td>{`${task.speed.toFixed(1)} 文件/秒`}</td>
              <td title={task.updatedAt}>{updatedAtText(task.updatedAt)}</td>
              <td>
                {task.lastErr || "—"}
                {errorItems.length > 0 ? (
                  <details className="scan-task-recent">
                    <summary>错误明细（{errorItems.length}）</summary>
                    <ul>{errorItems.map(item => (
                      <li key={item.path}><span title={item.path}>{item.path}</span>：{item.err ?? item.status}</li>
                    ))}</ul>
                  </details>
                ) : null}
              </td>
              <td>
                {isTerminal(task) ? "—" : (
                  <button disabled={stopping} onClick={() => void stopTask(task)} type="button">
                    {stopping ? "正在停止…" : "停止"}
                  </button>
                )}
              </td>
            </tr>
          );
        })}</tbody>
      </table>
    </div>
  );

  return (
    <section aria-labelledby="scans-heading" className="operational-page">
      <header className="operational-page__header operational-surface">
        <h1 id="scans-heading">扫描任务</h1>
        <p>仅可向在线 Agent 提交扫描任务（可选一筛/二筛阶段）；可从远程目录选择或逐个手工输入根目录。</p>
      </header>
      <section aria-label="创建扫描任务" className="operational-form operational-surface">
        <label htmlFor="scan-agent">扫描 Agent</label>
        <select id="scan-agent" onChange={event => changeMachine(event.target.value)} value={selectedMachine}>
          <option value="">请选择在线 Agent</option>
          {agents.map(agent => (
            <option disabled={!agent.online || agent.identityState !== "claimed"} key={agent.machineId} value={agent.machineId}>
              {agent.machineId}（{agent.addr}，{agent.online && agent.identityState === "claimed" ? "在线" : "离线"}）
            </option>
          ))}
        </select>
        <label htmlFor="scan-manual-root">手工根目录</label>
        <div className="scan-roots-input">
          <input id="scan-manual-root" onChange={event => setManualRoot(event.target.value)} placeholder="D:\\Music 或 \\server\\share" value={manualRoot} />
          <button onClick={() => addRoot(manualRoot)} type="button">添加根目录</button>
          <button disabled={!canBrowse} onClick={() => setBrowserOpen(true)} type="button">选择目录…</button>
        </div>
        {roots.length > 0 ? <ul aria-label="已选扫描根目录" className="scan-roots-list">{roots.map(root => <li key={root}>
          <span>{root}</span><button aria-label={`移除 ${root}`} onClick={() => setRoots(values => values.filter(value => value !== root))} type="button">移除</button>
        </li>)}</ul> : null}
        <label><input checked={rescan} onChange={event => setRescan(event.target.checked)} type="checkbox" />重新扫描</label>
        <fieldset className="scan-phase-select">
          <legend>扫描阶段</legend>
          <label><input checked={phase === 1} name="scan-phase" onChange={() => setPhase(1)} type="radio" />一筛（phase 1）</label>
          <label><input checked={phase === 2} name="scan-phase" onChange={() => setPhase(2)} type="radio" />二筛（phase 2）</label>
        </fieldset>
        {phase2AutoDispatch !== undefined ? (
          <p className="scan-phase2-autodispatch">phase2 自动派发：{phase2AutoDispatch ? "开" : "关"}</p>
        ) : null}
        <button disabled={submitting} onClick={() => void submit()} type="button">
          {submitting ? "正在创建…" : "创建扫描任务"}
        </button>
        {formError ? <p role="alert">{formError}</p> : null}
        {formNotice ? <p role="status">{formNotice}</p> : null}
        {createdTaskId ? <p>已创建任务：{createdTaskId}</p> : null}
      </section>
      <RemotePathBrowser
        api={api}
        machineID={selectedMachine}
        onAdd={path => {
          if (addRoot(path)) setBrowserOpen(false);
        }}
        onClose={() => setBrowserOpen(false)}
        open={browserOpen && canBrowse}
      />
      {agentsState.error ? <AsyncState error={agentsState.error.message} state="error" /> : null}
      {!agentsState.data && agentsState.loading ? <AsyncState message="正在加载 Agent…" state="loading" /> : null}
      <section aria-label="扫描任务列表" className="operational-surface">
        <h2>当前任务</h2>
        <div className="scan-tasks-toolbar">
          <label htmlFor="scan-task-filter">任务筛选</label>
          <select id="scan-task-filter" onChange={event => setStatusFilter(event.target.value as TaskFilter)} value={statusFilter}>
            <option value="all">全部</option>
            <option value="active">进行中</option>
            <option value="finished">已结束</option>
          </select>
          <button onClick={() => setTaskRefreshVersion(version => version + 1)} type="button">刷新任务列表</button>
        </div>
        {taskActionError ? <p role="alert">{taskActionError}</p> : null}
        {tasksState.error ? <AsyncState error={tasksState.error.message} onRetry={() => setTaskRefreshVersion(version => version + 1)} state="error" /> : null}
        {!tasksState.data && tasksState.loading ? <AsyncState message="正在加载任务…" state="loading" /> : null}
        {tasksState.data && tasks.length === 0 ? <AsyncState message="当前没有扫描任务。" state="empty" /> : null}
        {statusFilter !== "finished" && tasks.length > 0 ? (
          activeTasks.length > 0
            ? taskTable(activeTasks, "扫描任务数据表")
            : <AsyncState message="当前没有进行中的任务。" state="empty" />
        ) : null}
        {statusFilter === "all" && finishedTasks.length > 0 ? (
          <details className="scan-tasks-history">
            <summary>已结束任务（{finishedTasks.length}）</summary>
            {taskTable(finishedTasks, "已结束任务数据表")}
          </details>
        ) : null}
        {statusFilter === "finished" && tasks.length > 0 ? (
          finishedTasks.length > 0
            ? taskTable(finishedTasks, "扫描任务数据表")
            : <AsyncState message="当前没有已结束的任务。" state="empty" />
        ) : null}
      </section>
    </section>
  );
}
