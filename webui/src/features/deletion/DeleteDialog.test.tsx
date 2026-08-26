import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { StrictMode, useState } from "react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { ApiError } from "../../api/client";
import type {
  AgentStatus,
  AppApi,
  DeletePreparation,
  DeleteTaskStatus,
  GroupDetail,
  GroupPage,
  GroupQuery
} from "../../api/contracts";
import { GroupsPage } from "../groups/GroupsPage";
import { DeleteDialog } from "./DeleteDialog";

const malicious = '<img data-testid="delete-xss" src=x onerror=alert(1)>';

function preparation(overrides: Partial<DeletePreparation> = {}): DeletePreparation {
  return {
    confirmToken: "token-a",
    expiresInSeconds: 60,
    summary: {
      totalFiles: 2,
      totalBytes: 3_072,
      byMachine: { "agent-z": 1, "agent-a": 1 },
      samples: ["D:\\safe\\one.jpg", "E:\\safe\\two.jpg"]
    },
    ...overrides
  };
}

function taskStatus(overrides: Partial<DeleteTaskStatus> = {}): DeleteTaskStatus {
  return {
    taskId: "task-a",
    mode: "soft",
    total: 2,
    ok: 0,
    failed: 0,
    uncertain: 0,
    pending: 2,
    complete: false,
    stateSyncFailures: 0,
    byMachine: {
      "agent-z": {
        machineId: "agent-z", total: 1, ok: 0, failed: 0, uncertain: 0,
        pending: 1, complete: false, stateSyncFailures: 0, sequences: {}
      },
      "agent-a": {
        machineId: "agent-a", total: 1, ok: 0, failed: 0, uncertain: 0,
        pending: 1, complete: false, stateSyncFailures: 0, sequences: {}
      }
    },
    errorCodes: {},
    problems: [],
    ...overrides
  };
}

function apiFor(overrides: Partial<AppApi> = {}): AppApi {
  return {
    prepareDelete: vi.fn().mockResolvedValue(preparation()),
    executeDelete: vi.fn().mockResolvedValue({ taskId: "task-a" }),
    getDeleteStatus: vi.fn().mockResolvedValue(taskStatus()),
    getGroupsStats: vi.fn().mockResolvedValue({ kind: "", groups: 0, totalBytes: 0, wastedBytes: 0 }),
    ...overrides
  } as unknown as AppApi;
}

function deferred<T>() {
  let resolve: ((value: T) => void) | undefined;
  let reject: ((reason?: unknown) => void) | undefined;
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return {
    promise,
    resolve: (value: T) => resolve?.(value),
    reject: (reason?: unknown) => reject?.(reason)
  };
}

function DialogHarness({
  api,
  memberIds = [7, 3, 7],
  onAccepted = vi.fn(),
  onTerminal = vi.fn()
}: {
  api: AppApi;
  memberIds?: number[];
  onAccepted?: (taskId: string) => void;
  onTerminal?: (status: DeleteTaskStatus) => void;
}) {
  const [open, setOpen] = useState(true);
  const [locked, setLocked] = useState(false);
  return <>
    <button disabled={locked} onClick={() => setOpen(false)} type="button">切换分组视图</button>
    <button onClick={() => setOpen(true)} type="button">重新打开删除</button>
    <DeleteDialog
      api={api}
      memberIds={memberIds}
      onAccepted={onAccepted}
      onClose={() => setOpen(false)}
      onExecutionLockChange={setLocked}
      onTerminal={onTerminal}
      open={open}
    />
  </>;
}

function TerminalSelectionHarness({
  api,
  onTerminal
}: {
  api: AppApi;
  onTerminal: (status: DeleteTaskStatus) => void;
}) {
  const [memberIds, setMemberIds] = useState([7, 3]);
  return <>
    <output aria-label="父级已选数量">{memberIds.length}</output>
    <DeleteDialog
      api={api}
      memberIds={memberIds}
      onClose={vi.fn()}
      onTerminal={status => {
        setMemberIds([]);
        onTerminal(status);
      }}
      open
    />
  </>;
}

function ExecutionPendingHarness({
  api,
  mounted
}: {
  api: AppApi;
  mounted: boolean;
}) {
  const [pending, setPending] = useState(false);
  return <>
    <output aria-label="删除执行受理状态">
      {pending ? "等待删除任务受理" : "可发起删除"}
    </output>
    {mounted
      ? <DeleteDialog
          api={api}
          memberIds={[7, 3]}
          onClose={vi.fn()}
          onExecutionPendingChange={setPending}
          onTerminal={vi.fn()}
          open
        />
      : null}
  </>;
}

async function flush() {
  await act(async () => {});
}

function setViewport(width: number) {
  Object.defineProperty(window, "matchMedia", {
    configurable: true,
    value: vi.fn().mockImplementation((query: string) => ({
      matches: (() => {
        const match = /max-width:\s*(\d+)px/.exec(query);
        return match ? width <= Number(match[1]) : false;
      })(),
      media: query,
      onchange: null,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      dispatchEvent: vi.fn()
    }))
  });
}

async function confirmHardDelete() {
  const user = userEvent.setup();
  await user.click(await screen.findByRole("radio", { name: "硬删除" }));
  await user.click(screen.getByRole("button", { name: "最终确认硬删除" }));
}

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

beforeEach(() => {
  setViewport(1920);
  Object.defineProperty(document, "visibilityState", { configurable: true, value: "visible" });
  vi.spyOn(document, "hasFocus").mockReturnValue(true);
  vi.spyOn(HTMLElement.prototype, "offsetHeight", "get").mockReturnValue(512);
  vi.spyOn(HTMLElement.prototype, "offsetWidth", "get").mockReturnValue(800);
});

