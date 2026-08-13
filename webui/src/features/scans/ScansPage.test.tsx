import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, test, vi } from "vitest";
import type { AppApi, AgentStatus, ScanTask } from "../../api/contracts";
import { ScansPage } from "./ScansPage";

const online: AgentStatus = { machineId: "agent-a", addr: "10.0.0.1", online: true, identityState: "claimed" };
const offline: AgentStatus = { machineId: "agent-b", addr: "10.0.0.2", online: false, identityState: "claimed" };
const terminal: ScanTask = {
  taskId: "task-done", machineId: "agent-a", phase: 1, roots: ["D:\\Music"], rescan: false,
  status: "done", done: 2, total: 2, skipped: 0, failed: 0, scanErrors: 0, elapsedMs: 10,
  speed: 1, recent: [], updatedAt: "2026-07-31T00:00:00Z"
};

function apiFor(overrides: Partial<AppApi> = {}): AppApi {
  return {
    listAgents: vi.fn().mockResolvedValue([online, offline]),
    listTasks: vi.fn().mockResolvedValue([]),
    startScan: vi.fn().mockResolvedValue({ taskId: "task-new" }),
    ...overrides
  } as unknown as AppApi;
}

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("ScansPage", () => {
  test("deduplicates identities and prefers the online claimed connection", async () => {
    const duplicate = { ...online, addr: "10.0.0.9", online: false, identityState: "conflict" as const };
    const pending: AgentStatus = { machineId: "", addr: "10.0.0.3", online: false, identityState: "pending" };
    render(<ScansPage api={apiFor({ listAgents: vi.fn().mockResolvedValue([duplicate, pending, online]) })} />);

    await screen.findByRole("option", { name: /agent-a/ });
    expect(screen.getAllByRole("option").filter(option => option.getAttribute("value") === "agent-a")).toHaveLength(1);
    expect(screen.queryByRole("option", { name: "待识别" })).not.toBeInTheDocument();
    expect(screen.getByRole("option", { name: /agent-a/ })).toBeEnabled();
  });

  test("rejects an absent machine or empty parsed roots before calling the API", async () => {
    const api = apiFor();
    const user = userEvent.setup();
    render(<ScansPage api={api} />);
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
    render(<ScansPage api={api} />);
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
    render(<ScansPage api={api} />);
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

  test("rejects a selected Agent that became offline in the latest snapshot", async () => {
    vi.useFakeTimers();
    vi.spyOn(document, "hasFocus").mockReturnValue(true);
    const listAgents = vi.fn()
      .mockResolvedValueOnce([online])
      .mockResolvedValue([{ ...online, online: false }]);
    const api = apiFor({ listAgents });
    render(<ScansPage api={api} />);
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
    render(<ScansPage api={api} />);
    await act(async () => {});
    expect(screen.getByRole("region", { name: "扫描任务数据表" })).toBeInTheDocument();
    await act(async () => { await vi.advanceTimersByTimeAsync(12_000); });
    expect(api.listTasks).toHaveBeenCalledTimes(1);
  });

  test("browses the selected Agent and submits multiple roots", async () => {
    const api = apiFor({
      browseAgentFilesystem: vi.fn().mockResolvedValue({
        currentPath: "D:\\Media", parentPath: "D:\\", nextCursor: "",
        entries: [{ name: "Photos", path: "D:\\Media\\Photos", kind: "directory", hidden: false, system: false, selectable: true }]
      })
    });
    render(<ScansPage api={api} />);
    const user = userEvent.setup();
    await user.selectOptions(await screen.findByLabelText("扫描 Agent"), "agent-a");
    await user.click(screen.getByRole("button", { name: "选择目录…" }));
    await user.click(await screen.findByRole("button", { name: /Photos/ }));
    await user.click(screen.getByRole("button", { name: "添加当前目录" }));
    expect(screen.getByText("D:\\Media\\Photos")).toBeVisible();
  });

  test("disables remote browsing until an online Agent is selected", async () => {
    render(<ScansPage api={apiFor()} />);
    expect(screen.getByRole("button", { name: "选择目录…" })).toBeDisabled();
    await screen.findByRole("option", { name: /agent-a/ });
    await userEvent.setup().selectOptions(screen.getByLabelText("扫描 Agent"), "agent-a");
    expect(screen.getByRole("button", { name: "选择目录…" })).toBeEnabled();
  });

  test("clears draft roots and explains why when changing Agent", async () => {
    const secondOnline = { ...online, machineId: "agent-c", addr: "10.0.0.3" };
    render(<ScansPage api={apiFor({ listAgents: vi.fn().mockResolvedValue([online, secondOnline]) })} />);
    const user = userEvent.setup();
    await screen.findByRole("option", { name: /agent-a/ });
    await user.selectOptions(screen.getByLabelText("扫描 Agent"), "agent-a");
    await user.type(screen.getByLabelText("手工根目录"), "D:\\Music");
    await user.click(screen.getByRole("button", { name: "添加根目录" }));
    await user.selectOptions(screen.getByLabelText("扫描 Agent"), "agent-c");

    expect(screen.queryByText("D:\\Music")).not.toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent("切换 Agent 后已清空待选根目录");
  });

  test("asks before replacing a child root with its parent", async () => {
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    render(<ScansPage api={apiFor()} />);
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
});
