import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { AppApi, GUIConfig } from "../../api/contracts";
import { GUIConfigValidationError } from "../../api/appApi";
import { GUISettingsPage } from "./GUISettingsPage";

const baseConfig: GUIConfig = {
  listenAddr: "127.0.0.1:18080",
  pgDsn: "postgres://user:secret@127.0.0.1:5432/dedup",
  agents: [
    { addr: "192.168.1.10:9101" },
    { addr: "192.168.1.11:9101" }
  ],
  heartbeatS: 15,
  firstScreen: {
    hammingMax: 31,
    aspectTolerance: 0.1,
    videoDurationWindowMs: 2000,
    imageQualityMin: 50,
    readPageSize: 50000,
    groupInsertBatch: 1000,
    shaResolveChunk: 10000
  },
  phase2: {
    phashPassT2: 0.8,
    phashPartThreshold: 10,
    sobelT3: 0.85,
    videoFrames: 6,
    videoAvgT4: 0.8,
    videoMinPassed: 4,
    videoMinValid: 4,
    videoFileTimeoutS: 120,
    videoFrameCommandTimeoutS: 20,
    imageFileTimeoutS: 30,
    taskShardSize: 5000,
    autoDispatch: true
  }
};

function copyConfig(config: GUIConfig = baseConfig): GUIConfig {
  return JSON.parse(JSON.stringify(config)) as GUIConfig;
}

function apiFor(overrides: Partial<AppApi> = {}): AppApi {
  return {
    loadGUIConfig: vi.fn().mockResolvedValue({ config: copyConfig(), restartRequired: false }),
    saveGUIConfig: vi.fn().mockResolvedValue({ saved: true, restartRequired: true }),
    ...overrides
  } as unknown as AppApi;
}

test("loads and renders every GUI configuration section", async () => {
  render(<GUISettingsPage api={apiFor()} />);

  expect(await screen.findByDisplayValue("127.0.0.1:18080")).toBeInTheDocument();
  for (const heading of ["基本设置", "PostgreSQL", "Agent", "一筛参数", "二筛参数"]) {
    expect(screen.getByRole("heading", { name: heading })).toBeInTheDocument();
  }
  expect(screen.getByLabelText("监听地址")).toHaveValue("127.0.0.1:18080");
  expect(screen.queryByLabelText("机器标识 1")).not.toBeInTheDocument();
  expect(screen.getByLabelText("一筛汉明距离上限")).toHaveValue(31);
  expect(screen.getByLabelText("二筛任务分片大小")).toHaveValue(5000);
  expect(screen.getByLabelText("自动分发二筛任务")).toBeChecked();
});

test("keeps the DSN hidden until the user chooses to show it", async () => {
  const user = userEvent.setup();
  render(<GUISettingsPage api={apiFor()} />);

  const dsn = await screen.findByLabelText("PostgreSQL DSN");
  expect(dsn).toHaveAttribute("type", "password");
  await user.click(screen.getByRole("button", { name: "显示 DSN" }));
  expect(dsn).toHaveAttribute("type", "text");
  expect(dsn).toHaveValue(baseConfig.pgDsn);
  await user.click(screen.getByRole("button", { name: "隐藏 DSN" }));
  expect(dsn).toHaveAttribute("type", "password");
});

test("adds reorders and removes agents in the configuration draft", async () => {
  const user = userEvent.setup();
  render(<GUISettingsPage api={apiFor()} />);
  await screen.findByLabelText("Agent 地址 1");

  await user.click(screen.getByRole("button", { name: "添加 Agent" }));
  await user.type(screen.getByLabelText("Agent 地址 3"), "192.168.1.12:9101");
  await user.click(screen.getByRole("button", { name: "上移 Agent 3" }));
  expect(screen.getAllByLabelText(/Agent 地址 \d/).map(input => (input as HTMLInputElement).value))
    .toEqual(["192.168.1.10:9101", "192.168.1.12:9101", "192.168.1.11:9101"]);
  expect(screen.getByText("有未保存更改")).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "删除 Agent 2" }));
  expect(screen.getAllByLabelText(/Agent 地址 \d/).map(input => (input as HTMLInputElement).value))
    .toEqual(["192.168.1.10:9101", "192.168.1.11:9101"]);
  expect(screen.queryByText("有未保存更改")).not.toBeInTheDocument();
});

test("keeps edited values and binds indexed field errors after save failure", async () => {
  const user = userEvent.setup();
  const saveGUIConfig = vi.fn().mockRejectedValue(new GUIConfigValidationError([{
    field: "agents[1].addr",
    code: "duplicate",
    message: "Agent 地址不能重复"
  }]));
  render(<GUISettingsPage api={apiFor({ saveGUIConfig })} />);
	const address = await screen.findByLabelText("Agent 地址 2");
	await user.clear(address);
	await user.type(address, "192.168.1.10:9101");
  await user.click(screen.getByRole("button", { name: "保存配置" }));

  expect(await screen.findByText("Agent 地址不能重复")).toHaveAttribute("role", "alert");
  expect(address).toHaveValue("192.168.1.10:9101");
  expect(address).toHaveAttribute("aria-invalid", "true");
  expect(saveGUIConfig).toHaveBeenCalledTimes(1);
});

test("reloads disk configuration and clears dirty state", async () => {
  const reloaded = copyConfig();
  reloaded.listenAddr = "127.0.0.1:28080";
  const loadGUIConfig = vi.fn()
    .mockResolvedValueOnce({ config: copyConfig(), restartRequired: false })
    .mockResolvedValueOnce({ config: reloaded, restartRequired: true });
  const user = userEvent.setup();
  render(<GUISettingsPage api={apiFor({ loadGUIConfig })} />);
  const listen = await screen.findByLabelText("监听地址");
  await user.clear(listen);
  await user.type(listen, "127.0.0.1:38080");
  expect(screen.getByText("有未保存更改")).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: "重新加载" }));
  await waitFor(() => expect(screen.getByLabelText("监听地址")).toHaveValue("127.0.0.1:28080"));
  expect(screen.queryByText("有未保存更改")).not.toBeInTheDocument();
  expect(loadGUIConfig).toHaveBeenCalledTimes(2);
});

test("shows the manual restart message after a changed save", async () => {
  const saveGUIConfig = vi.fn().mockResolvedValue({ saved: true, restartRequired: true });
  const user = userEvent.setup();
  render(<GUISettingsPage api={apiFor({ saveGUIConfig })} />);
  const listen = await screen.findByLabelText("监听地址");
  await user.clear(listen);
  await user.type(listen, "127.0.0.1:28080");
  await user.click(screen.getByRole("button", { name: "保存配置" }));

  expect(await screen.findByText("配置已保存，请手动重启 GUI 后生效")).toBeInTheDocument();
  expect(saveGUIConfig).toHaveBeenCalledWith(
    expect.objectContaining({ listenAddr: "127.0.0.1:28080" }),
    expect.any(AbortSignal)
  );
  expect(screen.queryByText("有未保存更改")).not.toBeInTheDocument();
});

test("shows no-restart message when saved configuration matches runtime", async () => {
  const saveGUIConfig = vi.fn().mockResolvedValue({ saved: false, restartRequired: false });
  const user = userEvent.setup();
  render(<GUISettingsPage api={apiFor({ saveGUIConfig })} />);
  await screen.findByLabelText("监听地址");
  await user.click(screen.getByRole("button", { name: "保存配置" }));

  expect(await screen.findByText("配置已保存，当前无需重启")).toBeInTheDocument();
});
