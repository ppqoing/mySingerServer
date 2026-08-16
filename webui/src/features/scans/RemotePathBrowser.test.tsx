import { act, render, screen, waitFor } from "@testing-library/react";
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

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, reject, resolve };
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

test("clears the previous directory before a reopened session root request completes", async () => {
  const reopenedRoot = deferred<FilesystemPage>();
  const browse = vi.fn()
    .mockResolvedValueOnce(page("D:\\Media", [
      { name: "Old", path: "D:\\Media\\Old", kind: "directory", hidden: false, system: false, selectable: true }
    ]))
    .mockImplementationOnce(() => reopenedRoot.promise);
  const onAdd = vi.fn();
  const props = { api: apiFor(browse), machineID: "agent-a", onAdd, onClose: vi.fn() };
  const view = render(<RemotePathBrowser {...props} open />);

  expect(await screen.findByRole("button", { name: "Old" })).toBeVisible();
  view.rerender(<RemotePathBrowser {...props} open={false} />);
  view.rerender(<RemotePathBrowser {...props} open />);

  await waitFor(() => expect(browse).toHaveBeenCalledTimes(2));
  expect(screen.queryByRole("button", { name: "Old" })).not.toBeInTheDocument();
  expect(screen.queryByText("D:\\Media")).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "添加当前目录" })).toBeDisabled();
  expect(onAdd).not.toHaveBeenCalled();
});

test("reloads the current directory exactly once when hidden entries are toggled", async () => {
  const browse = vi.fn()
    .mockResolvedValueOnce(page("D:\\Media", [
      { name: "Public", path: "D:\\Media\\Public", kind: "directory", hidden: false, system: false, selectable: true }
    ]))
    .mockResolvedValueOnce(page("D:\\Media", [
      { name: "Secret", path: "D:\\Media\\Secret", kind: "directory", hidden: true, system: false, selectable: true }
    ]));
  render(<RemotePathBrowser api={apiFor(browse)} machineID="agent-a" onAdd={vi.fn()} onClose={vi.fn()} open />);
  const user = userEvent.setup();

  await screen.findByRole("button", { name: "Public" });
  await user.click(screen.getByLabelText("显示隐藏和系统项目"));
  expect(await screen.findByRole("button", { name: "Secret" })).toBeVisible();

  expect(browse).toHaveBeenCalledTimes(2);
  expect(browse.mock.calls[1]?.[1]).toEqual({
    path: "D:\\Media", showHidden: true, cursor: "", limit: 100
  });
});

test("ignores a cancelled request that resolves after its replacement", async () => {
  const requestA = deferred<FilesystemPage>();
  const requestB = deferred<FilesystemPage>();
  const browse = vi.fn()
    .mockImplementationOnce(() => requestA.promise)
    .mockImplementationOnce(() => requestB.promise);
  render(<RemotePathBrowser api={apiFor(browse)} machineID="agent-a" onAdd={vi.fn()} onClose={vi.fn()} open />);
  const user = userEvent.setup();

  await waitFor(() => expect(browse).toHaveBeenCalledTimes(1));
  await user.click(screen.getByLabelText("显示隐藏和系统项目"));
  await waitFor(() => expect(browse).toHaveBeenCalledTimes(2));
  await act(async () => {
    requestB.resolve(page("D:\\Fresh", [
      { name: "Replacement", path: "D:\\Fresh\\Replacement", kind: "directory", hidden: true, system: false, selectable: true }
    ]));
    await requestB.promise;
  });
  expect(await screen.findByRole("button", { name: "Replacement" })).toBeVisible();

  await act(async () => {
    requestA.resolve(page("D:\\Old", [
      { name: "Old", path: "D:\\Old\\Old", kind: "directory", hidden: false, system: false, selectable: true }
    ]));
    await requestA.promise;
  });

  expect(screen.getByRole("button", { name: "Replacement" })).toBeVisible();
  expect(screen.queryByRole("button", { name: "Old" })).not.toBeInTheDocument();
  expect(screen.queryByRole("status")).not.toBeInTheDocument();
});

test("reopens with the saved hidden preference using one root request", async () => {
  const reopenedRoot = deferred<FilesystemPage>();
  const browse = vi.fn()
    .mockResolvedValueOnce(page("D:\\Media", []))
    .mockResolvedValueOnce(page("D:\\Media", []))
    .mockImplementationOnce(() => reopenedRoot.promise);
  const props = { api: apiFor(browse), machineID: "agent-a", onAdd: vi.fn(), onClose: vi.fn() };
  const view = render(<RemotePathBrowser {...props} open />);
  const user = userEvent.setup();

  await waitFor(() => expect(browse).toHaveBeenCalledTimes(1));
  await user.click(screen.getByLabelText("显示隐藏和系统项目"));
  await waitFor(() => expect(browse).toHaveBeenCalledTimes(2));
  view.rerender(<RemotePathBrowser {...props} open={false} />);
  view.rerender(<RemotePathBrowser {...props} open />);

  await waitFor(() => expect(browse).toHaveBeenCalledTimes(3));
  expect(browse.mock.calls[2]?.[1]).toEqual({
    path: "", showHidden: true, cursor: "", limit: 100
  });
  expect(screen.getByLabelText("显示隐藏和系统项目")).toBeChecked();
});

test("keeps the last successful directory addable after a navigation error", async () => {
  const browse = vi.fn()
    .mockResolvedValueOnce(page("D:\\Media", [{ name: "Photos", path: "D:\\Media\\Photos", kind: "directory", hidden: false, system: false, selectable: true }]))
    .mockRejectedValueOnce(new Error("网络断开"));
  const onAdd = vi.fn();
  render(<RemotePathBrowser api={apiFor(browse)} machineID="agent-a" onAdd={onAdd} onClose={vi.fn()} open />);
  const user = userEvent.setup();

  await user.click(await screen.findByRole("button", { name: "Photos" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("网络断开");
  expect(screen.getByText("D:\\Media")).toBeVisible();
  await user.click(screen.getByRole("button", { name: "添加当前目录" }));
  expect(onAdd).toHaveBeenCalledWith("D:\\Media");
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
