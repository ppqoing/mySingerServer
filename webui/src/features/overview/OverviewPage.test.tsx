import { act, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { AppApi } from "../../api/contracts";
import { OverviewPage } from "./OverviewPage";

test("shows database degradation and does not start business polling", async () => {
  const api = {
    getRuntimeStatus: vi.fn().mockResolvedValue({
      databaseState: "error",
      databaseErrorCode: "database_unavailable",
      agents: [],
      restarting: false,
      recoveryURL: ""
    }),
    listAgents: vi.fn(),
    listTasks: vi.fn(),
    getAnalysisStatus: vi.fn()
  } as unknown as AppApi;

  render(<MemoryRouter><OverviewPage api={api} /></MemoryRouter>);

  expect(await screen.findByText("PostgreSQL 未连接")).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "打开 GUI 设置" })).toHaveAttribute("href", "/settings");
  await waitFor(() => expect(api.getRuntimeStatus).toHaveBeenCalledTimes(1));
  expect(api.listAgents).not.toHaveBeenCalled();
  expect(api.listTasks).not.toHaveBeenCalled();
  expect(api.getAnalysisStatus).not.toHaveBeenCalled();
});

test("starts business polling only after a connected runtime status", async () => {
  const api = {
    getRuntimeStatus: vi.fn().mockResolvedValue({ databaseState: "connected", databaseErrorCode: "", agents: [], restarting: false, recoveryURL: "" }),
    listAgents: vi.fn().mockResolvedValue([]), listTasks: vi.fn().mockResolvedValue([]),
    getAnalysisStatus: vi.fn().mockResolvedValue({ running: false, last: null, lastErr: "" })
  } as unknown as AppApi;
  render(<MemoryRouter><OverviewPage api={api} /></MemoryRouter>);

  await waitFor(() => expect(api.listAgents).toHaveBeenCalledTimes(1));
  expect(api.listTasks).toHaveBeenCalledTimes(1);
  expect(api.getAnalysisStatus).toHaveBeenCalledTimes(1);
});

test("shows the connecting state without starting business polling", async () => {
  const api = {
    getRuntimeStatus: vi.fn().mockResolvedValue({ databaseState: "connecting", databaseErrorCode: "", agents: [], restarting: false, recoveryURL: "" }),
    listAgents: vi.fn(), listTasks: vi.fn(), getAnalysisStatus: vi.fn()
  } as unknown as AppApi;
  render(<MemoryRouter><OverviewPage api={api} /></MemoryRouter>);

  expect(await screen.findByText("正在连接 PostgreSQL…")).toBeInTheDocument();
  expect(api.listAgents).not.toHaveBeenCalled();
});

test("aborts business polling when runtime changes from connected to error", async () => {
  vi.useFakeTimers();
  vi.spyOn(document, "hasFocus").mockReturnValue(true);
  let agentsSignal: AbortSignal | undefined;
  const api = {
    getRuntimeStatus: vi.fn()
      .mockResolvedValueOnce({ databaseState: "connected", databaseErrorCode: "", agents: [], restarting: false, recoveryURL: "" })
      .mockResolvedValueOnce({ databaseState: "error", databaseErrorCode: "database_unavailable", agents: [], restarting: false, recoveryURL: "" }),
    listAgents: vi.fn((signal?: AbortSignal) => new Promise(() => { agentsSignal = signal; })),
    listTasks: vi.fn().mockResolvedValue([]), getAnalysisStatus: vi.fn().mockResolvedValue({ running: false, last: null, lastErr: "" })
  } as unknown as AppApi;
  render(<MemoryRouter><OverviewPage api={api} /></MemoryRouter>);

  await act(async () => {});
  await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });
  expect(agentsSignal?.aborted).toBe(true);
  vi.useRealTimers();
});
