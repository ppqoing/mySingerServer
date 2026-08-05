import { act, fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, test, vi } from "vitest";
import type { AppApi, DeleteTaskStatus } from "../../api/contracts";
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

function apiFor(getDeleteStatus = vi.fn().mockResolvedValue(status)): AppApi {
  return { getDeleteStatus } as unknown as AppApi;
}

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
});
