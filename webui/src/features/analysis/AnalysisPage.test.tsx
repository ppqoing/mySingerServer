import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";
import { ApiError } from "../../api/client";
import type { AnalysisStatus, AppApi } from "../../api/contracts";
import { AnalysisPage } from "./AnalysisPage";

const completed: AnalysisStatus = {
  running: false,
  last: {
    filesScanned: 12, exactGroups: 2, exactMembers: 4, imageFeatures: 6, imagePairs: 5,
    videoFeatures: 3, videoPairs: 2, badRows: 1, skippedPairs: 1, groupsWritten: 3,
    membersWritten: 6, stageElapsedMs: { exact: 23 }, heapAllocBytes: 1024
  },
  lastErr: ""
};

function apiFor(overrides: Partial<AppApi> = {}): AppApi {
  return {
    getAnalysisStatus: vi.fn().mockResolvedValue(completed),
    runAnalysis: vi.fn().mockResolvedValue(undefined),
    ...overrides
  } as unknown as AppApi;
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
    render(<AnalysisPage api={api} />);

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
    render(<AnalysisPage api={api} />);
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
  });

  test("disables the run button while running and retains metrics with a last error", async () => {
    const api = apiFor({ getAnalysisStatus: vi.fn().mockResolvedValue({ ...completed, running: true, lastErr: "写入部分失败" }) });
    render(<AnalysisPage api={api} />);
    await screen.findByText((_, node) => node?.textContent === "状态：运行中");
    expect(screen.getByRole("button", { name: "开始一筛分析" })).toBeDisabled();
    expect(screen.getByText("12")).toBeInTheDocument();
    expect(screen.getByText("写入部分失败")).toBeInTheDocument();
  });

  test("starts polling after acceptance and stops after a non-running status", async () => {
    vi.useFakeTimers();
    vi.spyOn(document, "hasFocus").mockReturnValue(true);
    const getAnalysisStatus = vi.fn()
      .mockResolvedValueOnce({ ...completed, running: false })
      .mockResolvedValueOnce({ ...completed, running: true })
      .mockResolvedValueOnce(completed);
    const api = apiFor({ getAnalysisStatus, runAnalysis: vi.fn().mockResolvedValue(undefined) });
    render(<AnalysisPage api={api} />);
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
    render(<AnalysisPage api={apiFor({ getAnalysisStatus, runAnalysis })} />);
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
    render(<AnalysisPage api={apiFor({ getAnalysisStatus, runAnalysis })} />);
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
    const { unmount } = render(<AnalysisPage api={apiFor({ getAnalysisStatus, runAnalysis })} />);
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
    const { unmount } = render(<AnalysisPage api={apiFor({ getAnalysisStatus, runAnalysis })} />);
    await screen.findByText((_, node) => node?.textContent === "状态：空闲");

    fireEvent.click(screen.getByRole("button", { name: "开始一筛分析" }));
    unmount();
    await act(async () => pending.reject(new DOMException("aborted", "AbortError")));

    expect(getAnalysisStatus).toHaveBeenCalledTimes(1);
  });
});
