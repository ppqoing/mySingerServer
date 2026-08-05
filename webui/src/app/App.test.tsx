import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ComponentProps } from "react";
import type {
  AgentStatus,
  AppApi,
  DeletePreparation,
  DeleteTaskStatus,
  GroupDetail,
  GroupPage,
  GroupQuery
} from "../api/contracts";
import { App } from "./App";

const routeTest = vi.hoisted(() => ({
  injectedApi: { sentinel: "route-test-api" },
  useRealGroups: false
}));

vi.mock("../features/overview/OverviewPage", () => ({
  OverviewPage: ({ api }: { api?: unknown }) => (
    <h1 data-api={api === routeTest.injectedApi ? "injected" : "missing"}>overview-route</h1>
  )
}));

vi.mock("../features/agents/AgentsPage", () => ({
  AgentsPage: ({ api }: { api?: unknown }) => (
    <h1 data-api={api === routeTest.injectedApi ? "injected" : "missing"}>agents-route</h1>
  )
}));

vi.mock("../features/scans/ScansPage", () => ({
  ScansPage: ({ api }: { api?: unknown }) => (
    <h1 data-api={api === routeTest.injectedApi ? "injected" : "missing"}>scans-route</h1>
  )
}));

vi.mock("../features/analysis/AnalysisPage", () => ({
  AnalysisPage: ({ api }: { api?: unknown }) => (
    <h1 data-api={api === routeTest.injectedApi ? "injected" : "missing"}>analysis-route</h1>
  )
}));

vi.mock("../features/groups/GroupsPage", async importOriginal => {
  const actual = await importOriginal<typeof import("../features/groups/GroupsPage")>();
  return {
    GroupsPage: (props: ComponentProps<typeof actual.GroupsPage>) => routeTest.useRealGroups
      ? <actual.GroupsPage {...props} />
      : <h1 data-api={(props.api as unknown) === routeTest.injectedApi ? "injected" : "missing"}>groups-route</h1>
  };
});

vi.mock("../features/deletion/DeleteStatusPanel", () => ({
  DeleteStatusPanel: ({ api, taskId }: { api?: unknown; taskId?: string }) => (
    <h1
      data-api={api === routeTest.injectedApi ? "injected" : "missing"}
      data-task-id={taskId ?? ""}
    >
      audit-route
    </h1>
  )
}));

vi.mock("../features/settings/GUISettingsPage", () => ({
  GUISettingsPage: ({ api }: { api?: unknown }) => (
    <h1 data-api={api === routeTest.injectedApi ? "injected" : "missing"}>settings-route</h1>
  )
}));

const routes = [
  ["/overview", "overview-route"],
  ["/agents", "agents-route"],
  ["/scans", "scans-route"],
  ["/analysis", "analysis-route"],
  ["/groups", "groups-route"],
  ["/audit", "audit-route"],
  ["/settings", "settings-route"]
] as const;

beforeEach(() => {
  window.location.hash = "";
  routeTest.useRealGroups = false;
  Object.defineProperty(window, "matchMedia", {
    configurable: true,
    value: vi.fn().mockImplementation((query: string) => ({
      matches: false,
      media: query,
      onchange: null,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      dispatchEvent: vi.fn()
    }))
  });
  Object.defineProperty(document, "visibilityState", { configurable: true, value: "visible" });
  vi.spyOn(document, "hasFocus").mockReturnValue(true);
  vi.spyOn(HTMLElement.prototype, "offsetHeight", "get").mockReturnValue(512);
  vi.spyOn(HTMLElement.prototype, "offsetWidth", "get").mockReturnValue(800);
});

afterEach(() => {
  vi.restoreAllMocks();
});

function deferred<T>() {
  let resolve: ((value: T) => void) | undefined;
  const promise = new Promise<T>(resolvePromise => {
    resolve = resolvePromise;
  });
  return {
    promise,
    resolve: (value: T) => resolve?.(value)
  };
}

function groupPage(query: GroupQuery): GroupPage {
  return {
    kind: query.kind,
    page: query.page,
    size: query.size,
    total: 1,
    groups: [{
      id: 1,
      kind: "exact",
      memberCount: 2,
      repMachine: "agent-a",
      repPath: "D:\\duplicates\\representative.jpg",
      machines: ["agent-a", "agent-b"],
      createdAt: "2026-07-31T09:00:00Z",
      totalBytes: 2_000,
      wastedBytes: 1_000
    }]
  };
}

function groupDetail(): GroupDetail {
  return {
    id: 1,
    kind: "exact",
    representativeFileId: 1,
    memberTotal: 2,
    memberPage: 1,
    memberSize: 100,
    members: [
      {
        fileId: 1,
        machineId: "agent-a",
        path: "D:\\duplicates\\representative.jpg",
        size: 1_000,
        mtime: 1_722_400_000,
        score: 1
      },
      {
        fileId: 2,
        machineId: "agent-b",
        path: "D:\\duplicates\\copy.jpg",
        size: 1_000,
        mtime: 1_722_400_000,
        score: 1
      }
    ]
  };
}

