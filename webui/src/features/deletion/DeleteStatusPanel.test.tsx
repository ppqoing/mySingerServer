import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, test, vi } from "vitest";
import type { AppApi, DeleteTaskStatus, DeleteTaskSummary } from "../../api/contracts";
import type { DeleteReviewSnapshot } from "../groups/deleteReview";
import { DeleteStatusPanel } from "./DeleteStatusPanel";

const status: DeleteTaskStatus = {
  taskId: "task-1", mode: "soft", total: 4, ok: 2, failed: 1, uncertain: 0, pending: 1,
  complete: false, stateSyncFailures: 1,
  byMachine: {
    "agent-b": { machineId: "agent-b", total: 2, ok: 1, failed: 1, uncertain: 0, pending: 0, complete: true, stateSyncFailures: 1, sequences: {} },
    "agent-a": { machineId: "agent-a", total: 2, ok: 1, failed: 0, uncertain: 0, pending: 1, complete: false, stateSyncFailures: 0, sequences: {} }
  },
  errorCodes: { "E_HELPER_LOST": 1, "<script>alert(1)</script>": 2 },
  problems: [{ machineId: "agent-b", sequence: 3, path: "D:\\<img src=x>", errorCode: "<svg>", errorMessage: "<b>bad</b>", uncertain: false }]
};

function apiFor(
  getDeleteStatus = vi.fn().mockResolvedValue(status),
  listDeleteTasks = vi.fn().mockResolvedValue([] as DeleteTaskSummary[])
): AppApi {
  return { getDeleteStatus, listDeleteTasks } as unknown as AppApi;
}

const runningSummary: DeleteTaskSummary = {
  taskId: "11111111-1111-4111-8111-111111111111",
  mode: "soft",
  total: 4,
  ok: 1,
  failed: 0,
  uncertain: 0,
  pending: 3,
  complete: false,
  createdAt: "2026-08-14T01:02:03Z"
};

