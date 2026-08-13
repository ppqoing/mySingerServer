import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, test, vi } from "vitest";
import type { AppApi, FilesystemPage } from "../../api/contracts";
import { RemotePathBrowser } from "./RemotePathBrowser";

const drives: FilesystemPage = {
  currentPath: "", parentPath: "", nextCursor: "",
  entries: [{ name: "D:", path: "D:\\", kind: "drive", hidden: false, system: false, selectable: true }]
};

function page(currentPath: string, entries: FilesystemPage["entries"], nextCursor = ""): FilesystemPage {
  return { currentPath, parentPath: "D:\\", entries, nextCursor };
}

function apiFor(browse: AppApi["browseAgentFilesystem"]): AppApi {
  return { browseAgentFilesystem: browse } as AppApi;
}

afterEach(() => vi.restoreAllMocks());

test("shows drive entries and navigates directories through breadcrumbs", async () => {
  const browse = vi.fn()
    .mockResolvedValueOnce(drives)
    .mockResolvedValueOnce(page("D:\\", [{ name: "Media", path: "D:\\Media", kind: "directory", hidden: false, system: false, selectable: true }]))
    .mockResolvedValueOnce(page("D:\\Media", []));
  render(<RemotePathBrowser api={apiFor(browse)} machineID="agent-a" onAdd={vi.fn()} onClose={vi.fn()} open />);
  const user = userEvent.setup();

  await user.click(await screen.findByRole("button", { name: "D:" }));
  await user.click(await screen.findByRole("button", { name: "Media" }));
  expect(await screen.findByRole("button", { name: "D:" })).toBeVisible();
  expect(screen.getByText("D:\\Media")).toBeVisible();
});

test("disables files while allowing a selected directory to be added", async () => {
  const browse = vi.fn().mockResolvedValue(page("D:\\Media", [
    { name: "Photos", path: "D:\\Media\\Photos", kind: "directory", hidden: false, system: false, selectable: true },
    { name: "cover.jpg", path: "D:\\Media\\cover.jpg", kind: "file", hidden: false, system: false, selectable: false }
  ]));
  const onAdd = vi.fn();
  render(<RemotePathBrowser api={apiFor(browse)} machineID="agent-a" onAdd={onAdd} onClose={vi.fn()} open />);
  const user = userEvent.setup();

  const file = await screen.findByRole("button", { name: "cover.jpg" });
  expect(file).toBeDisabled();
  expect(file).toHaveAttribute("aria-disabled", "true");
  await user.click(screen.getByRole("button", { name: "Photos" }));
  await user.click(screen.getByRole("button", { name: "添加当前目录" }));
  expect(onAdd).toHaveBeenCalledWith("D:\\Media\\Photos");
});

test("reloads hidden entries and appends the next page", async () => {
  const browse = vi.fn()
    .mockResolvedValueOnce(page("D:\\Media", [{ name: "Public", path: "D:\\Media\\Public", kind: "directory", hidden: false, system: false, selectable: true }], "cursor-2"))
    .mockResolvedValueOnce(page("D:\\Media", [{ name: "Secret", path: "D:\\Media\\Secret", kind: "directory", hidden: true, system: false, selectable: true }], "cursor-3"))
    .mockResolvedValueOnce(page("D:\\Media", [{ name: "More", path: "D:\\Media\\More", kind: "directory", hidden: false, system: false, selectable: true }]));
  render(<RemotePathBrowser api={apiFor(browse)} machineID="agent-a" onAdd={vi.fn()} onClose={vi.fn()} open />);
  const user = userEvent.setup();

  await screen.findByRole("button", { name: "Public" });
  await user.click(screen.getByLabelText("显示隐藏和系统项目"));
  await screen.findByRole("button", { name: "Secret" });
  expect(browse).toHaveBeenLastCalledWith("agent-a", expect.objectContaining({ showHidden: true }), expect.any(AbortSignal));
  await user.click(screen.getByRole("button", { name: "加载更多" }));
  expect(await screen.findByRole("button", { name: "More" })).toBeVisible();
});

test("keeps the selected directory after a browse error", async () => {
  const browse = vi.fn()
    .mockResolvedValueOnce(page("D:\\Media", [{ name: "Photos", path: "D:\\Media\\Photos", kind: "directory", hidden: false, system: false, selectable: true }]))
    .mockRejectedValueOnce(new Error("网络断开"));
  render(<RemotePathBrowser api={apiFor(browse)} machineID="agent-a" onAdd={vi.fn()} onClose={vi.fn()} open />);
  const user = userEvent.setup();

  await user.click(await screen.findByRole("button", { name: "Photos" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("网络断开");
  expect(screen.getByText("D:\\Media\\Photos")).toBeVisible();
});

test("aborts the active browse request when the dialog closes", async () => {
  let signal: AbortSignal | undefined;
  const browse = vi.fn().mockImplementation((_machineID, _input, requestSignal: AbortSignal) => {
    signal = requestSignal;
    return new Promise<FilesystemPage>(() => {});
  });
  const view = render(<RemotePathBrowser api={apiFor(browse)} machineID="agent-a" onAdd={vi.fn()} onClose={vi.fn()} open />);

  await waitFor(() => expect(signal).toBeDefined());
  view.rerender(<RemotePathBrowser api={apiFor(browse)} machineID="agent-a" onAdd={vi.fn()} onClose={vi.fn()} open={false} />);
  expect(signal?.aborted).toBe(true);
});
