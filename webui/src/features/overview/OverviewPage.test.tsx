import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { AgentStatus, AppApi } from "../../api/contracts";
import { OverviewPage } from "./OverviewPage";

function apiFor(overrides: Partial<AppApi> = {}): AppApi {
  return {
    getRuntimeStatus: vi.fn().mockResolvedValue({
      databaseState: "connected",
      databaseErrorCode: "",
      agents: [],
      restarting: false,
      recoveryURL: ""
    }),
    listAgents: vi.fn().mockResolvedValue([]),
    listTasks: vi.fn().mockResolvedValue([]),
    getAnalysisStatus: vi.fn().mockResolvedValue({ running: false, last: null, lastErr: "" }),
    getGroupsStats: vi.fn().mockResolvedValue({ kind: "", groups: 0, totalBytes: 0, wastedBytes: 0 }),
    ...overrides
  } as unknown as AppApi;
}

test("shows database degradation and does not start business polling", async () => {
  const api = apiFor({
    getRuntimeStatus: vi.fn().mockResolvedValue({
      databaseState: "error",
      databaseErrorCode: "postgres_unreachable",
      agents: [],
      restarting: false,
      recoveryURL: ""
    })
  });

  render(<MemoryRouter><OverviewPage api={api} /></MemoryRouter>);

  expect(await screen.findByText("PostgreSQL 未连接")).toBeInTheDocument();
  expect(screen.getByText("无法连接数据库：请检查网络与数据库服务状态。")).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "打开 GUI 设置" })).toHaveAttribute("href", "/settings");
  await waitFor(() => expect(api.getRuntimeStatus).toHaveBeenCalledTimes(1));
  expect(api.listAgents).not.toHaveBeenCalled();
  expect(api.listTasks).not.toHaveBeenCalled();
  expect(api.getAnalysisStatus).not.toHaveBeenCalled();
  expect(api.getGroupsStats).not.toHaveBeenCalled();
});

test("does not claim recovery for an unconfigured database", async () => {
  const api = apiFor({
    getRuntimeStatus: vi.fn().mockResolvedValue({
      databaseState: "error",
      databaseErrorCode: "postgres_not_configured",
      agents: [],
      restarting: false,
      recoveryURL: ""
    })
  });

  render(<MemoryRouter><OverviewPage api={api} /></MemoryRouter>);

  expect(await screen.findByText(/未配置数据库/)).toBeInTheDocument();
  expect(screen.queryByText(/会继续尝试恢复连接/)).not.toBeInTheDocument();
});

test("starts business polling only after a connected runtime status", async () => {
  const api = apiFor();
  render(<MemoryRouter><OverviewPage api={api} /></MemoryRouter>);

  await waitFor(() => expect(api.listAgents).toHaveBeenCalledTimes(1));
  expect(api.listTasks).toHaveBeenCalledTimes(1);
  expect(api.getAnalysisStatus).toHaveBeenCalledTimes(1);
  await waitFor(() => expect(api.getGroupsStats).toHaveBeenCalledTimes(1));
});

test("shows the connecting state without starting business polling", async () => {
  const api = apiFor({
    getRuntimeStatus: vi.fn().mockResolvedValue({ databaseState: "connecting", databaseErrorCode: "", agents: [], restarting: false, recoveryURL: "" }),
    listAgents: vi.fn(), listTasks: vi.fn(), getAnalysisStatus: vi.fn()
  });
  render(<MemoryRouter><OverviewPage api={api} /></MemoryRouter>);

  expect(await screen.findByText("正在连接 PostgreSQL…")).toBeInTheDocument();
  expect(api.listAgents).not.toHaveBeenCalled();
});

test("aborts business polling when runtime changes from connected to error", async () => {
  vi.useFakeTimers();
  vi.spyOn(document, "hasFocus").mockReturnValue(true);
  let agentsSignal: AbortSignal | undefined;
  const api = apiFor({
    getRuntimeStatus: vi.fn()
      .mockResolvedValueOnce({ databaseState: "connected", databaseErrorCode: "", agents: [], restarting: false, recoveryURL: "" })
      .mockResolvedValueOnce({ databaseState: "error", databaseErrorCode: "postgres_unavailable", agents: [], restarting: false, recoveryURL: "" }),
    listAgents: vi.fn((signal?: AbortSignal) => new Promise<AgentStatus[]>(() => { agentsSignal = signal; }))
  });
  render(<MemoryRouter><OverviewPage api={api} /></MemoryRouter>);

  await act(async () => {});
  await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });
  expect(agentsSignal?.aborted).toBe(true);
  vi.useRealTimers();
});