const doneSummary: DeleteTaskSummary = {
  taskId: "task-done-2",
  mode: "hard",
  total: 2,
  ok: 2,
  failed: 0,
  uncertain: 0,
  pending: 0,
  complete: true,
  createdAt: "2026-08-13T10:00:00Z"
};

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("DeleteStatusPanel", () => {
  test("renders per-machine progress sorted by machine ID and literal problem text", async () => {
    render(<DeleteStatusPanel taskId="task-1" api={apiFor()} />);
    await screen.findByText("agent-a");
    expect(screen.getAllByTestId("delete-machine-row").map(row => row.textContent)).toEqual([
      expect.stringContaining("agent-a"), expect.stringContaining("agent-b")
    ]);
    expect(screen.getAllByRole("listitem").some(node => node.textContent?.includes("D:\\<img src=x>"))).toBe(true);
    expect(screen.getAllByRole("listitem").some(node => node.textContent?.includes("<b>bad</b>"))).toBe(true);
    expect(screen.getByText("E_HELPER_LOST（Helper 连接丢失）：1")).toBeInTheDocument();
    expect(screen.getAllByRole("listitem").some(node => node.textContent?.includes("<script>alert(1)</script>：2"))).toBe(true);
    expect(screen.getAllByRole("listitem").some(node => node.textContent?.includes("代码：<svg>"))).toBe(true);
    expect(screen.getByRole("region", { name: "删除 Agent 进度表" })).toBeInTheDocument();
    expect(document.querySelector("img, svg, script")).toBeNull();
  });

  test("asks for a task ID when an uncontrolled lookup is empty", () => {
    const api = apiFor();
    render(<DeleteStatusPanel api={api} />);

    fireEvent.click(screen.getByRole("button", { name: "查询" }));

    expect(screen.getByRole("alert")).toHaveTextContent("请输入删除任务 ID");
    expect(api.getDeleteStatus).not.toHaveBeenCalled();
  });

  test("reports a missing controlled task ID without requesting the API", () => {
    const api = apiFor();
    render(<DeleteStatusPanel taskId="" api={api} />);

    expect(screen.getByRole("alert")).toHaveTextContent("缺少删除任务 ID");
    expect(api.getDeleteStatus).not.toHaveBeenCalled();
  });

  test("hides retained task status when a controlled task ID becomes empty", async () => {
    const view = render(<DeleteStatusPanel taskId="task-1" api={apiFor()} />);
    await screen.findByText("agent-a");

    view.rerender(<DeleteStatusPanel taskId="" api={apiFor()} />);

    expect(screen.getByRole("alert")).toHaveTextContent("缺少删除任务 ID");
    expect(screen.queryByText("agent-a")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("删除任务总览")).not.toBeInTheDocument();
  });

  test("hides a retained polling error when a controlled task ID becomes empty", async () => {
    const api = apiFor(vi.fn().mockRejectedValue(new Error("旧任务查询失败")));
    const view = render(<DeleteStatusPanel taskId="task-1" api={api} />);
    expect(await screen.findByRole("alert")).toHaveTextContent("旧任务查询失败");

    view.rerender(<DeleteStatusPanel taskId="" api={api} />);

    expect(screen.getByRole("alert")).toHaveTextContent("缺少删除任务 ID");
    expect(screen.queryByText("旧任务查询失败")).not.toBeInTheDocument();
  });

  test("hides retained loading state when a controlled task ID becomes empty", async () => {
    const api = apiFor(vi.fn().mockReturnValue(new Promise<DeleteTaskStatus>(() => undefined)));
    const view = render(<DeleteStatusPanel taskId="task-1" api={api} />);
    expect(await screen.findByRole("status")).toHaveTextContent("正在加载");

    view.rerender(<DeleteStatusPanel taskId="" api={api} />);

    expect(screen.getByRole("alert")).toHaveTextContent("缺少删除任务 ID");
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  test("does not poll an already complete task and supports explicit lookup", async () => {
    vi.useFakeTimers();
    vi.spyOn(document, "hasFocus").mockReturnValue(true);
    const complete = { ...status, complete: true, pending: 0 };
    const getDeleteStatus = vi.fn().mockResolvedValue(complete);
    render(<DeleteStatusPanel api={apiFor(getDeleteStatus)} />);
    expect(getDeleteStatus).not.toHaveBeenCalled();
    fireEvent.change(screen.getByLabelText("删除任务 ID"), { target: { value: "task-1" } });
    fireEvent.click(screen.getByRole("button", { name: "查询" }));
    await act(async () => {});
    expect(getDeleteStatus).toHaveBeenCalledTimes(1);
    await act(async () => { await vi.advanceTimersByTimeAsync(12_000); });
    expect(getDeleteStatus).toHaveBeenCalledTimes(1);
  });

  test("keeps invalid and not-found errors explicit", async () => {
    const api = apiFor(vi.fn().mockRejectedValue(new Error("delete task not found")));
    const user = userEvent.setup();
    render(<DeleteStatusPanel taskId="not-a-task" api={api} />);
    expect(await screen.findByRole("alert")).toHaveTextContent("delete task not found");
    await user.click(screen.getByRole("button", { name: "重试" }));
    expect(api.getDeleteStatus).toHaveBeenCalled();
  });

  test("controlled mode keeps the lookup form prefilled and can query another task", async () => {
    const getDeleteStatus = vi.fn()
      .mockResolvedValueOnce(status)
      .mockResolvedValue({ ...status, taskId: "task-other" });
    render(<DeleteStatusPanel taskId="task-1" api={apiFor(getDeleteStatus)} />);

    await screen.findByText("agent-a");
    expect(screen.getByLabelText("删除任务 ID")).toHaveValue("task-1");

    fireEvent.change(screen.getByLabelText("删除任务 ID"), { target: { value: "task-other" } });
    fireEvent.click(screen.getByRole("button", { name: "查询" }));

    await waitFor(() => expect(getDeleteStatus).toHaveBeenLastCalledWith("task-other", expect.any(AbortSignal)));
    expect(await screen.findByText("agent-a")).toBeInTheDocument();
  });

  test("a changed controlled task ID repopulates the form and polls the new task", async () => {
    const getDeleteStatus = vi.fn().mockResolvedValue(status);
    const view = render(<DeleteStatusPanel taskId="task-1" api={apiFor(getDeleteStatus)} />);
    await screen.findByText("agent-a");

    fireEvent.change(screen.getByLabelText("删除任务 ID"), { target: { value: "task-other" } });
    fireEvent.click(screen.getByRole("button", { name: "查询" }));
    await waitFor(() => expect(getDeleteStatus).toHaveBeenLastCalledWith("task-other", expect.any(AbortSignal)));

    view.rerender(<DeleteStatusPanel taskId="task-2" api={apiFor(getDeleteStatus)} />);

    expect(screen.getByLabelText("删除任务 ID")).toHaveValue("task-2");
    await waitFor(() => expect(getDeleteStatus).toHaveBeenLastCalledWith("task-2", expect.any(AbortSignal)));
  });

  test("copies the active task ID and problem details", async () => {
    const user = userEvent.setup();
    // userEvent.setup 会安装自己的剪贴板桩，须在其后覆盖
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
    render(<DeleteStatusPanel taskId="task-1" api={apiFor()} />);

    await screen.findByText("agent-a");
    await user.click(screen.getByRole("button", { name: "复制" }));
    expect(writeText).toHaveBeenCalledWith("task-1");
    expect(await screen.findByRole("button", { name: "已复制" })).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "复制路径" }));
    expect(writeText).toHaveBeenCalledWith("D:\\<img src=x>");
    await user.click(screen.getByRole("button", { name: "复制错误" }));
    expect(writeText).toHaveBeenCalledWith("<b>bad</b>");
  });

  test("lists delete tasks when no task is selected and loads one on click", async () => {
    const getDeleteStatus = vi.fn().mockResolvedValue({ ...status, taskId: runningSummary.taskId });
    const listDeleteTasks = vi.fn().mockResolvedValue([runningSummary, doneSummary]);
    render(<DeleteStatusPanel api={apiFor(getDeleteStatus, listDeleteTasks)} />);

    const rows = await screen.findAllByTestId("delete-task-row");
    expect(rows.map(row => row.textContent)).toEqual([
      expect.stringContaining("进行中"),
      expect.stringContaining("已完成")
    ]);
    expect(rows[0]).toHaveTextContent("11111111…");
    expect(rows[0]).toHaveTextContent("1/4");
    expect(rows[0]).toHaveTextContent("2026-08-14T01:02:03Z");
    expect(rows[1]).toHaveTextContent("task-done-2");
    expect(rows[1]).toHaveTextContent("2/2");
    expect(within(rows[0]).getByRole("button", { name: "复制任务 ID" })).toBeInTheDocument();

    fireEvent.click(within(rows[0]).getByRole("button", { name: "查看" }));

    await waitFor(() => expect(getDeleteStatus)
      .toHaveBeenCalledWith(runningSummary.taskId, expect.any(AbortSignal)));
    expect(screen.getByLabelText("删除任务 ID")).toHaveValue(runningSummary.taskId);
    expect(screen.queryByLabelText("删除任务列表")).not.toBeInTheDocument();
    expect(await screen.findByText("agent-a")).toBeInTheDocument();
  });

  test("shows an empty notice when there are no delete tasks", async () => {
    render(<DeleteStatusPanel api={apiFor()} />);

    expect(await screen.findByText("暂无删除任务。")).toBeInTheDocument();
  });

  test("refreshes the task list on demand even when all tasks are terminal", async () => {
    const listDeleteTasks = vi.fn().mockResolvedValue([doneSummary]);
    const user = userEvent.setup();
    render(<DeleteStatusPanel api={apiFor(vi.fn(), listDeleteTasks)} />);

    expect(await screen.findAllByTestId("delete-task-row")).toHaveLength(1);
    expect(listDeleteTasks).toHaveBeenCalledTimes(1);
    await user.click(screen.getByRole("button", { name: "刷新" }));
    await waitFor(() => expect(listDeleteTasks).toHaveBeenCalledTimes(2));
  });

  test("keeps polling the list while a task runs and stops once all are terminal", async () => {
    vi.useFakeTimers();
    vi.spyOn(document, "hasFocus").mockReturnValue(true);
    const listDeleteTasks = vi.fn()
      .mockResolvedValueOnce([runningSummary])
      .mockResolvedValue([doneSummary]);
    render(<DeleteStatusPanel api={apiFor(vi.fn(), listDeleteTasks)} />);

    await act(async () => {});
    expect(listDeleteTasks).toHaveBeenCalledTimes(1);
    await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });
    expect(listDeleteTasks).toHaveBeenCalledTimes(2);
    await act(async () => { await vi.advanceTimersByTimeAsync(12_000); });
    expect(listDeleteTasks).toHaveBeenCalledTimes(2);
  });

  test("shows the list error with an explicit retry", async () => {
    const listDeleteTasks = vi.fn().mockRejectedValue(new Error("delete service unavailable"));
    const user = userEvent.setup();
    render(<DeleteStatusPanel api={apiFor(vi.fn(), listDeleteTasks)} />);

    expect(await screen.findByRole("alert")).toHaveTextContent("delete service unavailable");
    await user.click(screen.getByRole("button", { name: "重试" }));
    await waitFor(() => expect(listDeleteTasks.mock.calls.length).toBeGreaterThanOrEqual(2));
  });

  test("shows soft-delete recycle destinations sorted by source path with copy support", async () => {
    const recycled: DeleteTaskStatus = {
      ...status,
      byMachine: {
        "agent-a": {
          machineId: "agent-a", total: 2, ok: 2, failed: 0, uncertain: 0, pending: 0,
          complete: true, stateSyncFailures: 0, sequences: {},
          recycledTo: {
            "D:\\dupes\\b.flac": "Z:\\recycled\\b.flac",
            "D:\\dupes\\a.flac": "Z:\\recycled\\a.flac"
          }
        }
      }
    };
    const user = userEvent.setup();
    // userEvent.setup 会安装自己的剪贴板桩，须在其后覆盖
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
    render(<DeleteStatusPanel taskId="task-1" api={apiFor(vi.fn().mockResolvedValue(recycled))} />);

    const region = await screen.findByLabelText("Agent agent-a 已移入回收目录");
    expect(region).toHaveTextContent("已移入回收目录：2 项");
    const items = within(region).getAllByRole("listitem");
    expect(items[0]).toHaveTextContent("D:\\dupes\\a.flac → Z:\\recycled\\a.flac");
    expect(items[1]).toHaveTextContent("D:\\dupes\\b.flac → Z:\\recycled\\b.flac");
    await user.click(within(items[0]).getByRole("button", { name: "复制去向" }));
    expect(writeText).toHaveBeenCalledWith("Z:\\recycled\\a.flac");
  });

  test("collapses a long recycle list behind an expand toggle", async () => {
    const recycledTo = Object.fromEntries(Array.from({ length: 5 },
      (_, index) => [`D:\\dupes\\f${index}.flac`, `Z:\\recycled\\f${index}.flac`]));
    const recycled: DeleteTaskStatus = {
      ...status,
      byMachine: {
        "agent-a": {
          machineId: "agent-a", total: 5, ok: 5, failed: 0, uncertain: 0, pending: 0,
          complete: true, stateSyncFailures: 0, sequences: {}, recycledTo
        }
      }
    };
    const user = userEvent.setup();
    render(<DeleteStatusPanel taskId="task-1" api={apiFor(vi.fn().mockResolvedValue(recycled))} />);

    const region = await screen.findByLabelText("Agent agent-a 已移入回收目录");
    expect(within(region).getAllByRole("listitem")).toHaveLength(3);
    await user.click(within(region).getByRole("button", { name: "展开全部 5 条" }));
    expect(within(region).getAllByRole("listitem")).toHaveLength(5);
    await user.click(within(region).getByRole("button", { name: "收起回收去向" }));
    expect(within(region).getAllByRole("listitem")).toHaveLength(3);
  });

  test("omits the recycle section when nothing was recycled", async () => {
    render(<DeleteStatusPanel taskId="task-1" api={apiFor()} />);

    await screen.findByText("agent-a");
    expect(screen.queryByLabelText(/已移入回收目录/)).not.toBeInTheDocument();
  });

  function reviewSnapshot(terminalStatus: DeleteTaskStatus): DeleteReviewSnapshot {
    return {
      groupId: 1,
      kind: "exact",
      scopeKey: "exact:1",
      members: [{ fileId: 7, machineId: "agent-b", path: "D:\\<img src=x>" }],
      terminalStatus,
      reconciled: true
    };
  }

  test("offers one-click retry for helper-lost items and hands the derived file IDs back", async () => {
    const onRetryRequest = vi.fn();
    const user = userEvent.setup();
    render(<DeleteStatusPanel
      api={apiFor()}
      deleteReviewSnapshot={reviewSnapshot({ ...status, complete: true })}
      onRetryRequest={onRetryRequest}
      taskId="task-1"
    />);

    const region = await screen.findByRole("region", { name: "一键重试" });
    const button = within(region).getByRole("button", { name: "重试这些项" });
    expect(button).toBeEnabled();
    await user.click(button);
    // 问题项目按 machineId+path 映射回快照成员 fileId
    expect(onRetryRequest).toHaveBeenCalledWith([7]);
  });

  test("disables one-click retry with an explicit hint when no review snapshot exists", async () => {
    render(<DeleteStatusPanel api={apiFor()} taskId="task-1" />);

    const region = await screen.findByRole("region", { name: "一键重试" });
    expect(within(region).getByRole("button", { name: "重试这些项" })).toBeDisabled();
    expect(within(region).getByRole("status")).toHaveTextContent("缺少可核对的原始选择，无法一键重试。");
  });

  test("disables one-click retry when the snapshot belongs to a different task", async () => {
    render(<DeleteStatusPanel
      api={apiFor()}
      deleteReviewSnapshot={reviewSnapshot({ ...status, taskId: "task-other", complete: true })}
      onRetryRequest={vi.fn()}
      taskId="task-1"
    />);

    const region = await screen.findByRole("region", { name: "一键重试" });
    expect(within(region).getByRole("button", { name: "重试这些项" })).toBeDisabled();
    expect(within(region).getByRole("status")).toHaveTextContent("缺少可核对的原始选择，无法一键重试。");
  });

  test("hides one-click retry when nothing is uncertain or helper-lost", async () => {
    const clean: DeleteTaskStatus = { ...status, uncertain: 0, errorCodes: {}, problems: [] };
    render(<DeleteStatusPanel api={apiFor(vi.fn().mockResolvedValue(clean))} taskId="task-1" />);

    await screen.findByText("agent-a");
    expect(screen.queryByRole("region", { name: "一键重试" })).not.toBeInTheDocument();
  });
});