describe("DeleteDialog", () => {
  test("opening snapshots sorted unique positive integers and prepares exactly once without executing", async () => {
    const api = apiFor();
    render(<DialogHarness api={api} memberIds={[9, -2, 3, 0, 9, 4.5]} />);

    await screen.findByRole("dialog", { name: "确认删除" });

    expect(api.prepareDelete).toHaveBeenCalledTimes(1);
    expect(api.prepareDelete).toHaveBeenCalledWith([3, 9], expect.any(AbortSignal));
    expect(api.executeDelete).not.toHaveBeenCalled();
  });

  test("an empty eligible selection fails closed without preparing", async () => {
    const api = apiFor();
    render(<DialogHarness api={api} memberIds={[]} />);

    expect(await screen.findByRole("alert")).toHaveTextContent("没有可删除的已选文件");
    expect(api.prepareDelete).not.toHaveBeenCalled();
    expect(api.executeDelete).not.toHaveBeenCalled();
  });

  test("shows the reconfirmed summary as text, defaults to soft delete, and uses the shared focus trap", async () => {
    const api = apiFor({
      prepareDelete: vi.fn().mockResolvedValue(preparation({
        summary: {
          totalFiles: 2,
          totalBytes: 3_072,
          byMachine: { "agent-z": 1, "agent-a": 1 },
          samples: [malicious, "D:\\safe\\two.jpg"]
        }
      }))
    });
    render(<DialogHarness api={api} />);

    await screen.findByRole("dialog", { name: "确认删除" });
    expect(screen.getByRole("radio", { name: "软删除" })).toBeChecked();
    expect(screen.getByText("2 个文件")).toBeInTheDocument();
    expect(screen.getByText("3,072 字节")).toBeInTheDocument();
    expect(screen.getByText("硬删除会永久删除文件且不可恢复。")).toBeInTheDocument();
    expect(screen.getByText(malicious)).toBeInTheDocument();
    expect(screen.getAllByTestId("delete-machine-summary").map(node => node.textContent)).toEqual([
      expect.stringContaining("agent-a"), expect.stringContaining("agent-z")
    ]);
    expect(document.querySelector("[data-testid='delete-xss']")).toBeNull();

    const first = screen.getByRole("button", { name: "关闭 确认删除" });
    const last = screen.getByRole("button", { name: "最终确认删除" });
    last.focus();
    expect(fireEvent.keyDown(last, { key: "Tab" })).toBe(false);
    expect(document.activeElement).toBe(first);
    fireEvent.keyDown(first, { key: "Tab", shiftKey: true });
    expect(document.activeElement).toBe(last);
  });

  test("hard delete shows an irreversible warning and uses an explicit danger action", async () => {
    const user = userEvent.setup();
    render(<DialogHarness api={apiFor()} />);

    await user.click(await screen.findByRole("radio", { name: "硬删除" }));

    expect(screen.getByRole("alert")).toHaveTextContent("硬删除将永久删除文件，无法恢复");
    expect(screen.getByRole("button", { name: "最终确认硬删除" })).toHaveClass("danger-action");
  });

  test("Cancel and Escape discard an unused token and require a fresh prepare before a later confirmation", async () => {
    const api = apiFor({
      prepareDelete: vi.fn()
        .mockResolvedValueOnce(preparation({ confirmToken: "cancelled-token" }))
        .mockResolvedValueOnce(preparation({ confirmToken: "escape-token" }))
        .mockResolvedValueOnce(preparation({ confirmToken: "fresh-token" }))
    });
    const user = userEvent.setup();
    render(<DialogHarness api={api} />);

    await screen.findByRole("dialog", { name: "确认删除" });
    await user.click(screen.getByRole("button", { name: "取消" }));
    expect(screen.queryByRole("dialog", { name: "确认删除" })).toBeNull();
    expect(api.executeDelete).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: "重新打开删除" }));
    const escapeDialog = await screen.findByRole("dialog", { name: "确认删除" });
    fireEvent.keyDown(escapeDialog, { key: "Escape" });
    expect(screen.queryByRole("dialog", { name: "确认删除" })).toBeNull();
    expect(api.executeDelete).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: "重新打开删除" }));
    await confirmHardDelete();
    expect(api.executeDelete).toHaveBeenCalledWith("fresh-token", "hard", expect.any(AbortSignal));
  });

  test("shows a retryable prepare failure without exposing a stale summary", async () => {
    const api = apiFor({
      prepareDelete: vi.fn()
        .mockRejectedValueOnce(new Error("准备服务暂时不可用"))
        .mockResolvedValueOnce(preparation({ confirmToken: "retried-token" }))
    });
    const user = userEvent.setup();
    render(<DialogHarness api={api} />);

    expect(await screen.findByRole("alert")).toHaveTextContent("准备服务暂时不可用");
    expect(screen.queryByRole("radio", { name: "软删除" })).toBeNull();
    await user.click(screen.getByRole("button", { name: "重新准备" }));

    expect(await screen.findByRole("radio", { name: "软删除" })).toBeChecked();
    expect(api.prepareDelete).toHaveBeenCalledTimes(2);
    expect(api.executeDelete).not.toHaveBeenCalled();
  });

  test("a scope change discards an unused preparation and prepares a new immutable ID snapshot", async () => {
    const api = apiFor({
      prepareDelete: vi.fn()
        .mockResolvedValueOnce(preparation({ confirmToken: "old-token" }))
        .mockResolvedValueOnce(preparation({ confirmToken: "new-token" }))
    });
    const user = userEvent.setup();
    const view = render(<DeleteDialog api={api} memberIds={[7, 3]} onClose={vi.fn()} onTerminal={vi.fn()} open />);

    await screen.findByRole("radio", { name: "软删除" });
    view.rerender(<DeleteDialog api={api} memberIds={[8, 2, 8]} onClose={vi.fn()} onTerminal={vi.fn()} open />);
    await waitFor(() => expect(api.prepareDelete).toHaveBeenLastCalledWith([2, 8], expect.any(AbortSignal)));
    await user.click(screen.getByRole("button", { name: "最终确认删除" }));

    expect(api.executeDelete).toHaveBeenCalledWith("new-token", "soft", expect.any(AbortSignal));
  });

  test("preparing keeps its modal mounted and locks conflicting dialog and view controls", async () => {
    const prepare = deferred<DeletePreparation>();
    const api = apiFor({ prepareDelete: vi.fn().mockReturnValue(prepare.promise) });
    render(<DialogHarness api={api} />);

    const dialog = await screen.findByRole("dialog", { name: "确认删除" });
    expect(screen.getByText("正在准备删除")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "切换分组视图", hidden: true })).toBeDisabled();
    expect(screen.getByRole("button", { name: "关闭 确认删除" })).toBeDisabled();
    fireEvent.keyDown(dialog, { key: "Escape" });
    fireEvent.click(screen.getByTestId("modal-backdrop"));
    expect(screen.getByRole("dialog", { name: "确认删除" })).toBeInTheDocument();

    await act(async () => prepare.resolve(preparation()));
    expect(await screen.findByRole("radio", { name: "软删除" })).toBeChecked();
  });

  test("an expiring confirmation is disabled, discarded, and can only continue after a fresh prepare", async () => {
    vi.useFakeTimers();
    const api = apiFor({
      prepareDelete: vi.fn()
        .mockResolvedValueOnce(preparation({ confirmToken: "expired-token", expiresInSeconds: 1 }))
        .mockResolvedValueOnce(preparation({ confirmToken: "replacement-token", expiresInSeconds: 60 }))
    });
    render(<DialogHarness api={api} />);

    await flush();
    screen.getByRole("button", { name: "最终确认删除" });
    await act(async () => { await vi.advanceTimersByTimeAsync(1_000); });
    expect(screen.getByRole("button", { name: "最终确认删除" })).toBeDisabled();
    expect(screen.getByRole("alert")).toHaveTextContent("确认已过期，请重新准备");
    expect(api.executeDelete).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "重新准备" }));
    await flush();
    expect(screen.getByRole("radio", { name: "软删除" })).toBeChecked();
    expect(api.prepareDelete).toHaveBeenCalledTimes(2);
  });

  test("an API token-expired execute rejection never retries execute and returns to explicit re-prepare", async () => {
    const api = apiFor({
      executeDelete: vi.fn().mockRejectedValue(new ApiError(400, "invalid confirmation", false))
    });
    const user = userEvent.setup();
    render(<DialogHarness api={api} />);

    await screen.findByRole("radio", { name: "软删除" });
    await user.click(screen.getByRole("button", { name: "最终确认删除" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("确认已过期，请重新准备");
    expect(api.executeDelete).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("button", { name: "最终确认删除" })).toBeNull();
  });

  test("a consumed-token execute rejection returns to explicit re-prepare instead of retrying the dead token", async () => {
    const api = apiFor({
      executeDelete: vi.fn().mockRejectedValue(new ApiError(409, "confirmation already used", false))
    });
    const user = userEvent.setup();
    render(<DialogHarness api={api} />);

    await screen.findByRole("radio", { name: "软删除" });
    await user.click(screen.getByRole("button", { name: "最终确认删除" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("该确认已被使用，请重新准备");
    expect(api.executeDelete).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("button", { name: "最终确认删除" })).toBeNull();
  });

  test("an expired execute with a newer live selection stays explicit until the user re-prepares", async () => {
    const execute = deferred<{ taskId: string }>();
    const prepareDelete = vi.fn()
      .mockResolvedValueOnce(preparation({ confirmToken: "expired-old-token" }))
      .mockResolvedValueOnce(preparation({ confirmToken: "fresh-live-token" }));
    const api = apiFor({
      prepareDelete,
      executeDelete: vi.fn().mockReturnValue(execute.promise)
    });
    const user = userEvent.setup();
    const view = render(
      <DeleteDialog api={api} memberIds={[7, 3]} onClose={vi.fn()} onTerminal={vi.fn()} open />
    );

    await user.click(await screen.findByRole("button", { name: "最终确认删除" }));
    view.rerender(
      <DeleteDialog api={api} memberIds={[8, 2]} onClose={vi.fn()} onTerminal={vi.fn()} open />
    );
    expect(prepareDelete).toHaveBeenCalledTimes(1);

    await act(async () => execute.reject(new ApiError(400, "invalid confirmation", false)));
    expect(await screen.findByRole("alert")).toHaveTextContent("确认已过期，请重新准备");
    expect(prepareDelete).toHaveBeenCalledTimes(1);
    await act(async () => {});
    expect(prepareDelete).toHaveBeenCalledTimes(1);

    await user.click(screen.getByRole("button", { name: "重新准备" }));
    await waitFor(() => {
      expect(prepareDelete).toHaveBeenLastCalledWith([2, 8], expect.any(AbortSignal));
    });
    expect(prepareDelete).toHaveBeenCalledTimes(2);
    expect(await screen.findByRole("radio", { name: "软删除" })).toBeChecked();
    expect(api.executeDelete).toHaveBeenCalledTimes(1);
  });

  test("a non-expiry execute failure stays explicit and never retries automatically", async () => {
    const api = apiFor({
      executeDelete: vi.fn().mockRejectedValue(new ApiError(503, "delete service unavailable", true))
    });
    const user = userEvent.setup();
    render(<DialogHarness api={api} />);

    await screen.findByRole("radio", { name: "软删除" });
    await user.click(screen.getByRole("button", { name: "最终确认删除" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("delete service unavailable");
    expect(api.executeDelete).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("button", { name: "最终确认删除" })).toBeEnabled();
    await act(async () => {});
    expect(api.executeDelete).toHaveBeenCalledTimes(1);
  });

  test("execute pending survives dialog unmount and clears only when the request rejects", async () => {
    const execute = deferred<{ taskId: string }>();
    const api = apiFor({ executeDelete: vi.fn().mockReturnValue(execute.promise) });
    const user = userEvent.setup();
    const view = render(<ExecutionPendingHarness api={api} mounted />);

    await user.click(await screen.findByRole("button", { name: "最终确认删除" }));
    expect(screen.getByRole("status", { name: "删除执行受理状态", hidden: true }))
      .toHaveTextContent("等待删除任务受理");

    view.rerender(<ExecutionPendingHarness api={api} mounted={false} />);
    expect(screen.getByRole("status", { name: "删除执行受理状态", hidden: true }))
      .toHaveTextContent("等待删除任务受理");

    await act(async () => execute.reject(new ApiError(503, "execute unavailable", true)));
    expect(screen.getByRole("status", { name: "删除执行受理状态", hidden: true }))
      .toHaveTextContent("可发起删除");
  });

  test("repeated confirmation locks the dialog and view controls until its single execute request is accepted", async () => {
    const execute = deferred<{ taskId: string }>();
    const api = apiFor({ executeDelete: vi.fn().mockReturnValue(execute.promise) });
    const onAccepted = vi.fn();
    const user = userEvent.setup();
    render(<DialogHarness api={api} onAccepted={onAccepted} />);

    const dialog = await screen.findByRole("dialog", { name: "确认删除" });
    const confirm = screen.getByRole("button", { name: "最终确认删除" });
    await user.click(confirm);
    await user.click(confirm);

    expect(api.executeDelete).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("button", { name: "取消" })).toBeDisabled();
    expect(screen.getByRole("radio", { name: "软删除" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "切换分组视图", hidden: true })).toBeDisabled();
    expect(screen.getByRole("button", { name: "关闭 确认删除" })).toBeDisabled();
    fireEvent.keyDown(dialog, { key: "Escape" });
    await user.click(screen.getByTestId("modal-backdrop"));
    await user.click(screen.getByRole("button", { name: "关闭 确认删除" }));
    expect(screen.getByRole("dialog", { name: "确认删除" })).toBeInTheDocument();

    await act(async () => execute.resolve({ taskId: "task-a" }));
    await waitFor(() => expect(onAccepted).toHaveBeenCalledTimes(1));
    expect(onAccepted).toHaveBeenLastCalledWith("task-a");
  });

  test("a standalone StrictMode dialog keeps a late accepted task after the effect replay", async () => {
    const execute = deferred<{ taskId: string }>();
    const status = deferred<DeleteTaskStatus>();
    const api = apiFor({
      executeDelete: vi.fn().mockReturnValue(execute.promise),
      getDeleteStatus: vi.fn().mockReturnValue(status.promise)
    });
    const user = userEvent.setup();
    render(
      <StrictMode>
        <DeleteDialog api={api} memberIds={[7, 3]} onClose={vi.fn()} onTerminal={vi.fn()} open />
      </StrictMode>
    );

    await user.click(await screen.findByRole("button", { name: "最终确认删除" }));
    await act(async () => execute.resolve({ taskId: "task-strict-late" }));

    expect(await screen.findByText("任务 ID：task-strict-late")).toBeInTheDocument();
    expect(api.getDeleteStatus).toHaveBeenCalledWith("task-strict-late", expect.any(AbortSignal));
    expect(api.executeDelete).toHaveBeenCalledTimes(1);
  });

  test("an asynchronous selection reconciliation cannot discard an execute request that may already be accepted", async () => {
    const execute = deferred<{ taskId: string }>();
    const status = deferred<DeleteTaskStatus>();
    let executeSignal: AbortSignal | undefined;
    const api = apiFor({
      executeDelete: vi.fn((_token: string, _mode: string, signal?: AbortSignal) => {
        executeSignal = signal;
        return execute.promise;
      }),
      getDeleteStatus: vi.fn().mockReturnValue(status.promise)
    });
    const onAccepted = vi.fn();
    const user = userEvent.setup();
    const view = render(
      <DeleteDialog
        api={api}
        memberIds={[7, 3]}
        onAccepted={onAccepted}
        onClose={vi.fn()}
        onTerminal={vi.fn()}
        open
      />
    );

    await screen.findByRole("radio", { name: "软删除" });
    await user.click(screen.getByRole("button", { name: "最终确认删除" }));
    view.rerender(
      <DeleteDialog
        api={api}
        memberIds={[8, 2]}
        onAccepted={onAccepted}
        onClose={vi.fn()}
        onTerminal={vi.fn()}
        open
      />
    );

    expect(executeSignal?.aborted).toBe(false);
    expect(api.prepareDelete).toHaveBeenCalledTimes(1);
    expect(api.executeDelete).toHaveBeenCalledTimes(1);

    await act(async () => execute.resolve({ taskId: "task-preserved" }));
    expect(await screen.findByText("任务 ID：task-preserved")).toBeInTheDocument();
    expect(api.getDeleteStatus).toHaveBeenCalledWith("task-preserved", expect.any(AbortSignal));
    expect(onAccepted).toHaveBeenCalledTimes(1);
    expect(onAccepted).toHaveBeenCalledWith("task-preserved");
    expect(api.prepareDelete).toHaveBeenCalledTimes(1);
    expect(api.executeDelete).toHaveBeenCalledTimes(1);
  });

  test("an execute rejection unfreezes the confirmation and re-prepares the latest reconciled selection", async () => {
    const firstExecute = deferred<{ taskId: string }>();
    const status = deferred<DeleteTaskStatus>();
    const api = apiFor({
      prepareDelete: vi.fn()
        .mockResolvedValueOnce(preparation({ confirmToken: "old-token" }))
        .mockResolvedValueOnce(preparation({ confirmToken: "latest-token" })),
      executeDelete: vi.fn()
        .mockReturnValueOnce(firstExecute.promise)
        .mockResolvedValueOnce({ taskId: "task-latest" }),
      getDeleteStatus: vi.fn().mockReturnValue(status.promise)
    });
    const user = userEvent.setup();
    const view = render(
      <DeleteDialog api={api} memberIds={[7, 3]} onClose={vi.fn()} onTerminal={vi.fn()} open />
    );

    await screen.findByRole("radio", { name: "软删除" });
    await user.click(screen.getByRole("button", { name: "最终确认删除" }));
    view.rerender(
      <DeleteDialog api={api} memberIds={[8, 2]} onClose={vi.fn()} onTerminal={vi.fn()} open />
    );
    expect(api.prepareDelete).toHaveBeenCalledTimes(1);

    await act(async () => firstExecute.reject(new ApiError(503, "execute unavailable", true)));
    await waitFor(() => {
      expect(api.prepareDelete).toHaveBeenLastCalledWith([2, 8], expect.any(AbortSignal));
    });
    expect(await screen.findByRole("radio", { name: "软删除" })).toBeChecked();
    expect(api.prepareDelete).toHaveBeenCalledTimes(2);
    expect(api.executeDelete).toHaveBeenCalledTimes(1);

    await user.click(screen.getByRole("button", { name: "最终确认删除" }));
    expect(await screen.findByText("任务 ID：task-latest")).toBeInTheDocument();
    expect(api.executeDelete).toHaveBeenNthCalledWith(1, "old-token", "soft", expect.any(AbortSignal));
    expect(api.executeDelete).toHaveBeenNthCalledWith(2, "latest-token", "soft", expect.any(AbortSignal));
  });

  test("polls an accepted task no faster than one second, renders all progress fields literally, and completes once", async () => {
    vi.useFakeTimers();
    const inProgress = taskStatus({
      taskId: "task-a",
      mode: "soft",
      total: 2,
      ok: 1,
      failed: 0,
      uncertain: 0,
      pending: 1,
      complete: false,
      stateSyncFailures: 1,
      errorCodes: { [malicious]: 1 },
      problems: [{
        machineId: "agent-z", sequence: 5, path: malicious, errorCode: malicious,
        errorMessage: malicious, uncertain: true, stateSyncErr: malicious
      }]
    });
    inProgress.byMachine["agent-z"].sequences = {
      "10": {
        sequence: 10, lastSeq: 11, received: false,
        total: 1, ok: 0, failed: 1, uncertain: 1
      },
      "2": {
        sequence: 2, lastSeq: 11, received: true,
        total: 3, ok: 2, failed: 1, uncertain: 0
      }
    };
    const terminal = taskStatus({ ...inProgress, pending: 0, complete: true, ok: 2 });
    const api = apiFor({
      getDeleteStatus: vi.fn()
        .mockResolvedValueOnce(inProgress)
        .mockResolvedValueOnce(terminal)
    });
    const onTerminal = vi.fn();
    render(<DialogHarness api={api} onTerminal={onTerminal} />);

    await flush();
    screen.getByRole("radio", { name: "软删除" });
    fireEvent.click(screen.getByRole("button", { name: "最终确认删除" }));
    await flush();
    expect(api.getDeleteStatus).toHaveBeenCalledTimes(1);
    expect(screen.getByText("任务 ID：task-a")).toBeInTheDocument();
    expect(screen.getByText("模式：soft")).toBeInTheDocument();
    expect(screen.getByText("总数：2")).toBeInTheDocument();
    expect(screen.getByText("成功：1")).toBeInTheDocument();
    expect(screen.getByText("待处理：1")).toBeInTheDocument();
    expect(screen.getAllByTestId("delete-sequence-status").map(node => node.textContent)).toEqual([
      "序列：2；最后序列：11；已接收：是；总数：3；成功：2；失败：1；不确定：0",
      "序列：10；最后序列：11；已接收：否；总数：1；成功：0；失败：1；不确定：1"
    ]);
    expect(screen.getByRole("region", { name: "按 Agent 删除进度" })).toHaveTextContent("agent-z");
    expect(screen.getByRole("region", { name: "删除错误代码" })).toHaveTextContent(malicious);
    expect(screen.getByRole("region", { name: "删除问题项目" })).toHaveTextContent(malicious);
    expect(document.querySelector("[data-testid='delete-xss']")).toBeNull();

    await act(async () => { await vi.advanceTimersByTimeAsync(999); });
    expect(api.getDeleteStatus).toHaveBeenCalledTimes(1);
    await act(async () => { await vi.advanceTimersByTimeAsync(1); });
    await flush();
    expect(onTerminal).toHaveBeenCalledWith(terminal);
    expect(onTerminal).toHaveBeenCalledTimes(1);
    await act(async () => { await vi.advanceTimersByTimeAsync(5_000); });
    expect(api.getDeleteStatus).toHaveBeenCalledTimes(2);
  });

  test("a polling failure retains the accepted task and retry only requests that same task", async () => {
    const complete = taskStatus({ complete: true, pending: 0, ok: 2 });
    const api = apiFor({
      getDeleteStatus: vi.fn()
        .mockRejectedValueOnce(new Error("状态同步暂不可用"))
        .mockResolvedValueOnce(complete)
    });
    const onAccepted = vi.fn();
    const user = userEvent.setup();
    render(<DialogHarness api={api} onAccepted={onAccepted} />);

    await screen.findByRole("radio", { name: "软删除" });
    await user.click(screen.getByRole("button", { name: "最终确认删除" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("状态同步暂不可用");
    expect(screen.getByText("任务 ID：task-a")).toBeInTheDocument();
    expect(onAccepted).toHaveBeenCalledWith("task-a");

    await user.click(screen.getByRole("button", { name: "重试获取进度" }));
    await waitFor(() => expect(api.getDeleteStatus).toHaveBeenLastCalledWith("task-a", expect.any(AbortSignal)));
    expect(api.prepareDelete).toHaveBeenCalledTimes(1);
    expect(api.executeDelete).toHaveBeenCalledTimes(1);
  });

  test("restores an accepted task directly without preparing, executing, or re-notifying acceptance", async () => {
    const pending = deferred<DeleteTaskStatus>();
    const api = apiFor({ getDeleteStatus: vi.fn().mockReturnValue(pending.promise) });
    const onAccepted = vi.fn();
    render(
      <DeleteDialog
        api={api}
        initialTaskId="task-restored"
        memberIds={[7, 3]}
        onAccepted={onAccepted}
        onClose={vi.fn()}
        onTerminal={vi.fn()}
        open
      />
    );

    await flush();
    expect(screen.getByText("任务 ID：task-restored")).toBeInTheDocument();
    expect(api.getDeleteStatus).toHaveBeenCalledWith("task-restored", expect.any(AbortSignal));
    expect(api.prepareDelete).not.toHaveBeenCalled();
    expect(api.executeDelete).not.toHaveBeenCalled();
    expect(onAccepted).not.toHaveBeenCalled();
  });

  test("a late initial task takes over an already-open confirmation without preparing or executing again", async () => {
    const pending = deferred<DeleteTaskStatus>();
    const api = apiFor({ getDeleteStatus: vi.fn().mockReturnValue(pending.promise) });
    const view = render(
      <DeleteDialog
        api={api}
        memberIds={[7, 3]}
        onClose={vi.fn()}
        onTerminal={vi.fn()}
        open
      />
    );

    await screen.findByRole("radio", { name: "软删除" });
    view.rerender(
      <DeleteDialog
        api={api}
        initialTaskId="task-late-authoritative"
        memberIds={[7, 3]}
        onClose={vi.fn()}
        onTerminal={vi.fn()}
        open
      />
    );

    expect(await screen.findByText("任务 ID：task-late-authoritative")).toBeInTheDocument();
    expect(api.getDeleteStatus).toHaveBeenCalledWith("task-late-authoritative", expect.any(AbortSignal));
    expect(api.prepareDelete).toHaveBeenCalledTimes(1);
    expect(api.executeDelete).not.toHaveBeenCalled();
  });

  test("a late status response from an unmounted task is aborted and cannot overwrite a newer confirmation", async () => {
    const oldStatus = deferred<DeleteTaskStatus>();
    let oldSignal: AbortSignal | undefined;
    const oldApi = apiFor({
      getDeleteStatus: vi.fn((_taskId: string, signal?: AbortSignal) => {
        oldSignal = signal;
        return oldStatus.promise;
      })
    });
    const newApi = apiFor({ prepareDelete: vi.fn().mockResolvedValue(preparation({ confirmToken: "new-token" })) });
    const onTerminal = vi.fn();
    const user = userEvent.setup();
    const view = render(<DeleteDialog api={oldApi} memberIds={[2]} onClose={vi.fn()} onTerminal={onTerminal} open />);

    await screen.findByRole("radio", { name: "软删除" });
    await user.click(screen.getByRole("button", { name: "最终确认删除" }));
    await flush();
    expect(oldApi.getDeleteStatus).toHaveBeenCalledTimes(1);

    view.rerender(<DeleteDialog api={newApi} key="new-dialog" memberIds={[8]} onClose={vi.fn()} onTerminal={onTerminal} open />);
    await screen.findByRole("radio", { name: "软删除" });
    expect(oldSignal?.aborted).toBe(true);
    await act(async () => oldStatus.resolve(taskStatus({ complete: true, pending: 0 })));

    expect(onTerminal).not.toHaveBeenCalled();
    expect(newApi.prepareDelete).toHaveBeenCalledWith([8], expect.any(AbortSignal));
    expect(screen.getByText("确认令牌剩余 60 秒")).toBeInTheDocument();
  });

  test("clearing the parent selection on terminal keeps the accepted task mounted and notifies once", async () => {
    const terminal = taskStatus({ complete: true, pending: 0, ok: 2 });
    const api = apiFor({ getDeleteStatus: vi.fn().mockResolvedValue(terminal) });
    const onTerminal = vi.fn();
    const user = userEvent.setup();
    render(<TerminalSelectionHarness api={api} onTerminal={onTerminal} />);

    await user.click(await screen.findByRole("button", { name: "最终确认删除" }));
    await waitFor(() => expect(onTerminal).toHaveBeenCalledWith(terminal));

    expect(screen.getByRole("status", { name: "父级已选数量", hidden: true })).toHaveTextContent("0");
    expect(screen.getByText("任务 ID：task-a")).toBeInTheDocument();
    expect(api.prepareDelete).toHaveBeenCalledTimes(1);
    expect(api.getDeleteStatus).toHaveBeenCalledTimes(1);
    await flush();
    expect(onTerminal).toHaveBeenCalledTimes(1);
  });

  test("a prepare selection conflict shows Chinese guidance instead of the raw English error", async () => {
    const api = apiFor({
      prepareDelete: vi.fn().mockRejectedValue(new ApiError(409, "delete selection conflict", false))
    });
    render(<DialogHarness api={api} />);

    expect(await screen.findByRole("alert"))
      .toHaveTextContent("选择冲突：部分文件已在其他删除任务中，请调整选择后重试。");
    expect(screen.queryByText(/delete selection conflict/)).toBeNull();
  });

  test("renders delete error codes with Chinese labels", async () => {
    const api = apiFor({
      getDeleteStatus: vi.fn().mockResolvedValue(taskStatus({
        errorCodes: { E_NOT_FOUND: 2, E_IN_USE: 1, E_UNKNOWN_XYZ: 3 }
      }))
    });
    render(
      <DeleteDialog api={api} initialTaskId="task-a" memberIds={[7, 3]} onClose={vi.fn()} onTerminal={vi.fn()} open />
    );

    expect(await screen.findByText("E_NOT_FOUND（文件不存在）：2")).toBeInTheDocument();
    expect(screen.getByText("E_IN_USE（文件正在使用）：1")).toBeInTheDocument();
    expect(screen.getByText("E_UNKNOWN_XYZ：3")).toBeInTheDocument();
  });

  test("shows recycle destinations for soft-deleted machines", async () => {
    const api = apiFor({
      getDeleteStatus: vi.fn().mockResolvedValue(taskStatus({
        complete: true,
        pending: 0,
        ok: 2,
        byMachine: {
          "agent-a": {
            machineId: "agent-a", total: 1, ok: 1, failed: 0, uncertain: 0,
            pending: 0, complete: true, stateSyncFailures: 0, sequences: {},
            recycledTo: { "D:\\dupes\\one.jpg": "Z:\\recycled\\one.jpg" }
          }
        }
      }))
    });
    render(
      <DeleteDialog api={api} initialTaskId="task-a" memberIds={[7, 3]} onClose={vi.fn()} onTerminal={vi.fn()} open />
    );

    const region = await screen.findByLabelText("Agent agent-a 已移入回收目录");
    expect(region).toHaveTextContent("已移入回收目录：1 项");
    expect(region).toHaveTextContent("D:\\dupes\\one.jpg");
    expect(region).toHaveTextContent("Z:\\recycled\\one.jpg");
    expect(within(region).getByRole("button", { name: "复制去向" })).toBeInTheDocument();
  });

  test("omits recycle destinations when the task recycled nothing", async () => {
    const api = apiFor({
      getDeleteStatus: vi.fn().mockResolvedValue(taskStatus({ complete: true, pending: 0, ok: 2 }))
    });
    render(
      <DeleteDialog api={api} initialTaskId="task-a" memberIds={[7, 3]} onClose={vi.fn()} onTerminal={vi.fn()} open />
    );

    await screen.findByText("状态：已完成");
    expect(screen.queryByLabelText(/已移入回收目录/)).not.toBeInTheDocument();
  });

  test("retryable polling failures back off with growing delays and recover without manual retry", async () => {
    vi.useFakeTimers();
    const inProgress = taskStatus({ pending: 1 });
    const terminal = taskStatus({ complete: true, pending: 0, ok: 2 });
    const api = apiFor({
      getDeleteStatus: vi.fn()
        .mockResolvedValueOnce(inProgress)
        .mockRejectedValueOnce(new ApiError(503, "delete service unavailable", true))
        .mockRejectedValueOnce(new ApiError(0, "网络请求失败", true))
        .mockResolvedValueOnce(inProgress)
        .mockResolvedValueOnce(terminal)
    });
    const onTerminal = vi.fn();
    render(
      <DeleteDialog api={api} initialTaskId="task-a" memberIds={[7, 3]} onClose={vi.fn()} onTerminal={onTerminal} open />
    );

    await flush();
    expect(api.getDeleteStatus).toHaveBeenCalledTimes(1);

    // t=1s：第一次失败（可重试）→ 1s 退避，不进入手动重试态
    await act(async () => { await vi.advanceTimersByTimeAsync(1_000); });
    expect(api.getDeleteStatus).toHaveBeenCalledTimes(2);
    expect(screen.getByText(/自动重试/)).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();

    // t=2s：第二次失败 → 2s 退避
    await act(async () => { await vi.advanceTimersByTimeAsync(999); });
    expect(api.getDeleteStatus).toHaveBeenCalledTimes(2);
    await act(async () => { await vi.advanceTimersByTimeAsync(1); });
    expect(api.getDeleteStatus).toHaveBeenCalledTimes(3);

    // t=4s：恢复成功，退避提示消失，回到 1s 轮询并在 t=5s 到达终态
    await act(async () => { await vi.advanceTimersByTimeAsync(1_999); });
    expect(api.getDeleteStatus).toHaveBeenCalledTimes(3);
    await act(async () => { await vi.advanceTimersByTimeAsync(1); });
    expect(api.getDeleteStatus).toHaveBeenCalledTimes(4);
    expect(screen.queryByText(/自动重试/)).toBeNull();
    await act(async () => { await vi.advanceTimersByTimeAsync(1_000); });
    expect(onTerminal).toHaveBeenCalledWith(terminal);
  });

  test("sustained retryable polling failures stop into manual retry after capped backoff", async () => {
    vi.useFakeTimers();
    const terminal = taskStatus({ complete: true, pending: 0, ok: 2 });
    const api = apiFor({
      getDeleteStatus: vi.fn()
        .mockRejectedValueOnce(new ApiError(503, "delete service unavailable", true))
        .mockRejectedValueOnce(new ApiError(503, "delete service unavailable", true))
        .mockRejectedValueOnce(new ApiError(503, "delete service unavailable", true))
        .mockRejectedValueOnce(new ApiError(503, "delete service unavailable", true))
        .mockRejectedValueOnce(new ApiError(503, "delete service unavailable", true))
        .mockResolvedValueOnce(terminal)
    });
    const onTerminal = vi.fn();
    render(
      <DeleteDialog api={api} initialTaskId="task-a" memberIds={[7, 3]} onClose={vi.fn()} onTerminal={onTerminal} open />
    );

    // 退避节奏 1s→2s→4s→8s，第 5 次连续失败才进入手动重试态
    await flush();
    expect(api.getDeleteStatus).toHaveBeenCalledTimes(1);
    await act(async () => { await vi.advanceTimersByTimeAsync(1_000); });
    await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });
    await act(async () => { await vi.advanceTimersByTimeAsync(4_000); });
    expect(api.getDeleteStatus).toHaveBeenCalledTimes(4);
    expect(screen.queryByRole("alert")).toBeNull();
    await act(async () => { await vi.advanceTimersByTimeAsync(8_000); });
    expect(api.getDeleteStatus).toHaveBeenCalledTimes(5);
    expect(screen.getByRole("alert")).toHaveTextContent("delete service unavailable");
    await act(async () => { await vi.advanceTimersByTimeAsync(30_000); });
    expect(api.getDeleteStatus).toHaveBeenCalledTimes(5);

    fireEvent.click(screen.getByRole("button", { name: "重试获取进度" }));
    await flush();
    expect(onTerminal).toHaveBeenCalledWith(terminal);
  });

  test("a non-retryable polling failure stops immediately with a mapped Chinese message", async () => {
    vi.useFakeTimers();
    const api = apiFor({
      getDeleteStatus: vi.fn().mockRejectedValue(new ApiError(404, "delete task not found", false))
    });
    render(
      <DeleteDialog api={api} initialTaskId="task-gone" memberIds={[7, 3]} onClose={vi.fn()} onTerminal={vi.fn()} open />
    );

    await flush();
    expect(screen.getByRole("alert")).toHaveTextContent("删除任务不存在或已随 Manager 重启清除。");
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(api.getDeleteStatus).toHaveBeenCalledTimes(1);
  });

  test("the terminal view links to the audit page and copies the task ID", async () => {
    const terminal = taskStatus({ complete: true, pending: 0, ok: 2 });
    const api = apiFor({ getDeleteStatus: vi.fn().mockResolvedValue(terminal) });
    const user = userEvent.setup();
    // userEvent.setup 会安装自己的剪贴板桩，须在其后覆盖
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
    render(
      <DeleteDialog api={api} initialTaskId="task-a" memberIds={[7, 3]} onClose={vi.fn()} onTerminal={vi.fn()} open />
    );

    const link = await screen.findByRole("link", { name: "前往删除审计页查看 →" });
    expect(link).toHaveAttribute("href", "#/audit?task=task-a");

    await user.click(screen.getByRole("button", { name: "复制" }));
    expect(writeText).toHaveBeenCalledWith("task-a");
    expect(await screen.findByRole("button", { name: "已复制" })).toBeInTheDocument();
  });
});

function groupPage(query: GroupQuery): GroupPage {
  return {
    kind: query.kind,
    page: query.page,
    size: query.size,
    total: 1,
    groups: [{
      id: 1,
      kind: "exact",
      memberCount: 3,
      repMachine: "agent-a",
      repPath: "D:\\duplicates\\representative.jpg",
      machines: ["agent-a", "agent-b"],
      createdAt: "2026-07-31T09:00:00Z",
      totalBytes: 3_000,
      wastedBytes: 2_000
    }]
  };
}

function groupDetail(representativeFileId: number): GroupDetail {
  return {
    id: 1,
    kind: "exact",
    representativeFileId,
    memberTotal: 3,
    memberPage: 1,
    memberSize: 100,
    members: [
      { fileId: 1, machineId: "agent-a", path: "D:\\duplicates\\one.jpg", size: 1_000, mtime: 1, score: 1 },
      { fileId: 2, machineId: "agent-b", path: "D:\\duplicates\\two.jpg", size: 1_000, mtime: 1, score: 1 },
      { fileId: 3, machineId: "missing-agent", path: "D:\\duplicates\\three.jpg", size: 1_000, mtime: 1, score: 1 }
    ]
  };
}

describe("GroupsPage deletion handoff", () => {
  test("representative promotion and offline or unknown Agent reconciliation cannot leak an ineligible ID into prepare", async () => {
    const prepareDelete = vi.fn().mockResolvedValue(preparation());
    const getGroup = vi.fn()
      .mockResolvedValueOnce(groupDetail(1))
      .mockResolvedValueOnce(groupDetail(2));
    const agents: AgentStatus[] = [
      { machineId: "agent-a", addr: "10.0.0.1", online: true, identityState: "claimed" },
      { machineId: "agent-b", addr: "10.0.0.2", online: true, identityState: "claimed" }
    ];
    const api = apiFor({
      listAgents: vi.fn().mockResolvedValue(agents),
      listGroups: vi.fn(async (query: GroupQuery) => groupPage(query)),
      getGroup,
      prepareDelete
    });
    const user = userEvent.setup();
    render(<GroupsPage api={api} onRequestDelete={memberIds => void api.prepareDelete(memberIds)} />);

    await user.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    await user.click(await screen.findByRole("checkbox", { name: "选择文件 2" }));
    expect(screen.getByText(/^已选 1 项/)).toBeInTheDocument();
    expect(screen.getByRole("checkbox", { name: "选择文件 3" })).toBeDisabled();

    await user.click(screen.getByRole("button", { name: "刷新成员" }));
    await waitFor(() => expect(screen.getByRole("checkbox", { name: "选择文件 2" })).toBeDisabled());
    expect(screen.getByText(/^已选 0 项/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "删除已选 0 项" })).toBeDisabled();
    expect(prepareDelete).not.toHaveBeenCalled();
  });
});

describe("DeleteDialog kept files", () => {
  test("lists kept files with machine and path on the confirmation step", async () => {
    const api = apiFor();
    render(<DeleteDialog
      api={api}
      keptMembers={[
        { fileId: 1, machineId: "agent-a", path: "D:\\keep\\representative.jpg" },
        { fileId: 9, machineId: "agent-b", path: "E:\\keep\\unselected.jpg" }
      ]}
      memberIds={[3]}
      onClose={vi.fn()}
      onTerminal={vi.fn()}
      open
    />);

    const region = await screen.findByRole("region", { name: "本次保留的文件" });
    expect(within(region).getAllByTestId("kept-member").map(item => item.textContent)).toEqual([
      "agent-a：D:\\keep\\representative.jpg",
      "agent-b：E:\\keep\\unselected.jpg"
    ]);
  });

  test("states the kept-files fallback when the selection cannot enumerate them", async () => {
    const api = apiFor();
    render(<DeleteDialog api={api} memberIds={[3]} onClose={vi.fn()} onTerminal={vi.fn()} open />);

    const region = await screen.findByRole("region", { name: "本次保留的文件" });
    expect(region).toHaveTextContent("未选中的成员与各组代表文件将全部保留。");
  });
});
