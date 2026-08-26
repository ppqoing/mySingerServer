import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, test, vi } from "vitest";
import { ApiError } from "../../api/client";
import type { AppApi, AgentStatus, GUIConfigSnapshot, ScanTask } from "../../api/contracts";
import { ScansPage } from "./ScansPage";

const online: AgentStatus = { machineId: "agent-a", addr: "10.0.0.1", online: true, identityState: "claimed" };
const offline: AgentStatus = { machineId: "agent-b", addr: "10.0.0.2", online: false, identityState: "claimed" };

function task(overrides: Partial<ScanTask> = {}): ScanTask {
  return {
    taskId: "task-1", machineId: "agent-a", phase: 1, roots: ["D:\\Music"], rescan: false,
    status: "running", done: 0, total: 10, skipped: 0, failed: 0, scanErrors: 0, elapsedMs: 0,
    speed: 0, recent: [], updatedAt: new Date().toISOString(), ...overrides
  };
}

const terminal = task({
  taskId: "task-done", status: "done", done: 2, total: 2, elapsedMs: 10,
  speed: 1, updatedAt: "2026-07-31T00:00:00Z"
});

function guiConfigSnapshot(autoDispatch: boolean): GUIConfigSnapshot {
  return {
    config: {
      listenAddr: "127.0.0.1:9310",
      pgDsn: "",
      agents: [],
      heartbeatS: 5,
      firstScreen: {
        hammingMax: 12, aspectTolerance: 0.12, videoDurationWindowMs: 1500, imageQualityMin: 20,
        readPageSize: 200, groupInsertBatch: 200, shaResolveChunk: 200
      },
      phase2: {
        phashPassT2: 12, phashPartThreshold: 3, sobelT3: 96, videoFrames: 6, videoAvgT4: 12,
        videoMinPassed: 4, videoMinValid: 4, videoFileTimeoutS: 30, videoFrameCommandTimeoutS: 10,
        imageFileTimeoutS: 20, taskShardSize: 100, autoDispatch
      }
    },
    restartRequired: false
  };
}

function apiFor(overrides: Partial<AppApi> = {}): AppApi {
  return {
    listAgents: vi.fn().mockResolvedValue([online, offline]),
    listTasks: vi.fn().mockResolvedValue([]),
    startScan: vi.fn().mockResolvedValue({ taskId: "task-new" }),
    cancelTask: vi.fn().mockResolvedValue(undefined),
    loadGUIConfig: vi.fn().mockResolvedValue(guiConfigSnapshot(true)),
    ...overrides
  } as unknown as AppApi;
}

