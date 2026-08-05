import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { ApiError } from "../../api/client";
import type {
  AgentStatus,
  AppApi,
  DeletePreparation,
  DeleteTaskStatus,
  GroupDetail,
  GroupKind,
  GroupMember,
  GroupPage,
  GroupQuery,
  GroupSummary
} from "../../api/contracts";
import { deriveDeleteRetryPlan, type DeleteReviewSnapshot } from "./deleteReview";
import { GroupsPage } from "./GroupsPage";

function group(id: number, kind: GroupKind = "exact", overrides: Partial<GroupSummary> = {}): GroupSummary {
  return {
    id,
    kind,
    memberCount: 3,
    repMachine: "agent-a",
    repPath: `D:\\media\\duplicate-${id}.jpg`,
    machines: ["agent-a", "agent-b"],
    createdAt: "2026-07-31T09:00:00Z",
    totalBytes: 3_000,
    wastedBytes: 2_000,
    ...overrides
  };
}

function page(query: GroupQuery, overrides: Partial<GroupPage> = {}): GroupPage {
  return {
    kind: query.kind,
    page: query.page,
    size: query.size,
    total: 1,
    groups: [group(1, query.kind)],
    ...overrides
  };
}

function member(fileId: number, machineId = "agent-a", overrides: Partial<GroupMember> = {}): GroupMember {
  return {
    fileId,
    machineId,
    path: `D:\\media\\file-${fileId}.jpg`,
    size: 1_000,
    mtime: 1_722_400_000,
    score: 0.98,
    ...overrides
  };
}

function detail(id: number, overrides: Partial<GroupDetail> = {}): GroupDetail {
  return {
    id,
    kind: "exact",
    representativeFileId: 1,
    memberTotal: 3,
    memberPage: 1,
    memberSize: 100,
    members: [member(1), member(2, "agent-b"), member(3, "agent-c")],
    ...overrides
  };
}

function deletePreparation(overrides: Partial<DeletePreparation> = {}): DeletePreparation {
  return {
    confirmToken: "confirm-groups",
    expiresInSeconds: 60,
    summary: {
      totalFiles: 1,
      totalBytes: 1_000,
      byMachine: { "agent-b": 1 },
      samples: ["D:\\media\\file-2.jpg"]
    },
    ...overrides
  };
}

function deleteTaskStatus(overrides: Partial<DeleteTaskStatus> = {}): DeleteTaskStatus {
  return {
    taskId: "delete-task-groups",
    mode: "soft",
    total: 1,
    ok: 1,
    failed: 0,
    uncertain: 0,
    pending: 0,
    complete: true,
    stateSyncFailures: 0,
    byMachine: {},
    errorCodes: {},
    problems: [],
    ...overrides
  };
}

test("keeps only one uncertain item when the other 99 deletions succeeded", () => {
  const members = Array.from({ length: 100 }, (_, index) => ({
    fileId: index + 1,
    machineId: "agent-a",
    path: `D:\\media\\file-${index + 1}.jpg`
  }));
  const snapshot: DeleteReviewSnapshot = {
    groupId: 1,
    kind: "exact",
    scopeKey: "exact:1",
    members
  };
  const status = deleteTaskStatus({
    total: 100,
    ok: 99,
    failed: 1,
    uncertain: 1,
    problems: [{
      machineId: "agent-a",
      sequence: 99,
      path: "D:\\media\\file-100.jpg",
      errorCode: "E_HELPER_LOST",
      errorMessage: "helper connection lost",
      uncertain: true
    }]
  });

  expect(deriveDeleteRetryPlan(status, snapshot).retryMembers.map(member => member.fileId))
    .toEqual([100]);
});

function apiFor(options: {
  agents?: AgentStatus[] | (() => Promise<AgentStatus[]>);
  groups?: (query: GroupQuery, signal?: AbortSignal) => Promise<GroupPage>;
  detail?: (id: number, memberPage: number, memberSize: number, signal?: AbortSignal) => Promise<GroupDetail>;
  prepareDelete?: (memberIds: number[], signal?: AbortSignal) => Promise<DeletePreparation>;
  executeDelete?: (confirmToken: string, mode: "soft" | "hard", signal?: AbortSignal) => Promise<{ taskId: string }>;
  getDeleteStatus?: (taskId: string, signal?: AbortSignal) => Promise<DeleteTaskStatus>;
} = {}): AppApi {
  return {
    listAgents: vi.fn(typeof options.agents === "function" ? options.agents : async () => options.agents ?? [
      { machineId: "agent-a", addr: "10.0.0.1", online: true, identityState: "claimed" },
      { machineId: "agent-b", addr: "10.0.0.2", online: true, identityState: "claimed" }
    ]),
    listGroups: vi.fn(options.groups ?? (async query => page(query))),
    getGroup: vi.fn(options.detail ?? (async (id, memberPage, memberSize) => detail(id, { memberPage, memberSize }))),
    prepareDelete: vi.fn(options.prepareDelete ?? (async () => deletePreparation())),
    executeDelete: vi.fn(options.executeDelete ?? (async () => ({ taskId: "delete-task-groups" }))),
    getDeleteStatus: vi.fn(options.getDeleteStatus ?? (async () => deleteTaskStatus()))
  } as unknown as AppApi;
}

function deferred<T>() {
  let resolve: ((value: T) => void) | undefined;
  let reject: ((reason?: unknown) => void) | undefined;
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve: (value: T) => resolve?.(value), reject: (reason?: unknown) => reject?.(reason) };
}

function setViewport(width: number) {
  Object.defineProperty(window, "matchMedia", {
    configurable: true,
    value: vi.fn().mockImplementation((query: string) => ({
      matches: viewportMatches(query, width),
      media: query,
      onchange: null,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      dispatchEvent: vi.fn()
    }))
  });
}

function viewportMatches(query: string, width: number) {
  const maximum = /max-width:\s*(\d+)px/.exec(query);
  if (maximum) return width <= Number(maximum[1]);
  const minimum = /min-width:\s*(\d+)px/.exec(query);
  return minimum ? width >= Number(minimum[1]) : false;
}

