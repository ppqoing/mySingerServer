import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, test, vi } from "vitest";
import type { AppApi, AgentStatus } from "../../api/contracts";
import { AgentsPage } from "./AgentsPage";

function apiFor(agents: AgentStatus[] | (() => Promise<AgentStatus[]>)): AppApi {
  return {
    listAgents: vi.fn(typeof agents === "function" ? agents : async () => agents)
  } as unknown as AppApi;
}

function deferred<T>() {
  let resolve: ((value: T) => void) | undefined;
  const promise = new Promise<T>(resolvePromise => {
    resolve = resolvePromise;
  });
  return { promise, resolve: (value: T) => resolve?.(value) };
}

describe("AgentsPage", () => {
  test("renders online and offline state with literal last-error text in stable order", async () => {
    render(<AgentsPage api={apiFor([
      { machineId: "z-offline", addr: "10.0.0.9", online: false, identityState: "claimed", lastErr: "<img src=x> disconnected" },
      { machineId: "b-online", addr: "10.0.0.2", online: true, identityState: "claimed" },
      { machineId: "a-online", addr: "10.0.0.1", online: true, identityState: "claimed" }
    ])} />);

    await screen.findByText("在线 2 / 共 3");
    const rows = screen.getAllByTestId("agent-row");
    expect(rows.map(row => row.textContent)).toEqual([
      expect.stringContaining("a-online"),
      expect.stringContaining("b-online"),
      expect.stringContaining("z-offline")
    ]);
    expect(screen.getAllByText("在线")).toHaveLength(2);
    expect(screen.getByText("离线")).toBeInTheDocument();
    expect(screen.getByText("<img src=x> disconnected")).toBeInTheDocument();
    expect(screen.getByRole("region", { name: "Agent 状态表" })).toBeInTheDocument();
    expect(document.querySelector("img")).toBeNull();
  });

  test("renders pending and conflict identity states with address-based rows", async () => {
    render(<AgentsPage api={apiFor([
      { machineId: "", addr: "10.0.0.3:9101", online: false, identityState: "pending" },
      { machineId: "", addr: "10.0.0.4:9101", online: false, identityState: "pending" },
      { machineId: "node-" + "a".repeat(64), addr: "10.0.0.5:9101", online: false, identityState: "conflict" }
    ])} />);

    expect((await screen.findAllByText("待识别")).length).toBeGreaterThanOrEqual(2);
    expect(screen.getByText("身份冲突")).toBeInTheDocument();
    expect(screen.getAllByTestId("agent-row")).toHaveLength(3);
  });

  test("shows loading, empty, retry, and preserves earlier agents during a refresh failure", async () => {
    const listAgents = vi.fn()
      .mockRejectedValueOnce(new Error("暂时不可用"))
      .mockResolvedValueOnce([{ machineId: "agent-a", addr: "10.0.0.1", online: true, identityState: "claimed" }])
      .mockRejectedValueOnce(new Error("刷新失败"));
    render(<AgentsPage api={{ listAgents } as unknown as AppApi} />);

    expect(screen.getByRole("status")).toHaveTextContent("正在加载");
    await screen.findByRole("alert");
    await userEvent.click(screen.getByRole("button", { name: "重试" }));
    await screen.findByText("agent-a");
    await userEvent.click(screen.getByRole("button", { name: "刷新 Agent 列表" }));
    await screen.findByRole("alert");
    expect(screen.getByText("agent-a")).toBeInTheDocument();

    render(<AgentsPage api={apiFor([])} />);
    await waitFor(() => expect(screen.getAllByText("当前没有 Agent。").length).toBeGreaterThan(0));
  });

  test("keeps the refresh button enabled during background polling and disables it only for a manual refresh", async () => {
    const first = deferred<AgentStatus[]>();
    const second = deferred<AgentStatus[]>();
    const listAgents = vi.fn()
      .mockImplementationOnce(() => first.promise)
      .mockImplementationOnce(() => second.promise)
      .mockResolvedValue([]);
    const user = userEvent.setup();
    render(<AgentsPage api={{ listAgents } as unknown as AppApi} />);

    // 首屏轮询进行中也不禁用：禁用态只跟随手动点击的这一轮。
    expect(screen.getByRole("button", { name: "刷新 Agent 列表" })).toBeEnabled();
    await act(async () => first.resolve([{ machineId: "agent-a", addr: "10.0.0.1", online: true, identityState: "claimed" }]));
    await screen.findByText("agent-a");

    await user.click(screen.getByRole("button", { name: "刷新 Agent 列表" }));
    expect(screen.getByRole("button", { name: "刷新 Agent 列表" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "刷新 Agent 列表" })).toHaveTextContent("正在刷新…");

    await act(async () => second.resolve([]));
    await waitFor(() => expect(screen.getByRole("button", { name: "刷新 Agent 列表" })).toBeEnabled());
  });

  test("explains what an identity conflict means", async () => {
    render(<AgentsPage api={apiFor([
      { machineId: "node-a", addr: "10.0.0.5:9101", online: false, identityState: "conflict" }
    ])} />);

    expect(await screen.findByText(/同一机器可能被重复部署或配置冲突，请检查 Agent 安装/)).toBeInTheDocument();
  });
});