function renderPage(api: AppApi) {
  return render(<MemoryRouter><ScansPage api={api} /></MemoryRouter>);
}

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("ScansPage", () => {
  test("deduplicates identities and prefers the online claimed connection", async () => {
    const duplicate = { ...online, addr: "10.0.0.9", online: false, identityState: "conflict" as const };
    const pending: AgentStatus = { machineId: "", addr: "10.0.0.3", online: false, identityState: "pending" };
    renderPage(apiFor({ listAgents: vi.fn().mockResolvedValue([duplicate, pending, online]) }));

    await screen.findByRole("option", { name: /agent-a/ });
    expect(screen.getAllByRole("option").filter(option => option.getAttribute("value") === "agent-a")).toHaveLength(1);
    expect(screen.queryByRole("option", { name: "待识别" })).not.toBeInTheDocument();
    expect(screen.getByRole("option", { name: /agent-a/ })).toBeEnabled();
    expect(screen.getByRole("option", { name: /agent-a/ })).toHaveTextContent("10.0.0.1");
  });

  test("opens remote browsing when an earlier duplicate Agent record is offline", async () => {
    const offlineDuplicate: AgentStatus = { ...online, addr: "10.0.0.9", online: false, identityState: "conflict" };
    const api = apiFor({
      listAgents: vi.fn().mockResolvedValue([offlineDuplicate, online]),
      browseAgentFilesystem: vi.fn().mockResolvedValue({ currentPath: "", parentPath: "", entries: [], nextCursor: "" })
    });
    renderPage(api);
    const user = userEvent.setup();

    await user.selectOptions(await screen.findByLabelText("扫描 Agent"), "agent-a");
    await user.click(screen.getByRole("button", { name: "选择目录…" }));

    expect(await screen.findByRole("dialog", { name: "选择目录" })).toBeVisible();
  });

  test("rejects an absent machine or empty parsed roots before calling the API", async () => {
    const api = apiFor();
    const user = userEvent.setup();
    renderPage(api);
    await screen.findByRole("option", { name: /agent-a/ });

    await user.click(screen.getByRole("button", { name: "创建扫描任务" }));
    expect(screen.getByRole("alert")).toHaveTextContent("请选择在线 Agent");
    await user.selectOptions(screen.getByLabelText("扫描 Agent"), "agent-a");
    await user.click(screen.getByRole("button", { name: "创建扫描任务" }));
    expect(screen.getByRole("alert")).toHaveTextContent("至少输入一个扫描根目录");
    expect(api.startScan).not.toHaveBeenCalled();
  });

  test("adds a manual Windows root and submits the selected root list", async () => {
    const api = apiFor();
    const user = userEvent.setup();
    renderPage(api);
    await screen.findByRole("option", { name: /agent-a/ });
    await user.selectOptions(screen.getByLabelText("扫描 Agent"), "agent-a");
    await user.type(screen.getByLabelText("手工根目录"), "D:\\Music");
    await user.click(screen.getByRole("button", { name: "添加根目录" }));
    await user.click(screen.getByRole("button", { name: "创建扫描任务" }));

    await waitFor(() => expect(api.startScan).toHaveBeenCalledWith({
      machineId: "agent-a", roots: ["D:\\Music"], phase: 1, rescan: false
    }, expect.any(AbortSignal)));
    expect(screen.getByText("已创建任务：task-new")).toBeInTheDocument();
    expect(screen.getByRole("option", { name: /agent-b/ })).toBeDisabled();
  });

  test("keeps form values after a failed submission", async () => {
    const api = apiFor({ startScan: vi.fn().mockRejectedValue(new Error("Agent 不可达")) });
    const user = userEvent.setup();
    renderPage(api);
    await screen.findByRole("option", { name: /agent-a/ });
    await user.selectOptions(screen.getByLabelText("扫描 Agent"), "agent-a");
    await user.type(screen.getByLabelText("手工根目录"), "D:\\Music");
    await user.click(screen.getByRole("button", { name: "添加根目录" }));
    await user.click(screen.getByLabelText("重新扫描"));
    await user.click(screen.getByRole("button", { name: "创建扫描任务" }));

    await screen.findByRole("alert");
    expect(screen.getByLabelText("扫描 Agent")).toHaveValue("agent-a");
    expect(screen.getByText("D:\\Music")).toBeVisible();
    expect(screen.getByLabelText("重新扫描")).toBeChecked();
  });

  test("blocks a duplicate submission for the same agent and normalized roots", async () => {
    const inFlight = task({ roots: ["D:\\Music"], status: "running" });
    const api = apiFor({ listTasks: vi.fn().mockResolvedValue([inFlight]) });
    const user = userEvent.setup();
    renderPage(api);
    await screen.findByText("task-1");
    await user.selectOptions(await screen.findByLabelText("扫描 Agent"), "agent-a");
    await user.type(screen.getByLabelText("手工根目录"), "d:/music/");
    await user.click(screen.getByRole("button", { name: "添加根目录" }));
    await user.click(screen.getByRole("button", { name: "创建扫描任务" }));

    expect(screen.getByRole("alert")).toHaveTextContent("相同根目录的进行中任务");
    expect(api.startScan).not.toHaveBeenCalled();
  });

  test("keeps a stale success message and a fresh error mutually exclusive", async () => {
    const startScan = vi.fn()
      .mockRejectedValueOnce(new Error("连接超时"))
      .mockResolvedValueOnce({ taskId: "task-ok" });
    const api = apiFor({ startScan });
    const user = userEvent.setup();
    renderPage(api);
    await screen.findByRole("option", { name: /agent-a/ });
    await user.selectOptions(screen.getByLabelText("扫描 Agent"), "agent-a");
    await user.type(screen.getByLabelText("手工根目录"), "D:\\Music");
    await user.click(screen.getByRole("button", { name: "添加根目录" }));

    await user.click(screen.getByRole("button", { name: "创建扫描任务" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("连接超时");
    expect(screen.queryByText(/已创建任务/)).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "创建扫描任务" }));
    expect(await screen.findByText("已创建任务：task-ok")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  test("rejects a selected Agent that became offline in the latest snapshot", async () => {
    vi.useFakeTimers();
    vi.spyOn(document, "hasFocus").mockReturnValue(true);
    const listAgents = vi.fn()
      .mockResolvedValueOnce([online])
      .mockResolvedValue([{ ...online, online: false }]);
    const api = apiFor({ listAgents });
    renderPage(api);
    await act(async () => {});
    fireEvent.change(screen.getByLabelText("扫描 Agent"), { target: { value: "agent-a" } });
    fireEvent.change(screen.getByLabelText("手工根目录"), { target: { value: "D:\\Music" } });
    fireEvent.click(screen.getByRole("button", { name: "添加根目录" }));

    await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });
    expect(listAgents).toHaveBeenCalledTimes(2);
    fireEvent.click(screen.getByRole("button", { name: "创建扫描任务" }));

    expect(screen.getByRole("alert")).toHaveTextContent("所选 Agent 当前离线，请重新选择。");
    expect(api.startScan).not.toHaveBeenCalled();
  });

  test("stops automatic task polling once every loaded task is terminal", async () => {
    vi.useFakeTimers();
    vi.spyOn(document, "hasFocus").mockReturnValue(true);
    const api = apiFor({ listTasks: vi.fn().mockResolvedValue([terminal]) });
    renderPage(api);
    await act(async () => {});
    expect(screen.getByText("已结束任务（1）")).toBeInTheDocument();
    await act(async () => { await vi.advanceTimersByTimeAsync(12_000); });
    expect(api.listTasks).toHaveBeenCalledTimes(1);
  });

  test("restarts task polling on manual refresh after all tasks are terminal", async () => {
    const api = apiFor({ listTasks: vi.fn().mockResolvedValue([terminal]) });
    const user = userEvent.setup();
    renderPage(api);
    await screen.findByText("task-done");

    await user.click(screen.getByRole("button", { name: "刷新任务列表" }));
    await waitFor(() => expect(api.listTasks).toHaveBeenCalledTimes(2));
  });

  test("collapses finished tasks by default and filters by status", async () => {
    const runningTask = task({ taskId: "task-run" });
    const doneTask = task({ taskId: "task-done", status: "done", done: 10, total: 10 });
    const api = apiFor({ listTasks: vi.fn().mockResolvedValue([runningTask, doneTask]) });
    const user = userEvent.setup();
    renderPage(api);
    await screen.findByText("task-run");
    expect(screen.getByRole("region", { name: "扫描任务数据表" })).toBeInTheDocument();
    expect(screen.getByText("已结束任务（1）")).toBeInTheDocument();

    await user.selectOptions(screen.getByLabelText("任务筛选"), "active");
    expect(screen.queryByText("task-done")).not.toBeInTheDocument();
    expect(screen.getByText("task-run")).toBeInTheDocument();

    await user.selectOptions(screen.getByLabelText("任务筛选"), "finished");
    expect(screen.queryByText("task-run")).not.toBeInTheDocument();
    expect(screen.getByText("task-done")).toBeInTheDocument();
    expect(screen.getByRole("region", { name: "扫描任务数据表" })).toBeInTheDocument();
  });

  test("localizes status columns, formats metrics, and expands recent error details", async () => {
    const runningTask = task({
      taskId: "task-run", roots: ["D:\\Music", "E:\\More"], done: 5, skipped: 1, failed: 1,
      scanErrors: 2, speed: 0.833_3, lastErr: "最近读取失败",
      recent: [
        { path: "D:\\Music\\a.jpg", status: "failed", err: "读取被拒绝" },
        { path: "D:\\Music\\b.jpg", status: "done" }
      ]
    });
    const view = renderPage(apiFor({ listTasks: vi.fn().mockResolvedValue([runningTask]) }));

    const row = (await screen.findByText("task-run")).closest("tr")!;
    const rowScope = within(row);
    expect(rowScope.getByText("运行中")).toBeInTheDocument();
    expect(rowScope.getByText("D:\\Music 等 2 个")).toHaveAttribute("title", "D:\\Music\nE:\\More");
    expect(view.container.querySelector(".scan-task-errors")).toHaveTextContent("2");
    // 非终态任务无完成方式与耗时
    expect(rowScope.getAllByText("—")).toHaveLength(2);
    expect(rowScope.getByText("0.8 文件/秒")).toBeInTheDocument();
    expect(rowScope.getByText("最近读取失败")).toBeInTheDocument();
    expect(rowScope.getByText("错误明细（1）")).toBeInTheDocument();
    expect(rowScope.getByText(/读取被拒绝/)).toBeInTheDocument();
  });

  test("offers an analysis next step and outcome text for finished tasks", async () => {
    const doneSkipped = task({
      taskId: "task-skip", status: "done", ackReason: "already_done", done: 8, total: 10, skipped: 2
    });
    const doneReal = task({ taskId: "task-real", status: "done", done: 10, total: 10 });
    renderPage(apiFor({ listTasks: vi.fn().mockResolvedValue([doneSkipped, doneReal]) }));

    await screen.findByText("已结束任务（2）");
    expect(screen.getByText("已跳过：already_done")).toBeInTheDocument();
    const row = screen.getByText("task-real").closest("tr")!;
    // 状态列与完成方式列各一处"已完成"
    expect(within(row).getAllByText("已完成")).toHaveLength(2);
    const links = screen.getAllByRole("link", { hidden: true, name: /运行一筛分析/ });
    expect(links).toHaveLength(2);
    expect(links[0]).toHaveAttribute("href", "/analysis");
  });

  test("highlights non-terminal tasks stalled for over five minutes", async () => {
    const stalled = task({ taskId: "task-stalled", updatedAt: new Date(Date.now() - 10 * 60_000).toISOString() });
    const fresh = task({ taskId: "task-fresh" });
    renderPage(apiFor({ listTasks: vi.fn().mockResolvedValue([stalled, fresh]) }));

    await screen.findByText("task-stalled");
    expect(screen.getByText("task-stalled").closest("tr")).toHaveClass("scan-task-row--stalled");
    expect(screen.getByText("task-fresh").closest("tr")).not.toHaveClass("scan-task-row--stalled");
  });

  test("browses the selected Agent and submits multiple roots", async () => {
    const api = apiFor({
      browseAgentFilesystem: vi.fn().mockResolvedValue({
        currentPath: "D:\\Media", parentPath: "D:\\", nextCursor: "",
        entries: [{ name: "Photos", path: "D:\\Media\\Photos", kind: "directory", hidden: false, system: false, selectable: true }]
      })
    });
    renderPage(api);
    const user = userEvent.setup();
    await user.selectOptions(await screen.findByLabelText("扫描 Agent"), "agent-a");
    await user.click(screen.getByRole("button", { name: "选择目录…" }));
    await user.click(await screen.findByRole("button", { name: /Photos/ }));
    await user.click(screen.getByRole("button", { name: "添加当前目录" }));
    expect(screen.getByText("D:\\Media\\Photos")).toBeVisible();
  });

  test("disables remote browsing until an online Agent is selected", async () => {
    renderPage(apiFor());
    expect(screen.getByRole("button", { name: "选择目录…" })).toBeDisabled();
    await screen.findByRole("option", { name: /agent-a/ });
    await userEvent.setup().selectOptions(screen.getByLabelText("扫描 Agent"), "agent-a");
    expect(screen.getByRole("button", { name: "选择目录…" })).toBeEnabled();
  });

  test("clears draft roots and explains why when changing Agent", async () => {
    const secondOnline = { ...online, machineId: "agent-c", addr: "10.0.0.3" };
    renderPage(apiFor({ listAgents: vi.fn().mockResolvedValue([online, secondOnline]) }));
    const user = userEvent.setup();
    await screen.findByRole("option", { name: /agent-a/ });
    await user.selectOptions(screen.getByLabelText("扫描 Agent"), "agent-a");
    await user.type(screen.getByLabelText("手工根目录"), "D:\\Music");
    await user.click(screen.getByRole("button", { name: "添加根目录" }));
    await user.selectOptions(screen.getByLabelText("扫描 Agent"), "agent-c");

    expect(screen.queryByText("D:\\Music")).not.toBeInTheDocument();
    expect(screen.getByText("切换 Agent 后已清空待选根目录。")).toHaveAttribute("role", "status");
  });

  test("asks before replacing a child root with its parent", async () => {
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    renderPage(apiFor());
    const user = userEvent.setup();
    await screen.findByRole("option", { name: /agent-a/ });
    await user.selectOptions(screen.getByLabelText("扫描 Agent"), "agent-a");
    await user.type(screen.getByLabelText("手工根目录"), "D:\\Media\\Photos");
    await user.click(screen.getByRole("button", { name: "添加根目录" }));
    await user.clear(screen.getByLabelText("手工根目录"));
    await user.type(screen.getByLabelText("手工根目录"), "D:\\Media");
    await user.click(screen.getByRole("button", { name: "添加根目录" }));

    expect(confirm).toHaveBeenCalled();
    expect(screen.getByText("D:\\Media\\Photos")).toBeVisible();
    expect(screen.queryByText("D:\\Media", { exact: true })).not.toBeInTheDocument();
  });

  test("shows the phase2 auto-dispatch config state beside the form", async () => {
    renderPage(apiFor());

    expect(await screen.findByText("phase2 自动派发：开")).toBeInTheDocument();
  });

  test("omits the auto-dispatch hint when the config cannot be read", async () => {
    renderPage(apiFor({ loadGUIConfig: vi.fn().mockRejectedValue(new Error("denied")) }));

    await screen.findByRole("option", { name: /agent-a/ });
    expect(screen.queryByText(/phase2 自动派发/)).not.toBeInTheDocument();
  });

  test("defaults to phase 1 and submits phase 2 when the second-stage radio is selected", async () => {
    const api = apiFor();
    const user = userEvent.setup();
    renderPage(api);
    await screen.findByRole("option", { name: /agent-a/ });
    await user.selectOptions(screen.getByLabelText("扫描 Agent"), "agent-a");
    await user.type(screen.getByLabelText("手工根目录"), "D:\\Music");
    await user.click(screen.getByRole("button", { name: "添加根目录" }));

    expect(screen.getByRole("radio", { name: /一筛/ })).toBeChecked();
    await user.click(screen.getByRole("radio", { name: /二筛/ }));
    await user.click(screen.getByRole("button", { name: "创建扫描任务" }));

    await waitFor(() => expect(api.startScan).toHaveBeenCalledWith({
      machineId: "agent-a", roots: ["D:\\Music"], phase: 2, rescan: false
    }, expect.any(AbortSignal)));
  });

  test("shows the scan phase column in the task table", async () => {
    renderPage(apiFor({
      listTasks: vi.fn().mockResolvedValue([task(), task({ taskId: "task-p2", phase: 2 })])
    }));

    const phase2Row = (await screen.findByText("task-p2")).closest("tr")!;
    expect(within(phase2Row).getByText("二筛")).toBeInTheDocument();
    const phase1Row = screen.getByText("task-1").closest("tr")!;
    expect(within(phase1Row).getByText("一筛")).toBeInTheDocument();
  });

  test("stops a running task and hands the optimistic state to the server status", async () => {
    let release!: () => void;
    const cancelTask = vi.fn().mockReturnValue(new Promise<void>(resolve => { release = resolve; }));
    const listTasks = vi.fn()
      .mockResolvedValueOnce([task()])
      .mockResolvedValue([task({ status: "cancelling" })]);
    renderPage(apiFor({ cancelTask, listTasks }));
    const user = userEvent.setup();

    await user.click(await screen.findByRole("button", { name: "停止" }));
    expect(cancelTask).toHaveBeenCalledWith("task-1");
    // 乐观中间态：请求在途即禁用并显示"正在停止…"
    expect(screen.getByRole("button", { name: "正在停止…" })).toBeDisabled();

    await act(async () => release());
    // 服务端 cancelling 状态接管：状态列中文化，按钮保持禁用
    const row = (await screen.findByText("task-1")).closest("tr")!;
    expect(within(row).getByText("正在停止")).toBeInTheDocument();
    expect(within(row).getByRole("button", { name: "正在停止…" })).toBeDisabled();
  });

  test("marks a cancelled task as 已取消 without the analysis next-step link", async () => {
    const cancelled = task({ taskId: "task-cancelled", status: "failed", ackReason: "cancelled" });
    renderPage(apiFor({ listTasks: vi.fn().mockResolvedValue([cancelled]) }));

    await screen.findByText("已结束任务（1）");
    const row = screen.getByText("task-cancelled").closest("tr")!;
    expect(within(row).getByText("已取消")).toBeInTheDocument();
    expect(screen.queryByRole("link", { hidden: true, name: /运行一筛分析/ })).not.toBeInTheDocument();
    expect(within(row).queryByRole("button")).not.toBeInTheDocument();
  });

  test("explains when the task already finished (409) and refreshes the list", async () => {
    const cancelTask = vi.fn().mockRejectedValue(new ApiError(409, "task_terminal", false));
    const listTasks = vi.fn()
      .mockResolvedValueOnce([task()])
      .mockResolvedValue([task({ status: "done", done: 10, total: 10 })]);
    renderPage(apiFor({ cancelTask, listTasks }));
    const user = userEvent.setup();

    await user.click(await screen.findByRole("button", { name: "停止" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("任务已结束，无法停止。");
    await waitFor(() => expect(listTasks).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.queryByRole("button", { name: "停止" })).not.toBeInTheDocument());
  });

  test("explains when the task no longer exists (404) and re-enables the stop button", async () => {
    const cancelTask = vi.fn().mockRejectedValue(new ApiError(404, "task_not_found", false));
    renderPage(apiFor({ cancelTask, listTasks: vi.fn().mockResolvedValue([task()]) }));
    const user = userEvent.setup();

    await user.click(await screen.findByRole("button", { name: "停止" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("任务不存在或已被清除。");
    expect(await screen.findByRole("button", { name: "停止" })).toBeEnabled();
  });

  test("translates agent_offline when the agent is unreachable (503)", async () => {
    const cancelTask = vi.fn().mockRejectedValue(new ApiError(503, "agent_offline", true));
    renderPage(apiFor({ cancelTask, listTasks: vi.fn().mockResolvedValue([task()]) }));
    const user = userEvent.setup();

    await user.click(await screen.findByRole("button", { name: "停止" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("Agent 离线：请确认目标节点在线后重试。");
  });
});