test("marks retained business data as stale after the database disconnects", async () => {
  vi.useFakeTimers();
  vi.spyOn(document, "hasFocus").mockReturnValue(true);
  const api = apiFor({
    getRuntimeStatus: vi.fn()
      .mockResolvedValueOnce({ databaseState: "connected", databaseErrorCode: "", agents: [], restarting: false, recoveryURL: "" })
      .mockResolvedValue({ databaseState: "error", databaseErrorCode: "postgres_unavailable", agents: [], restarting: false, recoveryURL: "" })
  });
  render(<MemoryRouter><OverviewPage api={api} /></MemoryRouter>);

  await act(async () => {});
  expect(screen.queryByRole("note")).not.toBeInTheDocument();
  await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });
  expect(screen.getByRole("note")).toHaveTextContent("以下为断开前最后数据");
  vi.useRealTimers();
});

test("cards link to their workspaces and summarize failures, identity and groups", async () => {
  const api = apiFor({
    listAgents: vi.fn().mockResolvedValue([
      { machineId: "a", addr: "10.0.0.1", online: true, identityState: "claimed" },
      { machineId: "b", addr: "10.0.0.2", online: true, identityState: "pending" },
      { machineId: "c", addr: "10.0.0.3", online: true, identityState: "conflict" }
    ]),
    listTasks: vi.fn().mockResolvedValue([
      { taskId: "t1", status: "running", scanErrors: 2 },
      { taskId: "t2", status: "failed", scanErrors: 3 },
      { taskId: "t3", status: "done", scanErrors: 0 }
    ]),
    getAnalysisStatus: vi.fn().mockResolvedValue({
      running: false,
      last: { filesScanned: 1234, groupsWritten: 5, membersWritten: 11 },
      lastErr: ""
    }),
    getGroupsStats: vi.fn().mockResolvedValue({ kind: "", groups: 8, totalBytes: 16_000, wastedBytes: 4 * 1024 ** 3 })
  });
  render(<MemoryRouter><OverviewPage api={api} /></MemoryRouter>);

  expect(await screen.findByText("在线 1 / 共 3")).toBeInTheDocument();
  expect(screen.getByText("待识别 1 / 身份冲突 1")).toBeInTheDocument();
  expect(screen.getByText("进行中 1 / 当前已加载 3")).toBeInTheDocument();
  expect(screen.getByText("失败 1 · 扫描错误 5")).toBeInTheDocument();
  expect(screen.getByText("已扫描文件 1234")).toBeInTheDocument();
  expect(screen.getByText("可回收空间 4.0 GB（共 8 组）")).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "Agent 概况" })).toHaveAttribute("href", "/agents");
  expect(screen.getByRole("link", { name: "扫描概况" })).toHaveAttribute("href", "/scans");
  expect(screen.getByRole("link", { name: "分析概况" })).toHaveAttribute("href", "/analysis");
  expect(screen.getByRole("link", { name: "重复组概况" })).toHaveAttribute("href", "/groups");
});

test("shows the pipeline status strip and restarts all polling on manual refresh", async () => {
  const listTasks = vi.fn().mockResolvedValue([{ taskId: "t1", status: "running", scanErrors: 0 }]);
  const getRuntimeStatus = vi.fn().mockResolvedValue({
    databaseState: "connected",
    databaseErrorCode: "",
    agents: [],
    restarting: false,
    recoveryURL: ""
  });
  const api = apiFor({
    getRuntimeStatus,
    listTasks,
    getGroupsStats: vi.fn().mockResolvedValue({ kind: "", groups: 9, totalBytes: 30_000, wastedBytes: 12_000 })
  });
  const user = userEvent.setup();
  render(<MemoryRouter><OverviewPage api={api} /></MemoryRouter>);

  const flow = await screen.findByLabelText("主流程状态");
  expect(flow).toHaveTextContent("扫描中 1");
  expect(flow).toHaveTextContent("待分析");
  expect(flow).toHaveTextContent("待处理组 9");

  await user.click(screen.getByRole("button", { name: "刷新" }));
  await waitFor(() => expect(listTasks.mock.calls.length).toBeGreaterThanOrEqual(2));
  expect(getRuntimeStatus.mock.calls.length).toBeGreaterThanOrEqual(2);
});

test("shows a restart banner while the Manager is restarting", async () => {
  const api = apiFor({
    getRuntimeStatus: vi.fn().mockResolvedValue({
      databaseState: "connecting",
      databaseErrorCode: "",
      agents: [],
      restarting: true,
      recoveryURL: ""
    })
  });
  render(<MemoryRouter><OverviewPage api={api} /></MemoryRouter>);

  expect(await screen.findByRole("status")).toHaveTextContent("Manager 正在重启，稍后自动恢复");
});

test("keeps polling analysis status while idle instead of stopping permanently", async () => {
  vi.useFakeTimers();
  vi.spyOn(document, "hasFocus").mockReturnValue(true);
  const getAnalysisStatus = vi.fn().mockResolvedValue({ running: false, last: null, lastErr: "" });
  const api = apiFor({ getAnalysisStatus });
  render(<MemoryRouter><OverviewPage api={api} /></MemoryRouter>);

  await act(async () => {});
  expect(getAnalysisStatus).toHaveBeenCalledTimes(1);
  await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });
  expect(getAnalysisStatus.mock.calls.length).toBeGreaterThanOrEqual(2);
  vi.useRealTimers();
});
