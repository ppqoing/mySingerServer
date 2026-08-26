import { act, fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, test, vi } from "vitest";
import { ApiError } from "../../api/client";
import type { AnalysisStatus, AppApi } from "../../api/contracts";
import { AnalysisPage } from "./AnalysisPage";

const completed: AnalysisStatus = {
  running: false,
  last: {
    filesScanned: 12, exactGroups: 2, exactMembers: 4, imageFeatures: 6, imagePairs: 5,
    videoFeatures: 3, videoPairs: 2, badRows: 1, skippedPairs: 1, groupsWritten: 3,
    membersWritten: 6, stageElapsedMs: { exact: 1500 }, heapAllocBytes: 12_582_912
  },
  lastErr: ""
};

function apiFor(overrides: Partial<AppApi> = {}): AppApi {
  return {
    getAnalysisStatus: vi.fn().mockResolvedValue(completed),
    runAnalysis: vi.fn().mockResolvedValue(undefined),
    cancelAnalysis: vi.fn().mockResolvedValue(undefined),
    ...overrides
  } as unknown as AppApi;
}

function renderPage(api: AppApi) {
  return render(<MemoryRouter><AnalysisPage api={api} /></MemoryRouter>);
}

function pendingRun() {
  let resolve!: () => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<void>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, reject, resolve };
}

