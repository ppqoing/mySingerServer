import { render, screen, waitFor } from "@testing-library/react";
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