function installResponsiveViewport(initialWidth: number) {
  let width = initialWidth;
  const subscriptions: Array<{
    query: string;
    listeners: Set<(event: MediaQueryListEvent) => void>;
  }> = [];
  Object.defineProperty(window, "matchMedia", {
    configurable: true,
    value: vi.fn().mockImplementation((query: string) => {
      const subscription = {
        query,
        listeners: new Set<(event: MediaQueryListEvent) => void>()
      };
      subscriptions.push(subscription);
      return {
        get matches() {
          return viewportMatches(query, width);
        },
        media: query,
        onchange: null,
        addEventListener: vi.fn((_type: string, listener: (event: MediaQueryListEvent) => void) => {
          subscription.listeners.add(listener);
        }),
        removeEventListener: vi.fn((_type: string, listener: (event: MediaQueryListEvent) => void) => {
          subscription.listeners.delete(listener);
        }),
        dispatchEvent: vi.fn()
      };
    })
  });
  return (nextWidth: number) => {
    width = nextWidth;
    for (const subscription of subscriptions) {
      const event = {
        matches: viewportMatches(subscription.query, width),
        media: subscription.query
      } as MediaQueryListEvent;
      for (const listener of subscription.listeners) listener(event);
    }
  };
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

describe("GroupsPage", () => {
  test("requests the first exact-group page at the fixed 100-summary window", async () => {
    const api = apiFor();
    render(<GroupsPage api={api} />);

    await act(async () => {});
    expect(api.listGroups).toHaveBeenCalledTimes(1);
    expect(api.listGroups).toHaveBeenLastCalledWith({
      kind: "exact",
      page: 1,
      size: 100,
      sort: "members_desc"
    }, expect.any(AbortSignal));
  });

  test("deduplicates claimed and conflicting endpoints by reported machine ID", async () => {
    const api = apiFor({ agents: [
      { machineId: "agent-a", addr: "10.0.0.1", online: true, identityState: "claimed" },
      { machineId: "agent-b", addr: "10.0.0.2", online: false, identityState: "conflict" },
      { machineId: "agent-b", addr: "10.0.0.3", online: true, identityState: "claimed" },
      { machineId: "", addr: "10.0.0.4", online: false, identityState: "pending" }
    ] });
    const user = userEvent.setup();
    render(<GroupsPage api={api} />);

    const agentFilter = screen.getByRole("combobox", { name: "Agent" });
    await waitFor(() => expect(within(agentFilter).getAllByRole("option")).toHaveLength(3));
    expect(within(agentFilter).getAllByRole("option", { name: "agent-b" })).toHaveLength(1);

    await user.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    expect(await screen.findByRole("checkbox", { name: "选择文件 2" })).toBeEnabled();
  });

  test("switching kind resets page, detail, and explicit member selection", async () => {
    const api = apiFor({
      groups: async query => page(query, { total: 300, groups: [group(query.page, query.kind)] }),
      detail: async (id, memberPage, memberSize) => detail(id, { memberPage, memberSize })
    });
    const user = userEvent.setup();
    render(<GroupsPage api={api} />);

    await screen.findByRole("button", { name: "打开重复组 1" });
    await user.click(screen.getByRole("button", { name: "下一页" }));
    await screen.findByRole("button", { name: "打开重复组 2" });
    await user.click(screen.getByRole("button", { name: "打开重复组 2" }));
    await screen.findByRole("checkbox", { name: /选择文件 2/ });
    await user.click(screen.getByRole("checkbox", { name: /选择文件 2/ }));
    expect(screen.getByText("已选 1 项")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "相似图片" }));

    await waitFor(() => expect(api.listGroups).toHaveBeenLastCalledWith({
      kind: "image",
      page: 1,
      size: 100,
      sort: "members_desc"
    }, expect.any(AbortSignal)));
    expect(screen.getByText("选择一个重复组以查看文件。")).toBeInTheDocument();
    expect(screen.getByText("已选 0 项")).toBeInTheDocument();
  });

  test.each([
    {
      name: "路径搜索",
      change: () => fireEvent.change(
        screen.getByRole("searchbox", { name: "路径搜索" }),
        { target: { value: "needle" } }
      )
    },
    {
      name: "Agent",
      change: () => fireEvent.change(
        screen.getByRole("combobox", { name: "Agent" }),
        { target: { value: "agent-b" } }
      )
    },
    {
      name: "最少文件数",
      change: () => fireEvent.change(
        screen.getByRole("spinbutton", { name: "最少文件数" }),
        { target: { value: "3" } }
      )
    },
    {
      name: "排序",
      change: () => fireEvent.change(
        screen.getByRole("combobox", { name: "排序" }),
        { target: { value: "reclaim_desc" } }
      )
    }
  ])("$name 变化立即清空旧组详情和显式删除选择", async ({ change }) => {
    const api = apiFor();
    const onRequestDelete = vi.fn();
    const user = userEvent.setup();
    render(<GroupsPage api={api} onRequestDelete={onRequestDelete} />);

    await user.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    await user.click(await screen.findByRole("checkbox", { name: "选择文件 2" }));
    expect(screen.getByText("已选 1 项")).toBeInTheDocument();

    change();

    expect(screen.getByText("选择一个重复组以查看文件。")).toBeInTheDocument();
    expect(screen.getByText("已选 0 项")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "删除已选 0 项" })).toBeDisabled();
    expect(onRequestDelete).not.toHaveBeenCalled();
  });

  test("debounces several path-search keystrokes into one effective list request", async () => {
    vi.useFakeTimers();
    const api = apiFor();
    render(<GroupsPage api={api} />);
    await act(async () => {});
    vi.mocked(api.listGroups).mockClear();

    fireEvent.change(screen.getByRole("searchbox", { name: "路径搜索" }), { target: { value: "poster" } });
    await act(async () => { await vi.advanceTimersByTimeAsync(299); });
    expect(api.listGroups).not.toHaveBeenCalled();
    await act(async () => { await vi.advanceTimersByTimeAsync(1); });

    await act(async () => {});
    expect(api.listGroups).toHaveBeenCalledTimes(1);
    expect(api.listGroups).toHaveBeenLastCalledWith({
      kind: "exact",
      page: 1,
      size: 100,
      q: "poster",
      sort: "members_desc"
    }, expect.any(AbortSignal));
  });

  test("keeps page two active until the 300ms search value becomes effective", async () => {
    vi.useFakeTimers();
    const api = apiFor({
      groups: async query => page(query, {
        total: 300,
        groups: [group(query.page, query.kind, { repPath: `D:\\page-${query.page}.jpg` })]
      })
    });
    render(<GroupsPage api={api} />);
    await act(async () => {});
    fireEvent.click(screen.getByRole("button", { name: "下一页" }));
    await act(async () => {});
    expect(screen.getByText("D:\\page-2.jpg")).toBeInTheDocument();
    vi.mocked(api.listGroups).mockClear();

    fireEvent.change(screen.getByRole("searchbox", { name: "路径搜索" }), { target: { value: "later" } });
    await act(async () => { await vi.advanceTimersByTimeAsync(299); });
    expect(api.listGroups).not.toHaveBeenCalled();
    expect(screen.getByText("D:\\page-2.jpg")).toBeInTheDocument();

    await act(async () => { await vi.advanceTimersByTimeAsync(1); });
    expect(api.listGroups).toHaveBeenCalledTimes(1);
    expect(api.listGroups).toHaveBeenCalledWith({
      kind: "exact",
      page: 1,
      q: "later",
      size: 100,
      sort: "members_desc"
    }, expect.any(AbortSignal));
  });

  test("hides old-scope rows immediately while a different kind loads", async () => {
    const nextScope = deferred<GroupPage>();
    const api = apiFor({
      groups: vi.fn()
        .mockResolvedValueOnce(page({ kind: "exact", page: 1, size: 100, sort: "members_desc" }, {
          groups: [group(1, "exact", { repPath: "D:\\old-scope.jpg" })]
        }))
        .mockReturnValueOnce(nextScope.promise)
    });
    render(<GroupsPage api={api} />);
    await screen.findByText("D:\\old-scope.jpg");

    fireEvent.click(screen.getByRole("button", { name: "相似图片" }));

    expect(screen.queryByText("D:\\old-scope.jpg")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "打开重复组 1" })).not.toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("正在加载重复组");
    await act(async () => nextScope.resolve(page({ kind: "image", page: 1, size: 100, sort: "members_desc" }, {
      groups: [group(9, "image")]
    })));
  });

  test("keeps the newer list response visible when an earlier query resolves last", async () => {
    vi.useFakeTimers();
    const oldRequest = deferred<GroupPage>();
    const newRequest = deferred<GroupPage>();
    const api = apiFor({
      groups: vi.fn()
        .mockReturnValueOnce(oldRequest.promise)
        .mockReturnValueOnce(newRequest.promise)
    });
    render(<GroupsPage api={api} />);

    fireEvent.change(screen.getByRole("searchbox", { name: "路径搜索" }), { target: { value: "fresh" } });
    await act(async () => { await vi.advanceTimersByTimeAsync(300); });
    await act(async () => {});
    expect(api.listGroups).toHaveBeenCalledTimes(2);
    await act(async () => newRequest.resolve(page({ kind: "exact", page: 1, size: 100, q: "fresh", sort: "members_desc" }, {
      groups: [group(9, "exact", { repPath: "D:\\fresh.jpg" })]
    })));
    expect(screen.getByText("D:\\fresh.jpg")).toBeInTheDocument();

    await act(async () => oldRequest.resolve(page({ kind: "exact", page: 1, size: 100, sort: "members_desc" }, {
      groups: [group(1, "exact", { repPath: "D:\\stale.jpg" })]
    })));
    expect(screen.getByText("D:\\fresh.jpg")).toBeInTheDocument();
    expect(screen.queryByText("D:\\stale.jpg")).not.toBeInTheDocument();
  });

  test("keeps the newer detail response visible when a prior group request resolves last", async () => {
    const firstDetail = deferred<GroupDetail>();
    const secondDetail = deferred<GroupDetail>();
    const api = apiFor({
      groups: async query => page(query, { total: 2, groups: [group(1), group(2)] }),
      detail: vi.fn()
        .mockReturnValueOnce(firstDetail.promise)
        .mockReturnValueOnce(secondDetail.promise)
    });
    const user = userEvent.setup();
    render(<GroupsPage api={api} />);

    await screen.findByRole("button", { name: "打开重复组 1" });
    await user.click(screen.getByRole("button", { name: "打开重复组 1" }));
    await user.click(screen.getByRole("button", { name: "打开重复组 2" }));
    await act(async () => secondDetail.resolve(detail(2, { members: [member(20, "agent-b", { path: "D:\\fresh-detail.jpg" })] })));
    await screen.findByText("D:\\fresh-detail.jpg");
    await act(async () => firstDetail.resolve(detail(1, { members: [member(10, "agent-a", { path: "D:\\stale-detail.jpg" })] })));

    expect(screen.getByText("D:\\fresh-detail.jpg")).toBeInTheDocument();
    expect(screen.queryByText("D:\\stale-detail.jpg")).not.toBeInTheDocument();
  });

  test("starts a fresh loading session when the same group is closed and reopened", async () => {
    const replacement = deferred<GroupDetail>();
    const getGroup = vi.fn()
      .mockResolvedValueOnce(detail(1, {
        members: [member(2, "agent-b", { path: "D:\\old-session.jpg" })]
      }))
      .mockReturnValueOnce(replacement.promise);
    const api = apiFor({ detail: getGroup });
    render(<GroupsPage api={api} />);

    fireEvent.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    expect(await screen.findByText("D:\\old-session.jpg")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "关闭详情" }));
    fireEvent.click(screen.getByRole("button", { name: "打开重复组 1" }));

    expect(screen.queryByText("D:\\old-session.jpg")).not.toBeInTheDocument();
    expect(screen.queryByRole("checkbox", { name: "选择文件 2" })).not.toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("正在加载重复组详情");

    await act(async () => replacement.resolve(detail(1, {
      members: [member(4, "agent-b", { path: "D:\\new-session.jpg" })]
    })));
    expect(screen.getByText("D:\\new-session.jpg")).toBeInTheDocument();
    expect(getGroup).toHaveBeenCalledTimes(2);
  });

  test("select-all and delete handoff contain only eligible members from the loaded detail page", async () => {
    const onRequestDelete = vi.fn();
    const api = apiFor({
      detail: async (id, memberPage, memberSize) => detail(id, {
        memberPage,
        memberSize,
        members: [member(1), member(2, "agent-b"), member(3, "agent-c"), member(4, "unknown-agent")]
      })
    });
    const user = userEvent.setup();
    render(<GroupsPage api={api} onRequestDelete={onRequestDelete} />);

    await user.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    await screen.findByRole("checkbox", { name: "全选当前页可删除项" });
    await user.click(screen.getByRole("checkbox", { name: "全选当前页可删除项" }));

    expect(screen.getByRole("checkbox", { name: /选择文件 1/ })).toBeDisabled();
    expect(screen.getByRole("checkbox", { name: /选择文件 3/ })).toBeDisabled();
    expect(screen.getByRole("checkbox", { name: /选择文件 4/ })).toBeDisabled();
    expect(screen.getByText("已选 1 项")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "删除已选 1 项" }));
    expect(onRequestDelete).toHaveBeenCalledWith([2]);
    expect(api.prepareDelete).not.toHaveBeenCalled();
    expect(screen.queryByRole("dialog", { name: "确认删除" })).not.toBeInTheDocument();
  });

  test("opens the real delete dialog by default from the live eligible selection", async () => {
    const api = apiFor();
    const user = userEvent.setup();
    render(<GroupsPage api={api} />);

    await user.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    await user.click(await screen.findByRole("checkbox", { name: "选择文件 2" }));
    await user.click(screen.getByRole("button", { name: "删除已选 1 项" }));

    const dialog = await screen.findByRole("dialog", { name: "确认删除" });
    expect(within(dialog).getByRole("radio", { name: "软删除" })).toBeChecked();
    expect(api.prepareDelete).toHaveBeenCalledTimes(1);
    expect(api.prepareDelete).toHaveBeenCalledWith([2], expect.any(AbortSignal));
    expect(api.executeDelete).not.toHaveBeenCalled();
  });

  test("re-prepares a changed live eligible selection and can execute only the replacement token", async () => {
    const prepareDelete = vi.fn()
      .mockResolvedValueOnce(deletePreparation({ confirmToken: "stale-token" }))
      .mockResolvedValueOnce(deletePreparation({ confirmToken: "replacement-token" }));
    const api = apiFor({
      detail: async (id, memberPage, memberSize) => detail(id, {
        memberPage,
        memberSize,
        members: [member(1), member(2, "agent-b"), member(4, "agent-b")]
      }),
      prepareDelete
    });
    const user = userEvent.setup();
    render(<GroupsPage api={api} />);

    await user.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    await user.click(await screen.findByRole("checkbox", { name: "选择文件 2" }));
    await user.click(screen.getByRole("button", { name: "删除已选 1 项" }));
    expect(await screen.findByText("确认令牌剩余 60 秒")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("checkbox", { name: "选择文件 4", hidden: true }));
    await waitFor(() => expect(prepareDelete).toHaveBeenLastCalledWith([2, 4], expect.any(AbortSignal)));
    expect(prepareDelete).toHaveBeenCalledTimes(2);

    await user.click(screen.getByRole("button", { name: "最终确认删除" }));
    expect(api.executeDelete).toHaveBeenCalledTimes(1);
    expect(api.executeDelete).toHaveBeenCalledWith("replacement-token", "soft", expect.any(AbortSignal));
    expect(api.executeDelete).not.toHaveBeenCalledWith("stale-token", expect.anything(), expect.anything());
  });

  test("fails closed and discards the old token when a refresh makes the selected member representative", async () => {
    const getGroup = vi.fn()
      .mockResolvedValueOnce(detail(1, { representativeFileId: 1 }))
      .mockResolvedValueOnce(detail(1, { representativeFileId: 2 }));
    const api = apiFor({ detail: getGroup });
    const user = userEvent.setup();
    render(<GroupsPage api={api} />);

    await user.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    await user.click(await screen.findByRole("checkbox", { name: "选择文件 2" }));
    await user.click(screen.getByRole("button", { name: "删除已选 1 项" }));
    expect(await screen.findByText("确认令牌剩余 60 秒")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "刷新成员", hidden: true }));
    await waitFor(() => expect(screen.getByText("已选 0 项")).toBeInTheDocument());

    expect(screen.getByRole("dialog", { name: "确认删除" })).toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent("没有可删除的已选文件");
    expect(screen.queryByRole("button", { name: "最终确认删除" })).not.toBeInTheDocument();
    expect(api.prepareDelete).toHaveBeenCalledTimes(1);
    expect(api.executeDelete).not.toHaveBeenCalled();
  });

  test("locks filters, pagination, detail actions, and member selection during prepare and execute", async () => {
    const prepare = deferred<DeletePreparation>();
    const execute = deferred<{ taskId: string }>();
    const api = apiFor({
      groups: async query => page(query, { total: 300 }),
      prepareDelete: () => prepare.promise,
      executeDelete: () => execute.promise
    });
    const user = userEvent.setup();
    render(<GroupsPage api={api} />);

    await user.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    await user.click(await screen.findByRole("checkbox", { name: "选择文件 2" }));
    const kindButton = screen.getByRole("button", { name: "相似图片" });
    const search = screen.getByRole("searchbox", { name: "路径搜索" });
    const nextPage = screen.getByRole("button", { name: "下一页" });
    const openGroup = screen.getByRole("button", { name: "打开重复组 1" });
    const closeDetail = screen.getByRole("button", { name: "关闭详情" });
    const memberCheckbox = screen.getByRole("checkbox", { name: "选择文件 2" });
    const refreshMembers = screen.getByRole("button", { name: "刷新成员" });

    await user.click(screen.getByRole("button", { name: "删除已选 1 项" }));
    const dialog = await screen.findByRole("dialog", { name: "确认删除" });
    expect(within(dialog).getByRole("status")).toHaveTextContent("正在准备删除");
    for (const control of [kindButton, search, nextPage, openGroup, closeDetail, memberCheckbox, refreshMembers]) {
      expect(control).toBeDisabled();
    }
    await user.keyboard("{Escape}");
    expect(screen.getByRole("dialog", { name: "确认删除" })).toBeInTheDocument();
    await user.click(nextPage);
    expect(api.listGroups).toHaveBeenCalledTimes(1);

    await act(async () => prepare.resolve(deletePreparation()));
    expect(await within(dialog).findByRole("radio", { name: "软删除" })).toBeChecked();
    expect(kindButton).toBeEnabled();
    expect(nextPage).toBeEnabled();

    await user.click(within(dialog).getByRole("button", { name: "最终确认删除" }));
    expect(await within(dialog).findByRole("status")).toHaveTextContent("正在提交删除");
    for (const control of [kindButton, search, nextPage, openGroup, closeDetail, memberCheckbox, refreshMembers]) {
      expect(control).toBeDisabled();
    }
    await user.click(kindButton);
    expect(api.listGroups).toHaveBeenCalledTimes(1);

    await act(async () => execute.resolve({ taskId: "delete-task-groups" }));
    expect(await screen.findByText("任务 ID：delete-task-groups")).toBeInTheDocument();
    expect(kindButton).toBeEnabled();
    expect(nextPage).toBeEnabled();
  });

  test("terminal deletion clears selection once, reloads list and detail once, and keeps the task modal mounted", async () => {
    const listGroups = vi.fn(async (query: GroupQuery) => page(query));
    const getGroup = vi.fn(async (id: number, memberPage: number, memberSize: number) =>
      detail(id, { memberPage, memberSize }));
    const terminal = deleteTaskStatus();
    const api = apiFor({
      groups: listGroups,
      detail: getGroup,
      getDeleteStatus: vi.fn().mockResolvedValue(terminal)
    });
    const user = userEvent.setup();
    render(<GroupsPage api={api} />);

    await user.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    await user.click(await screen.findByRole("checkbox", { name: "选择文件 2" }));
    await user.click(screen.getByRole("button", { name: "删除已选 1 项" }));
    const dialog = await screen.findByRole("dialog", { name: "确认删除" });
    await user.click(await within(dialog).findByRole("button", { name: "最终确认删除" }));

    await waitFor(() => expect(screen.getByText("已选 0 项")).toBeInTheDocument());
    const terminalDialog = screen.getByRole("dialog", { name: "确认删除" });
    expect(within(terminalDialog).getByText("任务 ID：delete-task-groups")).toBeInTheDocument();
    await waitFor(() => {
      expect(listGroups).toHaveBeenCalledTimes(2);
      expect(getGroup).toHaveBeenCalledTimes(2);
    });
    await act(async () => {});
    expect(listGroups).toHaveBeenCalledTimes(2);
    expect(getGroup).toHaveBeenCalledTimes(2);
    expect(api.getDeleteStatus).toHaveBeenCalledTimes(1);
    expect(within(screen.getByRole("dialog", { name: "确认删除" }))
      .getByText("任务 ID：delete-task-groups")).toBeInTheDocument();

    await user.click(within(screen.getByRole("dialog", { name: "确认删除" }))
      .getByRole("button", { name: "关闭" }));
    expect(screen.queryByRole("dialog", { name: "确认删除" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "查看进行中的删除任务" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "删除已选 0 项" })).toBeDisabled();
  });

  test("terminal deletion retains failed items so they can be explicitly retried", async () => {
    const terminal = deleteTaskStatus({
      ok: 0,
      failed: 1,
      problems: [{
        machineId: "agent-b",
        sequence: 0,
        path: "D:\\media\\file-2.jpg",
        errorCode: "E_IN_USE",
        errorMessage: "file in use",
        uncertain: false
      }]
    });
    const api = apiFor({ getDeleteStatus: vi.fn().mockResolvedValue(terminal) });
    const user = userEvent.setup();
    render(<GroupsPage api={api} />);

    await user.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    await user.click(await screen.findByRole("checkbox", { name: "选择文件 2" }));
    await user.click(screen.getByRole("button", { name: "删除已选 1 项" }));
    const dialog = await screen.findByRole("dialog", { name: "确认删除" });
    await user.click(await within(dialog).findByRole("button", { name: "最终确认删除" }));

    await waitFor(() => expect(api.executeDelete).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(api.getDeleteStatus).toHaveBeenCalledTimes(1));
    await screen.findByText("状态：已完成");
    await waitFor(() => expect(screen.getByText("已选 1 项")).toBeInTheDocument());
    expect(screen.getAllByText(/失败或不确定项已保留/)).toHaveLength(2);
    await user.click(screen.getByRole("button", { name: "关闭" }));

    const retry = screen.getByRole("button", { name: "删除已选 1 项" });
    expect(retry).toBeEnabled();
    await user.click(retry);
    expect(api.prepareDelete).toHaveBeenCalledTimes(2);
  });

  test("an older task finishing does not clear or refresh a different group selection", async () => {
    const getGroup = vi.fn(async (id: number, memberPage: number, memberSize: number) => detail(id, {
      representativeFileId: id * 10 + 1,
      memberPage,
      memberSize,
      members: [member(id * 10 + 1), member(id * 10 + 2, "agent-b")]
    }));
    const getDeleteStatus = vi.fn()
      .mockRejectedValueOnce(new Error("status temporarily unavailable"))
      .mockResolvedValueOnce(deleteTaskStatus());
    const listGroups = vi.fn(async (query: GroupQuery) => page(query, {
      total: 2,
      groups: [group(1), group(2)]
    }));
    const api = apiFor({ detail: getGroup, getDeleteStatus, groups: listGroups });
    const user = userEvent.setup();
    render(<GroupsPage api={api} />);

    await user.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    await user.click(await screen.findByRole("checkbox", { name: "选择文件 12" }));
    await user.click(screen.getByRole("button", { name: "删除已选 1 项" }));
    await user.click(await screen.findByRole("button", { name: "最终确认删除" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("status temporarily unavailable");
    await user.click(screen.getByRole("button", { name: "关闭 确认删除" }));

    await user.click(screen.getByRole("button", { name: "打开重复组 2" }));
    await user.click(await screen.findByRole("checkbox", { name: "选择文件 22" }));
    expect(screen.getByText("已选 1 项")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "查看进行中的删除任务" }));
    expect(await screen.findByText("任务 ID：delete-task-groups")).toBeInTheDocument();
    await waitFor(() => expect(getDeleteStatus).toHaveBeenCalledTimes(2));

    expect(screen.getByText("已选 1 项")).toBeInTheDocument();
    expect(screen.getByRole("checkbox", { name: "选择文件 22", hidden: true })).toBeChecked();
    expect(getGroup.mock.calls.filter(([id]) => id === 2)).toHaveLength(1);
    expect(listGroups).toHaveBeenCalledTimes(1);
  });

  test("a 503 prepare failure preserves selection and requires an explicit re-prepare", async () => {
    const prepareDelete = vi.fn()
      .mockRejectedValueOnce(new ApiError(503, "prepare unavailable", true))
      .mockResolvedValueOnce(deletePreparation());
    const api = apiFor({ prepareDelete });
    const user = userEvent.setup();
    render(<GroupsPage api={api} />);

    await user.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    await user.click(await screen.findByRole("checkbox", { name: "选择文件 2" }));
    await user.click(screen.getByRole("button", { name: "删除已选 1 项" }));
    const dialog = await screen.findByRole("dialog", { name: "确认删除" });

    expect(await within(dialog).findByRole("alert")).toHaveTextContent("prepare unavailable");
    expect(screen.getByText("已选 1 项")).toBeInTheDocument();
    expect(prepareDelete).toHaveBeenCalledTimes(1);
    expect(api.executeDelete).not.toHaveBeenCalled();
    await act(async () => {});
    expect(prepareDelete).toHaveBeenCalledTimes(1);

    await user.click(within(dialog).getByRole("button", { name: "重新准备" }));
    expect(await within(dialog).findByRole("radio", { name: "软删除" })).toBeChecked();
    expect(prepareDelete).toHaveBeenCalledTimes(2);
    expect(screen.getByText("已选 1 项")).toBeInTheDocument();
    expect(api.executeDelete).not.toHaveBeenCalled();
  });

  test("a 503 execute failure preserves selection and retries only after a second confirmation", async () => {
    const executeDelete = vi.fn()
      .mockRejectedValueOnce(new ApiError(503, "execute unavailable", true))
      .mockResolvedValueOnce({ taskId: "delete-task-groups" });
    const api = apiFor({ executeDelete });
    const user = userEvent.setup();
    render(<GroupsPage api={api} />);

    await user.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    await user.click(await screen.findByRole("checkbox", { name: "选择文件 2" }));
    await user.click(screen.getByRole("button", { name: "删除已选 1 项" }));
    const dialog = await screen.findByRole("dialog", { name: "确认删除" });
    const confirm = await within(dialog).findByRole("button", { name: "最终确认删除" });
    await user.click(confirm);

    expect(await within(dialog).findByRole("alert")).toHaveTextContent("execute unavailable");
    expect(screen.getByText("已选 1 项")).toBeInTheDocument();
    expect(executeDelete).toHaveBeenCalledTimes(1);
    await act(async () => {});
    expect(executeDelete).toHaveBeenCalledTimes(1);

    await user.click(within(dialog).getByRole("button", { name: "最终确认删除" }));
    expect(await screen.findByText("任务 ID：delete-task-groups")).toBeInTheDocument();
    expect(executeDelete).toHaveBeenCalledTimes(2);
  });

  test("closing a poll error reopens the same accepted task without another prepare or execute", async () => {
    const inProgress = deleteTaskStatus({ complete: false, ok: 0, pending: 1 });
    const getDeleteStatus = vi.fn()
      .mockRejectedValueOnce(new Error("状态同步暂不可用"))
      .mockResolvedValueOnce(inProgress);
    const api = apiFor({ getDeleteStatus });
    const user = userEvent.setup();
    render(<GroupsPage api={api} />);

    await user.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    await user.click(await screen.findByRole("checkbox", { name: "选择文件 2" }));
    await user.click(screen.getByRole("button", { name: "删除已选 1 项" }));
    const dialog = await screen.findByRole("dialog", { name: "确认删除" });
    await user.click(await within(dialog).findByRole("button", { name: "最终确认删除" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("状态同步暂不可用");

    await user.click(within(screen.getByRole("dialog", { name: "确认删除" }))
      .getByRole("button", { name: "关闭 确认删除" }));
    expect(screen.queryByRole("dialog", { name: "确认删除" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "查看进行中的删除任务" })).toBeEnabled();
    expect(screen.queryByRole("button", { name: "删除已选 1 项" })).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "查看进行中的删除任务" }));
    expect(await screen.findByText("任务 ID：delete-task-groups")).toBeInTheDocument();
    await waitFor(() => expect(getDeleteStatus).toHaveBeenCalledTimes(2));
    expect(getDeleteStatus).toHaveBeenLastCalledWith("delete-task-groups", expect.any(AbortSignal));
    expect(api.prepareDelete).toHaveBeenCalledTimes(1);
    expect(api.executeDelete).toHaveBeenCalledTimes(1);
  });

  test("unchecking current-page select-all preserves explicit selections from another member page", async () => {
    const api = apiFor({
      detail: async (id, memberPage, memberSize) => detail(id, memberPage === 1 ? {
        memberTotal: 200,
        memberPage,
        memberSize,
        members: [member(1), member(2, "agent-b")]
      } : {
        memberTotal: 200,
        memberPage,
        memberSize,
        members: [member(3), member(4, "agent-b")]
      })
    });
    render(<GroupsPage api={api} />);
    fireEvent.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    fireEvent.click(await screen.findByRole("checkbox", { name: "选择文件 2" }));
    fireEvent.click(screen.getByRole("button", { name: "下一页成员" }));
    await screen.findByRole("checkbox", { name: "选择文件 4" });

    const selectAll = screen.getByRole("checkbox", { name: "全选当前页可删除项" });
    fireEvent.click(selectAll);
    expect(screen.getByText("已选 3 项")).toBeInTheDocument();
    fireEvent.click(selectAll);

    expect(screen.getByText("已选 1 项")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "上一页成员" }));
    expect(await screen.findByRole("checkbox", { name: "选择文件 2" })).toBeChecked();
  });

  test("evicts an already selected member when a refreshed detail makes it representative", async () => {
    const getGroup = vi.fn()
      .mockResolvedValueOnce(detail(1, { representativeFileId: 1 }))
      .mockResolvedValueOnce(detail(1, { representativeFileId: 2 }));
    const api = apiFor({ detail: getGroup });
    const user = userEvent.setup();
    render(<GroupsPage api={api} />);

    await user.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    await user.click(await screen.findByRole("checkbox", { name: /选择文件 2/ }));
    expect(screen.getByText("已选 1 项")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "刷新成员" }));

    await waitFor(() => expect(screen.getByRole("checkbox", { name: /选择文件 2/ })).toBeDisabled());
    expect(screen.getByText("已选 0 项")).toBeInTheDocument();
  });

  test("labels offline and unknown-agent members as disabled fail-safe rows", async () => {
    const api = apiFor({
      detail: async (id, memberPage, memberSize) => detail(id, {
        memberPage,
        memberSize,
        members: [member(1), member(2, "agent-b"), member(3, "agent-c"), member(4, "missing-agent")]
      })
    });
    const user = userEvent.setup();
    render(<GroupsPage api={api} />);

    await user.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    expect(await screen.findAllByText("Agent 离线")).toHaveLength(2);
    expect(screen.getByRole("checkbox", { name: /选择文件 3/ })).toBeDisabled();
    expect(screen.getByRole("checkbox", { name: /选择文件 4/ })).toBeDisabled();
  });

  test("polls Agent status and evicts a selected member when its Agent goes offline", async () => {
    vi.useFakeTimers();
    const listAgents = vi.fn()
      .mockResolvedValueOnce([
        { machineId: "agent-a", addr: "10.0.0.1", online: true, identityState: "claimed" },
        { machineId: "agent-b", addr: "10.0.0.2", online: true, identityState: "claimed" }
      ])
      .mockResolvedValue([
        { machineId: "agent-a", addr: "10.0.0.1", online: true, identityState: "claimed" },
        { machineId: "agent-b", addr: "10.0.0.2", online: false, identityState: "claimed" }
      ]);
    const api = apiFor({ agents: listAgents });
    render(<GroupsPage api={api} />);
    await act(async () => {});
    fireEvent.click(screen.getByRole("button", { name: "打开重复组 1" }));
    await act(async () => {});
    fireEvent.click(screen.getByRole("checkbox", { name: "选择文件 2" }));
    expect(screen.getByText("已选 1 项")).toBeInTheDocument();

    await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });

    expect(listAgents).toHaveBeenCalledTimes(2);
    expect(screen.getByRole("checkbox", { name: "选择文件 2" })).toBeDisabled();
    expect(screen.getByText("已选 0 项")).toBeInTheDocument();
    expect(screen.getAllByText("Agent 离线")).toHaveLength(2);
  });

  test("evicts a selected member when its owner goes offline after navigating to another member page", async () => {
    vi.useFakeTimers();
    const listAgents = vi.fn()
      .mockResolvedValueOnce([
        { machineId: "agent-a", addr: "10.0.0.1", online: true, identityState: "claimed" },
        { machineId: "agent-b", addr: "10.0.0.2", online: true, identityState: "claimed" }
      ])
      .mockResolvedValue([
        { machineId: "agent-a", addr: "10.0.0.1", online: true, identityState: "claimed" },
        { machineId: "agent-b", addr: "10.0.0.2", online: false, identityState: "claimed" }
      ]);
    const api = apiFor({
      agents: listAgents,
      detail: async (id, memberPage, memberSize) => detail(id, memberPage === 1 ? {
        memberTotal: 200,
        memberPage,
        memberSize,
        members: [member(1), member(2, "agent-b")]
      } : {
        memberTotal: 200,
        memberPage,
        memberSize,
        members: [member(3), member(4, "agent-a")]
      })
    });
    render(<GroupsPage api={api} />);
    await act(async () => {});
    fireEvent.click(screen.getByRole("button", { name: "打开重复组 1" }));
    await act(async () => {});
    fireEvent.click(screen.getByRole("checkbox", { name: "选择文件 2" }));
    fireEvent.click(screen.getByRole("button", { name: "下一页成员" }));
    await act(async () => {});
    expect(screen.getByRole("checkbox", { name: "选择文件 4" })).toBeInTheDocument();
    expect(screen.getByText("已选 1 项")).toBeInTheDocument();

    await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });

    expect(listAgents).toHaveBeenCalledTimes(2);
    expect(screen.getByText("已选 0 项")).toBeInTheDocument();
  });

  test("treats a polling failure as unverified Agent state and clears selection", async () => {
    vi.useFakeTimers();
    const listAgents = vi.fn()
      .mockResolvedValueOnce([
        { machineId: "agent-a", addr: "10.0.0.1", online: true, identityState: "claimed" },
        { machineId: "agent-b", addr: "10.0.0.2", online: true, identityState: "claimed" }
      ])
      .mockRejectedValueOnce(new Error("Agent 状态读取失败"));
    const api = apiFor({ agents: listAgents });
    render(<GroupsPage api={api} />);
    await act(async () => {});
    fireEvent.click(screen.getByRole("button", { name: "打开重复组 1" }));
    await act(async () => {});
    fireEvent.click(screen.getByRole("checkbox", { name: "选择文件 2" }));
    expect(screen.getByText("已选 1 项")).toBeInTheDocument();

    await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });

    expect(screen.getByRole("alert")).toHaveTextContent("Agent 状态不可用");
    expect(screen.getByRole("checkbox", { name: "选择文件 2" })).toBeDisabled();
    expect(screen.getByText("已选 0 项")).toBeInTheDocument();
  });

  test("keeps Agent state unverified while the retry after a polling failure is pending", async () => {
    vi.useFakeTimers();
    const retry = deferred<AgentStatus[]>();
    const listAgents = vi.fn()
      .mockResolvedValueOnce([
        { machineId: "agent-a", addr: "10.0.0.1", online: true, identityState: "claimed" },
        { machineId: "agent-b", addr: "10.0.0.2", online: true, identityState: "claimed" }
      ])
      .mockRejectedValueOnce(new Error("Agent 状态读取失败"))
      .mockReturnValueOnce(retry.promise);
    const api = apiFor({ agents: listAgents });
    render(<GroupsPage api={api} />);
    await act(async () => {});
    fireEvent.click(screen.getByRole("button", { name: "打开重复组 1" }));
    await act(async () => {});
    fireEvent.click(screen.getByRole("checkbox", { name: "选择文件 2" }));

    await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });
    expect(screen.getByRole("alert")).toHaveTextContent("Agent 状态不可用");
    expect(screen.getByRole("checkbox", { name: "选择文件 2" })).toBeDisabled();

    await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });
    expect(listAgents).toHaveBeenCalledTimes(3);
    expect(screen.getByRole("alert")).toHaveTextContent("Agent 状态不可用");
    expect(screen.getByRole("checkbox", { name: "选择文件 2" })).toBeDisabled();

    await act(async () => retry.resolve([
      { machineId: "agent-a", addr: "10.0.0.1", online: true, identityState: "claimed" },
      { machineId: "agent-b", addr: "10.0.0.2", online: true, identityState: "claimed" }
    ]));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.getByRole("checkbox", { name: "选择文件 2" })).toBeEnabled();
    expect(screen.getByText("已选 0 项")).toBeInTheDocument();
  });

  test("renders malicious paths and scores as literal text rather than HTML", async () => {
    const malicious = "<img src=x onerror=alert(1)>";
    const api = apiFor({
      groups: async query => page(query, { groups: [group(1, "exact", { repPath: malicious })] }),
      detail: async (id, memberPage, memberSize) => detail(id, {
        memberPage,
        memberSize,
        members: [member(1, "agent-a", { score: { z: malicious, a: ["safe", malicious] } })]
      })
    });
    const user = userEvent.setup();
    render(<GroupsPage api={api} />);

    expect(await screen.findByText(malicious)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "打开重复组 1" }));
    await screen.findByText(/a: \[safe, <img src=x onerror=alert\(1\)>\], z: <img src=x onerror=alert\(1\)>/);
    expect(document.querySelector("img")).toBeNull();
  });

  test("bounds huge score keys, strings, object breadth, and circular references", async () => {
    const cycle: Record<string, unknown> = {};
    cycle.self = cycle;
    const hugeScore: Record<string, unknown> = {
      ["a".repeat(300)]: "v".repeat(2_000),
      cycle
    };
    for (let index = 0; index < 5_000; index += 1) {
      hugeScore[`z-${String(index).padStart(5, "0")}`] = index;
    }
    const api = apiFor({
      detail: async (id, memberPage, memberSize) => detail(id, {
        memberPage,
        memberSize,
        members: [member(1, "agent-a", { score: hugeScore })]
      })
    });
    render(<GroupsPage api={api} />);

    fireEvent.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    const score = await screen.findByTestId("member-score");

    expect(score.textContent?.length).toBeLessThanOrEqual(512);
    expect(score).toHaveTextContent("[Circular]");
    expect(score).not.toHaveTextContent("a".repeat(300));
    expect(score).not.toHaveTextContent("v".repeat(2_000));
  });

  test("keeps long member fields on one row and opens bounded full information from the keyboard", async () => {
    const longPath = `D:\\media\\${"nested-folder\\".repeat(30)}poster.jpg`;
    const api = apiFor({
      detail: async (id, memberPage, memberSize) => detail(id, {
        memberPage,
        memberSize,
        members: [member(2, "agent-b", {
          path: longPath,
          score: { explanation: "similarity-detail-".repeat(40) }
        })]
      })
    });
    const user = userEvent.setup();
    render(<GroupsPage api={api} />);

    fireEvent.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    const pathValue = await screen.findByText(longPath);
    const memberRow = pathValue.closest("article");
    const memberList = screen.getByRole("list", { name: "重复组成员列表" }).closest(".group-detail__members");
    expect(pathValue).toHaveClass("group-detail__member-value--truncated");
    expect(memberRow).toHaveAttribute("data-row-height", "208");
    expect(memberList).toHaveAttribute("data-row-height", "208");

    const inspect = screen.getByRole("button", { name: "查看文件 2 完整信息" });
    inspect.focus();
    await user.keyboard("{Enter}");

    const dialog = screen.getByRole("dialog", { name: "文件 2 完整信息" });
    expect(within(dialog).getByTestId("member-full-path")).toHaveTextContent(longPath);
    expect(within(dialog).getByTestId("member-full-score").textContent?.length).toBeLessThanOrEqual(512);
    await user.click(within(dialog).getByRole("button", { name: /关闭.*文件 2 完整信息/ }));
    expect(screen.queryByRole("dialog", { name: "文件 2 完整信息" })).not.toBeInTheDocument();
  });

  test("traps complete-information focus, closes on Escape, and restores the inspect trigger", async () => {
    const api = apiFor({
      detail: async (id, memberPage, memberSize) => detail(id, {
        memberPage,
        memberSize,
        members: [member(2, "agent-b")]
      })
    });
    const user = userEvent.setup();
    render(<GroupsPage api={api} />);

    await user.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    const inspect = await screen.findByRole("button", { name: "查看文件 2 完整信息" });
    inspect.focus();
    await user.keyboard("{Enter}");

    const dialog = screen.getByRole("dialog", { name: "文件 2 完整信息" });
    const close = within(dialog).getByRole("button", { name: /关闭.*文件 2 完整信息/ });
    expect(close).toHaveFocus();
    await user.tab();
    expect(close).toHaveFocus();

    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog", { name: "文件 2 完整信息" })).not.toBeInTheDocument();
    expect(inspect).toHaveFocus();
  });

  test("mounts a bounded virtual subset of a 100-summary page even when total reports one million", async () => {
    vi.spyOn(HTMLElement.prototype, "offsetHeight", "get").mockReturnValue(512);
    vi.spyOn(HTMLElement.prototype, "offsetWidth", "get").mockReturnValue(800);
    const summaries = Array.from({ length: 100 }, (_, index) => group(index + 1));
    const api = apiFor({
      groups: async query => page(query, { total: 1_000_000, groups: summaries })
    });
    render(<GroupsPage api={api} />);

    const list = await screen.findByRole("list", { name: "重复组列表" });
    await waitFor(() => expect(list.querySelectorAll('[role="listitem"]').length).toBeLessThan(100));
    expect(screen.getByText("本页 100 个重复组 / 共 1,000,000 个")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "打开重复组 1000000" })).not.toBeInTheDocument();
  });

  test("virtualizes a 100-member detail page with a bounded mounted subset", async () => {
    const members = Array.from({ length: 100 }, (_, index) => member(index + 1));
    const api = apiFor({
      detail: async (id, memberPage, memberSize) => detail(id, {
        memberPage,
        memberSize,
        memberTotal: 100,
        members
      })
    });
    render(<GroupsPage api={api} />);

    fireEvent.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    const list = await screen.findByRole("list", { name: "重复组成员列表" });

    expect(list.querySelectorAll('[role="listitem"]').length).toBeGreaterThan(0);
    expect(list.querySelectorAll('[role="listitem"]').length).toBeLessThan(100);
  });

  test("uses compact and comfortable 44/56 pixel row estimates", async () => {
    const api = apiFor();
    const user = userEvent.setup();
    render(<GroupsPage api={api} />);

    await screen.findByRole("list", { name: "重复组列表" });
    expect(screen.getByText("行高估算：44px")).toBeInTheDocument();
    expect(screen.getByRole("region", { name: "重复组结果" })).toHaveAttribute("data-row-height", "44");
    expect(screen.getByRole("button", { name: "打开重复组 1" })).toHaveAttribute("data-row-height", "44");
    await user.click(screen.getByRole("radio", { name: "舒适密度" }));
    expect(screen.getByText("行高估算：56px")).toBeInTheDocument();
    expect(screen.getByRole("region", { name: "重复组结果" })).toHaveAttribute("data-row-height", "56");
    expect(screen.getByRole("button", { name: "打开重复组 1" })).toHaveAttribute("data-row-height", "56");
  });

  test("keeps virtual row actions separate from styled pagination controls", async () => {
    const api = apiFor({
      groups: async query => page(query, { total: 300, groups: [group(1)] })
    });
    render(<GroupsPage api={api} />);

    const rowAction = await screen.findByRole("button", { name: "打开重复组 1" });
    const nextPage = screen.getByRole("button", { name: "下一页" });
    expect(rowAction).toHaveClass("group-table__row-action");
    expect(rowAction).not.toHaveClass("group-table__pager-button");
    expect(nextPage).toHaveClass("group-table__pager-button");
  });

  test.each([1440, 1280])("keeps detail inline at %ipx without exposing a modal drawer", async width => {
    setViewport(width);
    const pending = deferred<GroupDetail>();
    const api = apiFor({ detail: () => pending.promise });
    render(<GroupsPage api={api} />);

    fireEvent.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    const detailPanel = screen.getByLabelText("重复组详情");
    expect(detailPanel).not.toHaveAttribute("role", "dialog");
    expect(detailPanel).not.toHaveAttribute("aria-modal");
    expect(detailPanel).toHaveTextContent("正在加载重复组详情");
  });

  test("uses an accessible scrim-backed drawer below 1280px and restores its exact trigger", async () => {
    setViewport(1024);
    const api = apiFor();
    const user = userEvent.setup();
    const view = render(<GroupsPage api={api} />);

    const trigger = await screen.findByRole("button", { name: "打开重复组 1" });
    const workbench = screen.getByRole("group", { name: "重复组交互控件" });
    expect(workbench).not.toHaveAttribute("inert");
    expect(view.container).not.toHaveAttribute("inert");
    expect(view.container).not.toHaveAttribute("aria-hidden");
    await user.click(trigger);

    const drawer = screen.getByRole("dialog", { name: "重复组详情" });
    const close = within(drawer).getByRole("button", { name: "关闭详情" });
    const enabledButtons = within(drawer).getAllByRole("button")
      .filter(button => !(button as HTMLButtonElement).disabled);
    expect(drawer).toHaveAttribute("aria-modal", "true");
    expect(screen.getByTestId("group-detail-scrim")).toBeInTheDocument();
    expect(workbench).toHaveAttribute("inert");
    expect(view.container).toHaveAttribute("inert");
    expect(view.container).toHaveAttribute("aria-hidden", "true");
    expect(close).toHaveFocus();

    await user.tab({ shift: true });
    expect(enabledButtons.at(-1)).toHaveFocus();
    await user.tab();
    expect(close).toHaveFocus();

    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog", { name: "重复组详情" })).not.toBeInTheDocument();
    expect(screen.queryByTestId("group-detail-scrim")).not.toBeInTheDocument();
    expect(workbench).not.toHaveAttribute("inert");
    expect(view.container).not.toHaveAttribute("inert");
    expect(view.container).not.toHaveAttribute("aria-hidden");
    expect(trigger).toHaveFocus();
  });

  test("closes the below-1280 drawer from its scrim and restores the opener", async () => {
    setViewport(1024);
    const user = userEvent.setup();
    render(<GroupsPage api={apiFor()} />);

    const trigger = await screen.findByRole("button", { name: "打开重复组 1" });
    await user.click(trigger);
    await user.click(screen.getByTestId("group-detail-scrim"));

    expect(screen.queryByRole("dialog", { name: "重复组详情" })).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  });

  test("one Escape closes nested complete information without closing its parent drawer", async () => {
    setViewport(1024);
    const user = userEvent.setup();
    render(<GroupsPage api={apiFor()} />);

    await user.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    const drawer = screen.getByRole("dialog", { name: "重复组详情" });
    await user.click(await screen.findByRole("button", { name: "查看文件 2 完整信息" }));
    expect(screen.getByRole("dialog", { name: "文件 2 完整信息" })).toBeInTheDocument();
    expect(drawer).toHaveAttribute("inert");
    expect(drawer).toHaveAttribute("aria-hidden", "true");

    await user.keyboard("{Escape}");

    expect(screen.queryByRole("dialog", { name: "文件 2 完整信息" })).not.toBeInTheDocument();
    expect(screen.getByRole("dialog", { name: "重复组详情" })).toBeInTheDocument();
    expect(drawer).not.toHaveAttribute("inert");
    expect(drawer).not.toHaveAttribute("aria-hidden");
  });

  test("keeps the nested modal topmost while a 1024px drawer unmounts at the 1280px breakpoint", async () => {
    const resize = installResponsiveViewport(1024);
    const user = userEvent.setup();
    const view = render(<GroupsPage api={apiFor()} />);

    const trigger = await screen.findByRole("button", { name: "打开重复组 1" });
    await user.click(trigger);
    await user.click(await screen.findByRole("button", { name: "查看文件 2 完整信息" }));
    const modalClose = screen.getByRole("button", { name: "关闭 文件 2 完整信息" });
    expect(modalClose).toHaveFocus();
    expect(document.body.style.overflow).toBe("hidden");

    act(() => resize(1280));

    expect(screen.queryByRole("dialog", { name: "重复组详情", hidden: true })).not.toBeInTheDocument();
    expect(screen.getByRole("dialog", { name: "文件 2 完整信息" })).toBeInTheDocument();
    expect(modalClose).toHaveFocus();
    expect(document.body.style.overflow).toBe("hidden");
    expect(view.container).toHaveAttribute("inert");
    expect(view.container).toHaveAttribute("aria-hidden", "true");

    await user.keyboard("{Escape}");

    expect(screen.queryByRole("dialog", { name: "文件 2 完整信息" })).not.toBeInTheDocument();
    expect(document.body.style.overflow).toBe("");
    expect(view.container).not.toHaveAttribute("inert");
    expect(view.container).not.toHaveAttribute("aria-hidden");
    expect(trigger).toHaveFocus();
  });

  test("keeps portalled drawer controls disabled during the Task 7 execution lock", async () => {
    setViewport(1024);
    const prepare = deferred<DeletePreparation>();
    const api = apiFor({ prepareDelete: () => prepare.promise });
    render(<GroupsPage api={api} />);

    fireEvent.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    fireEvent.click(await screen.findByRole("checkbox", { name: "选择文件 2" }));
    const detailDrawer = screen.getByRole("dialog", { name: "重复组详情" });
    fireEvent.click(
      within(detailDrawer).getByRole("button", { name: "删除已选 1 项" }),
    );
    await screen.findByRole("dialog", { name: "确认删除" });

    expect(within(detailDrawer).getByRole("button", { name: "关闭详情", hidden: true })).toBeDisabled();
    expect(within(detailDrawer).getByRole("checkbox", { name: "选择文件 2", hidden: true })).toBeDisabled();
    await act(async () => prepare.resolve(deletePreparation()));
  });

  test("renders full and compact group-table headers for container-width selection", async () => {
    render(<GroupsPage api={apiFor()} />);

    await screen.findByRole("list", { name: "重复组列表" });
    expect(screen.getByText("组 / 代表文件 / 文件数 / Agent / 总容量 / 可回收 / 创建时间"))
      .toHaveClass("group-table__heading--full");
    expect(screen.getByText("组 / 代表文件 / 文件数 / 总容量 / 可回收"))
      .toHaveClass("group-table__heading--compact");
  });

  test("uses a decorative local placeholder without adding an accessible member image", async () => {
    render(<GroupsPage api={apiFor()} />);

    fireEvent.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    const placeholders = await screen.findAllByTestId("member-thumbnail-placeholder");
    expect(placeholders.length).toBeGreaterThan(0);
    for (const placeholder of placeholders) {
      expect(placeholder).toHaveAttribute("aria-hidden", "true");
    }
    expect(screen.queryByRole("img")).not.toBeInTheDocument();
  });

  test("retries list and same-group detail failures from their own error surfaces", async () => {
    setViewport(1024);
    const listGroups = vi.fn()
      .mockRejectedValueOnce(new Error("列表失败"))
      .mockImplementation(async (query: GroupQuery) => page(query));
    const getGroup = vi.fn()
      .mockRejectedValueOnce(new Error("详情失败"))
      .mockResolvedValueOnce(detail(1));
    const api = apiFor({ groups: listGroups, detail: getGroup });
    render(<GroupsPage api={api} />);

    expect(await screen.findByRole("alert")).toHaveTextContent("列表失败");
    fireEvent.click(screen.getByRole("button", { name: "重试重复组列表" }));
    fireEvent.click(await screen.findByRole("button", { name: "打开重复组 1" }));

    const drawer = await screen.findByRole("dialog", { name: "重复组详情" });
    expect(within(drawer).getByRole("alert")).toHaveTextContent("详情失败");
    expect(within(drawer).getByRole("button", { name: "关闭详情" })).toBeInTheDocument();
    fireEvent.click(within(drawer).getByRole("button", { name: "重试详情" }));
    expect(await within(drawer).findByRole("checkbox", { name: "选择文件 2" })).toBeEnabled();
    expect(getGroup).toHaveBeenCalledTimes(2);
  });

  test("renders explicit empty states for successful empty group and member pages", async () => {
    const emptyGroupsApi = apiFor({
      groups: async query => page(query, { total: 0, groups: [] })
    });
    const first = render(<GroupsPage api={emptyGroupsApi} />);
    expect(await screen.findByText("当前筛选没有重复组。")).toBeInTheDocument();
    first.unmount();

    const emptyMembersApi = apiFor({
      detail: async (id, memberPage, memberSize) => detail(id, {
        memberPage,
        memberSize,
        memberTotal: 0,
        members: []
      })
    });
    render(<GroupsPage api={emptyMembersApi} />);
    fireEvent.click(await screen.findByRole("button", { name: "打开重复组 1" }));
    expect(await screen.findByText("当前成员页没有文件。")).toBeInTheDocument();
  });

  test("uses a labelled segmented button group and does not introduce a nested main landmark", () => {
    const { container } = render(<GroupsPage api={apiFor()} />);
    const kindGroup = screen.getByRole("group", { name: "重复类型" });

    expect(within(kindGroup).getByRole("button", { name: "精确重复" })).toHaveAttribute("aria-pressed", "true");
    expect(within(kindGroup).getByRole("button", { name: "相似图片" })).toHaveAttribute("aria-pressed", "false");
    expect(container.querySelector("main")).toBeNull();
  });
});