function pendingStatus() {
  let resolve!: (status: AnalysisStatus) => void;
  const promise = new Promise<AnalysisStatus>(resolvePromise => {
    resolve = resolvePromise;
  });
  return { promise, resolve };
}

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("AnalysisPage", () => {
  test("shows an unknown state before the first status response", () => {
    const api = apiFor({
      getAnalysisStatus: vi.fn().mockReturnValue(new Promise<AnalysisStatus>(() => undefined))
    });
    renderPage(api);

    expect(screen.getByText((_, node) => node?.textContent === "状态：未知")).toBeInTheDocument();
  });

  test("HTTP 409 refreshes into the existing running analysis and resumes polling", async () => {
    vi.useFakeTimers();
    vi.spyOn(document, "hasFocus").mockReturnValue(true);
    const getAnalysisStatus = vi.fn()
      .mockResolvedValueOnce(completed)
      .mockResolvedValueOnce({ ...completed, running: true })
      .mockResolvedValueOnce(completed);
    const api = apiFor({
      getAnalysisStatus,
      runAnalysis: vi.fn().mockRejectedValue(new ApiError(409, "already running", false))
    });
    renderPage(api);
    await act(async () => {});

    fireEvent.click(screen.getByRole("button", { name: "开始一筛分析" }));
    await act(async () => {});

    expect(screen.getByRole("alert")).toHaveTextContent("已有分析正在运行");
    expect(getAnalysisStatus).toHaveBeenCalledTimes(2);
    expect(screen.getByText((_, node) => node?.textContent === "状态：运行中")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "开始一筛分析" })).toBeDisabled();

    await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });
    expect(getAnalysisStatus).toHaveBeenCalledTimes(3);
    expect(screen.getByText((_, node) => node?.textContent === "状态：空闲")).toBeInTheDocument();
    // 状态回到空闲后 409 提示自动清除
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  test("disables the run button while running and retains metrics with a last error", async () => {
    const api = apiFor({ getAnalysisStatus: vi.fn().mockResolvedValue({ ...completed, running: true, lastErr: "写入部分失败" }) });
    renderPage(api);
    await screen.findByText((_, node) => node?.textContent === "状态：运行中");
    expect(screen.getByRole("button", { name: "开始一筛分析" })).toBeDisabled();
    expect(screen.getByText("12")).toBeInTheDocument();
    expect(screen.getByText("写入部分失败")).toBeInTheDocument();
  });

  test("humanizes heap metrics and renders stage elapsed in seconds", async () => {
    renderPage(apiFor());

    expect(await screen.findByText("堆内存")).toBeInTheDocument();
    expect(screen.getByText("12.0 MB")).toBeInTheDocument();
    expect(screen.getByText("1.5 秒")).toBeInTheDocument();
    expect(screen.queryByText("堆内存（字节）")).not.toBeInTheDocument();
  });

  test("links to groups when the last run wrote duplicate groups", async () => {
    renderPage(apiFor());

    const link = await screen.findByRole("link", { name: "检出 3 个重复组，前往查看 →" });
    expect(link).toHaveAttribute("href", "/groups");
  });

  test("keeps the groups shortcut and export hidden without a last run", async () => {
    renderPage(apiFor({ getAnalysisStatus: vi.fn().mockResolvedValue({ running: false, last: null, lastErr: "" }) }));

    expect(await screen.findByRole("button", { name: "导出指标 JSON" })).toBeDisabled();
    expect(screen.queryByRole("link", { name: /前往查看/ })).not.toBeInTheDocument();
  });

  test("exports the last metrics as a JSON download", async () => {
    const createObjectURL = vi.fn().mockReturnValue("blob:metrics");
    const revokeObjectURL = vi.fn();
    const originalCreate = URL.createObjectURL;
    const originalRevoke = URL.revokeObjectURL;
    Object.defineProperty(URL, "createObjectURL", { configurable: true, writable: true, value: createObjectURL });
    Object.defineProperty(URL, "revokeObjectURL", { configurable: true, writable: true, value: revokeObjectURL });
    const clicks: Array<{ download: string; href: string }> = [];
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
      clicks.push({ download: this.download, href: this.href });
    });
    try {
      renderPage(apiFor());
      const user = userEvent.setup();
      await user.click(await screen.findByRole("button", { name: "导出指标 JSON" }));

      expect(createObjectURL).toHaveBeenCalledTimes(1);
      expect(createObjectURL.mock.calls[0]?.[0]).toBeInstanceOf(Blob);
      expect(clicks).toEqual([{ download: "firstscreen-analysis-metrics.json", href: "blob:metrics" }]);
      expect(revokeObjectURL).toHaveBeenCalledWith("blob:metrics");
    } finally {
      clickSpy.mockRestore();
      Object.defineProperty(URL, "createObjectURL", { configurable: true, writable: true, value: originalCreate });
      Object.defineProperty(URL, "revokeObjectURL", { configurable: true, writable: true, value: originalRevoke });
    }
  });

  test("translates backend error codes surfaced in lastErr", async () => {
    renderPage(apiFor({
      getAnalysisStatus: vi.fn().mockResolvedValue({ ...completed, lastErr: "postgres_unreachable" })
    }));

    expect(await screen.findByRole("alert")).toHaveTextContent("无法连接数据库：请检查网络与数据库服务状态。");
  });

  test("starts polling after acceptance and stops after a non-running status", async () => {
    vi.useFakeTimers();
    vi.spyOn(document, "hasFocus").mockReturnValue(true);
    const getAnalysisStatus = vi.fn()
      .mockResolvedValueOnce({ ...completed, running: false })
      .mockResolvedValueOnce({ ...completed, running: true })
      .mockResolvedValueOnce(completed);
    const api = apiFor({ getAnalysisStatus, runAnalysis: vi.fn().mockResolvedValue(undefined) });
    renderPage(api);
    await act(async () => {});
    fireEvent.click(screen.getByRole("button", { name: "开始一筛分析" }));
    await act(async () => {});
    await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });
    expect(getAnalysisStatus).toHaveBeenCalledTimes(3);
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(getAnalysisStatus).toHaveBeenCalledTimes(3);
  });

  test("keeps start disabled after acceptance until the refreshed status settles", async () => {
    const refreshed = pendingStatus();
    const getAnalysisStatus = vi.fn()
      .mockResolvedValueOnce(completed)
      .mockReturnValueOnce(refreshed.promise);
    const runAnalysis = vi.fn().mockResolvedValue(undefined);
    renderPage(apiFor({ getAnalysisStatus, runAnalysis }));
    await screen.findByText((_, node) => node?.textContent === "状态：空闲");

    fireEvent.click(screen.getByRole("button", { name: "开始一筛分析" }));
    await act(async () => {});

    expect(runAnalysis).toHaveBeenCalledTimes(1);
    expect(getAnalysisStatus).toHaveBeenCalledTimes(2);
    expect(screen.getByRole("button", { name: "开始一筛分析" })).toBeDisabled();
    fireEvent.click(screen.getByRole("button", { name: "开始一筛分析" }));
    expect(runAnalysis).toHaveBeenCalledTimes(1);

    await act(async () => refreshed.resolve({ ...completed, running: true }));
    expect(screen.getByText((_, node) => node?.textContent === "状态：运行中")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "开始一筛分析" })).toBeDisabled();
  });

  test("keeps start disabled when the first status refresh after acceptance fails", async () => {
    const getAnalysisStatus = vi.fn()
      .mockResolvedValueOnce(completed)
      .mockRejectedValueOnce(new ApiError(503, "status unavailable", true))
      .mockResolvedValueOnce({ ...completed, running: true });
    const runAnalysis = vi.fn().mockResolvedValue(undefined);
    renderPage(apiFor({ getAnalysisStatus, runAnalysis }));
    await screen.findByText((_, node) => node?.textContent === "状态：空闲");

    fireEvent.click(screen.getByRole("button", { name: "开始一筛分析" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("status unavailable");
    expect(screen.getByRole("button", { name: "开始一筛分析" })).toBeDisabled();

    fireEvent.click(screen.getByRole("button", { name: "刷新状态" }));
    await screen.findByText((_, node) => node?.textContent === "状态：运行中");
    expect(runAnalysis).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("button", { name: "开始一筛分析" })).toBeDisabled();
  });

  test("aborts an in-flight run on unmount and ignores a late resolution", async () => {
    const pending = pendingRun();
    const runAnalysis = vi.fn().mockReturnValue(pending.promise);
    const getAnalysisStatus = vi.fn().mockResolvedValue(completed);
    const { unmount } = renderPage(apiFor({ getAnalysisStatus, runAnalysis }));
    await screen.findByText((_, node) => node?.textContent === "状态：空闲");

    fireEvent.click(screen.getByRole("button", { name: "开始一筛分析" }));
    const signal = runAnalysis.mock.calls[0]?.[0] as AbortSignal;
    expect(signal).toBeInstanceOf(AbortSignal);
    unmount();
    expect(signal.aborted).toBe(true);
    await act(async () => pending.resolve());

    expect(getAnalysisStatus).toHaveBeenCalledTimes(1);
  });

  test("ignores an abort rejection after unmount", async () => {
    const pending = pendingRun();
    const runAnalysis = vi.fn().mockReturnValue(pending.promise);
    const getAnalysisStatus = vi.fn().mockResolvedValue(completed);
    const { unmount } = renderPage(apiFor({ getAnalysisStatus, runAnalysis }));
    await screen.findByText((_, node) => node?.textContent === "状态：空闲");

    fireEvent.click(screen.getByRole("button", { name: "开始一筛分析" }));
    unmount();
    await act(async () => pending.reject(new DOMException("aborted", "AbortError")));

    expect(getAnalysisStatus).toHaveBeenCalledTimes(1);
  });

  test("hides the cancel button when idle", async () => {
    renderPage(apiFor());

    await screen.findByText((_, node) => node?.textContent === "状态：空闲");
    expect(screen.queryByRole("button", { name: "取消分析" })).not.toBeInTheDocument();
  });

  test("cancels a running analysis and keeps the button disabled until idle", async () => {
    let release!: () => void;
    const cancelAnalysis = vi.fn().mockReturnValue(new Promise<void>(resolve => { release = resolve; }));
    const getAnalysisStatus = vi.fn()
      .mockResolvedValueOnce({ ...completed, running: true })
      .mockResolvedValue(completed);
    renderPage(apiFor({ cancelAnalysis, getAnalysisStatus }));
    const user = userEvent.setup();

    await user.click(await screen.findByRole("button", { name: "取消分析" }));
    expect(cancelAnalysis).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("button", { name: "正在取消…" })).toBeDisabled();

    await act(async () => release());
    await screen.findByText((_, node) => node?.textContent === "状态：空闲");
    expect(screen.queryByRole("button", { name: "取消分析" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "开始一筛分析" })).toBeEnabled();
  });

  test("explains when no analysis is running (409)", async () => {
    const cancelAnalysis = vi.fn().mockRejectedValue(new ApiError(409, "firstscreen not running", false));
    renderPage(apiFor({
      cancelAnalysis,
      getAnalysisStatus: vi.fn().mockResolvedValue({ ...completed, running: true })
    }));
    const user = userEvent.setup();

    await user.click(await screen.findByRole("button", { name: "取消分析" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("没有正在运行的分析。");
    expect(await screen.findByRole("button", { name: "取消分析" })).toBeEnabled();
  });

  test("shows 已取消 after a cancelled run", async () => {
    renderPage(apiFor({
      getAnalysisStatus: vi.fn().mockResolvedValue({ ...completed, lastErr: "已取消" })
    }));

    expect(await screen.findByRole("alert")).toHaveTextContent("已取消");
  });
});