function deletePreparation(): DeletePreparation {
  return {
    confirmToken: "route-confirm-token",
    expiresInSeconds: 60,
    summary: {
      totalFiles: 1,
      totalBytes: 1_000,
      byMachine: { "agent-b": 1 },
      samples: ["D:\\duplicates\\copy.jpg"]
    }
  };
}

function pendingDeleteStatus(taskId: string): DeleteTaskStatus {
  return {
    taskId,
    mode: "soft",
    total: 1,
    ok: 0,
    failed: 0,
    uncertain: 0,
    pending: 1,
    complete: false,
    stateSyncFailures: 0,
    byMachine: {},
    errorCodes: {},
    problems: []
  };
}

test("renders the seven operational workspace links from the shared shell", () => {
  window.location.hash = "#/overview";
  render(<App />);

  for (const label of ["总览", "Agent", "扫描任务", "一筛分析", "重复组", "删除审计", "GUI 设置"]) {
    expect(screen.getByRole("link", { name: label })).toBeInTheDocument();
  }
});

test.each(routes)("routes #%s to its page and injects the same AppApi instance", (path, heading) => {
  window.location.hash = `#${path}`;

  render(<App api={routeTest.injectedApi as unknown as AppApi} />);

  expect(screen.getByRole("heading", { name: heading })).toHaveAttribute("data-api", "injected");
});

test("redirects the root hash to the overview route", async () => {
  window.location.hash = "#/";

  render(<App />);

  await waitFor(() => expect(window.location.hash).toBe("#/overview"));
  expect(screen.getByRole("heading", { name: "overview-route" })).toBeInTheDocument();
});

test("redirects an unknown hash to the overview route", async () => {
  window.location.hash = "#/not-a-workspace";

  render(<App />);

  await waitFor(() => expect(window.location.hash).toBe("#/overview"));
  expect(screen.getByRole("heading", { name: "overview-route" })).toBeInTheDocument();
});

test("marks the active workspace link as the current page", () => {
  window.location.hash = "#/groups";

  render(<App />);

  expect(screen.getByRole("link", { name: "重复组" })).toHaveAttribute("aria-current", "page");
  expect(screen.getByRole("link", { name: "总览" })).not.toHaveAttribute("aria-current");
});

test("a pending execute survives a groups route round trip and remains the only accepted task", async () => {
  routeTest.useRealGroups = true;
  const execute = deferred<{ taskId: string }>();
  let executeSignal: AbortSignal | undefined;
  const getDeleteStatus = vi.fn(async (taskId: string) => pendingDeleteStatus(taskId));
  const api = {
    listAgents: vi.fn().mockResolvedValue([
      { machineId: "agent-a", addr: "10.0.0.1", online: true, identityState: "claimed" },
      { machineId: "agent-b", addr: "10.0.0.2", online: true, identityState: "claimed" }
    ]),
    listGroups: vi.fn(async (query: GroupQuery) => groupPage(query)),
    getGroup: vi.fn().mockResolvedValue(groupDetail()),
    prepareDelete: vi.fn().mockResolvedValue(deletePreparation()),
    executeDelete: vi.fn((_token: string, _mode: string, signal?: AbortSignal) => {
      executeSignal = signal;
      return execute.promise;
    }),
    getDeleteStatus
  } as unknown as AppApi;
  const user = userEvent.setup();
  window.location.hash = "#/groups";
  render(<App api={api} />);

  await user.click(await screen.findByRole("button", { name: "打开重复组 1" }));
  await user.click(await screen.findByRole("checkbox", { name: "选择文件 2" }));
  await user.click(screen.getByRole("button", { name: "删除已选 1 项" }));
  const dialog = await screen.findByRole("dialog", { name: "确认删除" });
  await user.click(await within(dialog).findByRole("button", { name: "最终确认删除" }));

  expect(api.executeDelete).toHaveBeenCalledTimes(1);
  fireEvent.click(screen.getByRole("link", { name: "删除审计", hidden: true }));
  expect(await screen.findByRole("heading", { name: "audit-route" })).toHaveAttribute("data-task-id", "");
  expect(executeSignal?.aborted).toBe(false);

  await user.click(screen.getByRole("link", { name: "重复组" }));
  const pendingAction = await screen.findByRole("button", { name: "等待删除任务受理" });
  expect(pendingAction).toBeDisabled();
  await user.click(await screen.findByRole("button", { name: "打开重复组 1" }));
  await user.click(await screen.findByRole("checkbox", { name: "选择文件 2" }));
  expect(screen.getByRole("button", { name: "等待删除任务受理" })).toBeDisabled();
  await user.click(screen.getByRole("button", { name: "等待删除任务受理" }));
  expect(screen.queryByRole("dialog", { name: "确认删除" })).not.toBeInTheDocument();
  expect(api.prepareDelete).toHaveBeenCalledTimes(1);
  expect(api.executeDelete).toHaveBeenCalledTimes(1);

  await user.click(screen.getByRole("link", { name: "删除审计", hidden: true }));
  await act(async () => execute.resolve({ taskId: "task-across-route" }));
  expect(await screen.findByRole("heading", { name: "audit-route" }))
    .toHaveAttribute("data-task-id", "task-across-route");
  await user.click(screen.getByRole("link", { name: "重复组" }));
  const restoreTask = await screen.findByRole("button", { name: "查看进行中的删除任务" });
  expect(restoreTask).toBeEnabled();
  await user.click(restoreTask);
  expect(await screen.findByText("任务 ID：task-across-route")).toBeInTheDocument();
  expect(getDeleteStatus).toHaveBeenCalledWith("task-across-route", expect.any(AbortSignal));
  expect(api.prepareDelete).toHaveBeenCalledTimes(1);
  expect(api.executeDelete).toHaveBeenCalledTimes(1);
});

