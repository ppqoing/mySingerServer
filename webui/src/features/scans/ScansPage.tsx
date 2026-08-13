import { useCallback, useEffect, useRef, useState } from "react";
import { appApi, type AppApi } from "../../api/appApi";
import type { AgentStatus, ScanTask } from "../../api/contracts";
import { AsyncState } from "../../components/AsyncState";
import { usePolling } from "../../hooks/usePolling";
import "../operational-pages.css";
import { RemotePathBrowser } from "./RemotePathBrowser";
import { addTaskRoot } from "./taskRoots";

export interface ScansPageProps {
  readonly api?: AppApi;
}

function isTerminal(task: ScanTask) {
  return task.status === "done" || task.status === "failed";
}

function formatDuration(milliseconds: number) {
  return `${(milliseconds / 1_000).toFixed(1)} 秒`;
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

export function ScansPage({ api = appApi }: ScansPageProps) {
  const [selectedMachine, setSelectedMachine] = useState("");
  const [roots, setRoots] = useState<string[]>([]);
  const [manualRoot, setManualRoot] = useState("");
  const [browserOpen, setBrowserOpen] = useState(false);
  const [rescan, setRescan] = useState(false);
  const [formError, setFormError] = useState<string>();
  const [createdTaskId, setCreatedTaskId] = useState<string>();
  const [submitting, setSubmitting] = useState(false);
  const [taskRefreshVersion, setTaskRefreshVersion] = useState(0);
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
    submitController.current?.abort();
    const controller = new AbortController();
    submitController.current = controller;
    setSubmitting(true);
    setFormError(undefined);
    try {
      const result = await api.startScan({ machineId: selectedMachine, roots, phase: 1, rescan }, controller.signal);
      if (!controller.signal.aborted) {
        setCreatedTaskId(result.taskId);
        setTaskRefreshVersion(version => version + 1);
      }
    } catch (error) {
      if (!controller.signal.aborted) {
        setFormError(error instanceof Error ? error.message : "创建扫描任务失败。");
      }
    } finally {
      if (submitController.current === controller) setSubmitting(false);
    }
  };

  const tasks = tasksState.data ?? [];
  const canBrowse = Boolean(selectedAgent?.online && selectedAgent.identityState === "claimed");

  const addRoot = (candidate: string) => {
    const change = addTaskRoot(roots, candidate);
    if (change.kind === "add") {
      setRoots(change.roots);
      setManualRoot("");
      setFormError(undefined);
      return true;
    }
    if (change.kind === "replace") {
      if (!window.confirm(`添加 ${candidate} 会覆盖已选子目录：${change.covered.join("、")}。是否继续？`)) return false;
      setRoots(change.roots);
      setManualRoot("");
      setFormError(undefined);
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
      setFormError("切换 Agent 后已清空待选根目录。");
    }
    setBrowserOpen(false);
    setSelectedMachine(machineID);
  };

  return (
    <section aria-labelledby="scans-heading" className="operational-page">
      <header className="operational-page__header operational-surface">
        <h1 id="scans-heading">扫描任务</h1>
        <p>仅可向在线 Agent 提交一阶段扫描；可从远程目录选择或逐个手工输入根目录。</p>
      </header>
      <section aria-label="创建扫描任务" className="operational-form operational-surface">
        <label htmlFor="scan-agent">扫描 Agent</label>
        <select id="scan-agent" onChange={event => changeMachine(event.target.value)} value={selectedMachine}>
          <option value="">请选择在线 Agent</option>
          {agents.map(agent => (
            <option disabled={!agent.online || agent.identityState !== "claimed"} key={agent.machineId} value={agent.machineId}>
              {agent.machineId}（{agent.online && agent.identityState === "claimed" ? "在线" : "离线"}）
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
        <button disabled={submitting} onClick={() => void submit()} type="button">
          {submitting ? "正在创建…" : "创建扫描任务"}
        </button>
        {formError ? <p role="alert">{formError}</p> : null}
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
        {tasksState.error ? <AsyncState error={tasksState.error.message} onRetry={() => setTaskRefreshVersion(version => version + 1)} state="error" /> : null}
        {!tasksState.data && tasksState.loading ? <AsyncState message="正在加载任务…" state="loading" /> : null}
        {tasksState.data && tasks.length === 0 ? <AsyncState message="当前没有扫描任务。" state="empty" /> : null}
        {tasks.length > 0 ? (
          <div aria-label="扫描任务数据表" className="operational-table-scroll" role="region" tabIndex={0}>
            <table>
              <thead><tr><th scope="col">任务 ID</th><th scope="col">Agent</th><th scope="col">状态</th><th scope="col">进度</th><th scope="col">跳过</th><th scope="col">失败</th><th scope="col">耗时</th><th scope="col">速度</th><th scope="col">最近错误</th></tr></thead>
              <tbody>{tasks.map(task => <tr key={task.taskId}>
                <td>{task.taskId}</td><td>{task.machineId}</td><td>{task.status}</td><td>{task.done}/{task.total}</td>
                <td>{task.skipped}</td><td>{task.failed}</td><td>{formatDuration(task.elapsedMs)}</td><td>{task.speed}</td><td>{task.lastErr || "—"}</td>
              </tr>)}</tbody>
            </table>
          </div>
        ) : null}
      </section>
    </section>
  );
}