test("a failed terminal task survives a route round trip and restores its retry selection", async () => {
  routeTest.useRealGroups = true;
  const remountedAgents = deferred<AgentStatus[]>();
  const agents: AgentStatus[] = [
    { machineId: "agent-a", addr: "10.0.0.1", online: true, identityState: "claimed" },
    { machineId: "agent-b", addr: "10.0.0.2", online: true, identityState: "claimed" }
  ];
  const agentsWithFailedOwnerOffline = [
    agents[0],
    { ...agents[1], online: false }
  ];
  const terminal: DeleteTaskStatus = {
    ...pendingDeleteStatus("task-route-failed"),
    pending: 0,
    failed: 1,
    complete: true,
    problems: [{
      machineId: "agent-b",
      sequence: 0,
      path: "D:\\duplicates\\copy.jpg",
      errorCode: "E_IN_USE",
      errorMessage: "file in use",
      uncertain: false
    }]
  };
  const getDeleteStatus = vi.fn()
    .mockReturnValueOnce(new Promise<DeleteTaskStatus>(() => undefined))
    .mockResolvedValueOnce(terminal);
  const api = {
    listAgents: vi.fn()
      .mockResolvedValueOnce(agents)
      .mockReturnValueOnce(remountedAgents.promise)
      .mockResolvedValue(agents),
    listGroups: vi.fn(async (query: GroupQuery) => groupPage(query)),
    getGroup: vi.fn().mockResolvedValue(groupDetail()),
    prepareDelete: vi.fn().mockResolvedValue(deletePreparation()),
    executeDelete: vi.fn().mockResolvedValue({ taskId: "task-route-failed" }),
    getDeleteStatus
  } as unknown as AppApi;
  const user = userEvent.setup();
  window.location.hash = "#/groups";
  render(<App api={api} />);

  await user.click(await screen.findByRole("button", { name: "打开重复组 1" }));
  await user.click(await screen.findByRole("checkbox", { name: "选择文件 2" }));
  await user.click(screen.getByRole("button", { name: "删除已选 1 项" }));
  await user.click(await screen.findByRole("button", { name: "最终确认删除" }));
  expect(await screen.findByText("任务 ID：task-route-failed")).toBeInTheDocument();

  fireEvent.click(screen.getByRole("link", { name: "删除审计", hidden: true }));
  expect(await screen.findByRole("heading", { name: "audit-route" }))
    .toHaveAttribute("data-task-id", "task-route-failed");
  await user.click(screen.getByRole("link", { name: "重复组" }));
  await user.click(await screen.findByRole("button", { name: "查看进行中的删除任务" }));
  expect(await screen.findByText("状态：已完成")).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "关闭" }));

  const retryCheckbox = await screen.findByRole("checkbox", { name: "选择文件 2" });
  expect(retryCheckbox).not.toBeChecked();
  await act(async () => remountedAgents.resolve(agentsWithFailedOwnerOffline));
  expect(await screen.findByText("失败项所属 Agent 离线；已保留结果，等待 Agent 恢复后再启用重试。"))
    .toBeInTheDocument();
  expect(screen.getByRole("checkbox", { name: "选择文件 2" })).not.toBeChecked();
  expect(screen.getByRole("button", { name: "删除已选 0 项" })).toBeDisabled();

  await user.click(screen.getByRole("link", { name: "删除审计" }));
  expect(await screen.findByRole("heading", { name: "audit-route" })).toBeInTheDocument();
  await user.click(screen.getByRole("link", { name: "重复组" }));
  await waitFor(() => expect(
    screen.getByRole("checkbox", { name: "选择文件 2" })
  ).toBeChecked());
  expect(screen.getByRole("button", { name: "删除已选 1 项" })).toBeEnabled();
  expect(getDeleteStatus).toHaveBeenCalledTimes(2);
});
