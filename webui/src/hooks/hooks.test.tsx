import { act, renderHook, waitFor } from "@testing-library/react";
import { ApiError } from "../api/client";
import type { AppApi, GroupPage, GroupQuery } from "../api/contracts";
import { usePagedGroups } from "./usePagedGroups";
import { usePolling } from "./usePolling";
import { useSelection } from "./useSelection";

function page(pageNumber: number): GroupPage {
  return { kind: "exact", page: pageNumber, size: 100, total: 6, groups: [] };
}

test("keeps explicit selection sorted, unique, and outside protected IDs", () => {
  const { result, rerender } = renderHook(
    ({ scopeKey, protectedIds }) => useSelection(scopeKey, protectedIds),
    { initialProps: { scopeKey: "group-1", protectedIds: new Set([2]) } }
  );

  act(() => {
    result.current.toggle(9);
    result.current.toggle(3);
    result.current.toggle(9);
    result.current.toggle(2);
  });
  expect(result.current.selectedIds).toEqual([3]);
  expect(result.current.selectedSet.has(2)).toBe(false);

  rerender({ scopeKey: "group-1", protectedIds: new Set([3]) });
  expect(result.current.selectedIds).toEqual([]);

  act(() => result.current.toggle(8));
  rerender({ scopeKey: "group-2", protectedIds: new Set() });
  expect(result.current.selectedIds).toEqual([]);
});

test("polls every two seconds when focused and stops after a terminal result", async () => {
  vi.useFakeTimers();
  vi.spyOn(document, "hasFocus").mockReturnValue(true);
  const request = vi.fn()
    .mockResolvedValueOnce({ complete: false })
    .mockResolvedValueOnce({ complete: true });

  renderHook(() => usePolling<{ complete: boolean }>(request, { isTerminal: value => value.complete }));

  await act(async () => {});
  await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });
  await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });

  expect(request).toHaveBeenCalledTimes(2);
  vi.useRealTimers();
});

test("ignores environment events after terminal polling until dependencies change", async () => {
  vi.useFakeTimers();
  vi.spyOn(document, "hasFocus").mockReturnValue(true);
  Object.defineProperty(document, "visibilityState", { configurable: true, value: "visible" });
  const request = vi.fn().mockResolvedValue({ complete: true });
  const { rerender } = renderHook(
    ({ scope }) => usePolling<{ complete: boolean }>(request, {
      dependencies: [scope],
      isTerminal: value => value.complete
    }),
    { initialProps: { scope: "old" } }
  );
  await act(async () => {});

  await act(async () => { window.dispatchEvent(new Event("focus")); });
  await act(async () => { window.dispatchEvent(new Event("blur")); });
  await act(async () => { document.dispatchEvent(new Event("visibilitychange")); });
  expect(request).toHaveBeenCalledTimes(1);

  rerender({ scope: "new" });
  await act(async () => {});
  expect(request).toHaveBeenCalledTimes(2);
  vi.useRealTimers();
});

test("uses a ten-second cadence while the document is hidden", async () => {
  vi.useFakeTimers();
  vi.spyOn(document, "hasFocus").mockReturnValue(true);
  Object.defineProperty(document, "visibilityState", { configurable: true, value: "hidden" });
  const request = vi.fn().mockResolvedValue({ complete: false });

  renderHook(() => usePolling(request));
  await act(async () => {});
  await act(async () => { await vi.advanceTimersByTimeAsync(9_999); });
  expect(request).toHaveBeenCalledTimes(1);
  await act(async () => { await vi.advanceTimersByTimeAsync(1); });
  expect(request).toHaveBeenCalledTimes(2);
  vi.useRealTimers();
});

test("stops automatic polling after a non-retryable API error", async () => {
  vi.useFakeTimers();
  vi.spyOn(document, "hasFocus").mockReturnValue(true);
  const request = vi.fn().mockRejectedValue(new ApiError(404, "delete task not found", false));

  const { rerender } = renderHook(
    ({ refreshVersion }) => usePolling(request, { dependencies: [refreshVersion] }),
    { initialProps: { refreshVersion: 0 } }
  );
  await act(async () => {});
  expect(request).toHaveBeenCalledTimes(1);

  await act(async () => { await vi.advanceTimersByTimeAsync(12_000); });
  expect(request).toHaveBeenCalledTimes(1);
  await act(async () => { window.dispatchEvent(new Event("focus")); });
  await act(async () => { window.dispatchEvent(new Event("blur")); });
  await act(async () => { document.dispatchEvent(new Event("visibilitychange")); });
  expect(request).toHaveBeenCalledTimes(1);

  rerender({ refreshVersion: 1 });
  await act(async () => {});
  expect(request).toHaveBeenCalledTimes(2);
  vi.useRealTimers();
});

test("does not let a stale polling request overwrite fresher state", async () => {
  let resolveFirst: ((value: string) => void) | undefined;
  let resolveSecond: ((value: string) => void) | undefined;
  const request = vi.fn()
    .mockImplementationOnce(() => new Promise<string>(resolve => { resolveFirst = resolve; }))
    .mockImplementationOnce(() => new Promise<string>(resolve => { resolveSecond = resolve; }));
  const { result, rerender } = renderHook(
    ({ key }) => usePolling(request, { dependencies: [key] }),
    { initialProps: { key: "old" } }
  );

  rerender({ key: "new" });
  await act(async () => resolveSecond?.("fresh"));
  await waitFor(() => expect(result.current.data).toBe("fresh"));
  await act(async () => resolveFirst?.("stale"));
  expect(result.current.data).toBe("fresh");
});

test("keeps an in-flight page request across equivalent inline-query parent rerenders", async () => {
  let resolveRequest: ((value: GroupPage) => void) | undefined;
  const api = {
    listGroups: vi.fn((query: GroupQuery, signal?: AbortSignal) => {
      void query;
      void signal;
      return new Promise<GroupPage>(resolve => { resolveRequest = resolve; });
    })
  };
  const query: GroupQuery = { kind: "exact", page: 1, size: 100, q: "same" };
  const { rerender } = renderHook(
    ({ current }) => usePagedGroups(api, current),
    { initialProps: { current: query } }
  );
  const firstSignal = api.listGroups.mock.calls[0]?.[1];

  rerender({ current: { ...query } });

  expect(api.listGroups).toHaveBeenCalledTimes(1);
  expect(firstSignal?.aborted).toBe(false);
  await act(async () => resolveRequest?.(page(1)));
});

test("uses reload to bypass only the current cached page once", async () => {
  const api = {
    listGroups: vi.fn((query: GroupQuery) => Promise.resolve(page(query.page)))
  } as Pick<AppApi, "listGroups">;
  const query: GroupQuery = { kind: "exact", page: 1, size: 100 };
  const { result, rerender } = renderHook(
    ({ current }) => usePagedGroups(api, current),
    { initialProps: { current: query } }
  );
  await waitFor(() => expect(result.current.data?.page).toBe(1));

  act(() => result.current.reload());
  await waitFor(() => expect(api.listGroups).toHaveBeenCalledTimes(2));
  rerender({ current: { ...query, page: 2 } });
  await waitFor(() => expect(result.current.data?.page).toBe(2));
  rerender({ current: query });
  await waitFor(() => expect(result.current.data?.page).toBe(1));

  expect(api.listGroups).toHaveBeenCalledTimes(3);
});

test("invalidateAll clears every cached page and refetches the current one", async () => {
  const api = {
    listGroups: vi.fn((query: GroupQuery) => Promise.resolve(page(query.page)))
  } as Pick<AppApi, "listGroups">;
  const query: GroupQuery = { kind: "exact", page: 1, size: 100 };
  const { result, rerender } = renderHook(
    ({ current }) => usePagedGroups(api, current),
    { initialProps: { current: query } }
  );
  await waitFor(() => expect(result.current.data?.page).toBe(1));
  rerender({ current: { ...query, page: 2 } });
  await waitFor(() => expect(result.current.data?.page).toBe(2));
  expect(api.listGroups).toHaveBeenCalledTimes(2);

  act(() => result.current.invalidateAll());
  // 当前页（第 2 页）强制重取
  await waitFor(() => expect(api.listGroups).toHaveBeenCalledTimes(3));
  await waitFor(() => expect(result.current.data?.page).toBe(2));

  // 全部缓存已清空：翻回第 1 页必须重新请求，而不是命中删除前的缓存
  rerender({ current: query });
  await waitFor(() => expect(api.listGroups).toHaveBeenCalledTimes(4));
  await waitFor(() => expect(result.current.data?.page).toBe(1));
});

test("hides data from a different page key while its replacement request is pending", async () => {
  let resolveSecond: ((value: GroupPage) => void) | undefined;
  const api = {
    listGroups: vi.fn((query: GroupQuery) => query.page === 1
      ? Promise.resolve(page(1))
      : new Promise<GroupPage>(resolve => { resolveSecond = resolve; }))
  } as Pick<AppApi, "listGroups">;
  const { result, rerender } = renderHook(
    ({ query }) => usePagedGroups(api, query),
    { initialProps: { query: { kind: "exact", page: 1, size: 100 } as GroupQuery } }
  );
  await waitFor(() => expect(result.current.data?.page).toBe(1));

  rerender({ query: { kind: "exact", page: 2, size: 100 } });

  expect(result.current.loading).toBe(true);
  expect(result.current.data).toBeUndefined();
  await act(async () => resolveSecond?.(page(2)));
  expect(result.current.data?.page).toBe(2);
});

test("keeps same-key successful data visible during an explicit reload", async () => {
  let resolveReload: ((value: GroupPage) => void) | undefined;
  const api = {
    listGroups: vi.fn()
      .mockResolvedValueOnce(page(1))
      .mockImplementationOnce(() => new Promise<GroupPage>(resolve => { resolveReload = resolve; }))
  } as Pick<AppApi, "listGroups">;
  const { result } = renderHook(() => usePagedGroups(api, { kind: "exact", page: 1, size: 100 }));
  await waitFor(() => expect(result.current.data?.page).toBe(1));

  act(() => result.current.reload());

  expect(result.current.loading).toBe(true);
  expect(result.current.data?.page).toBe(1);
  await act(async () => resolveReload?.(page(1)));
});

test("bounds each serialized query cache to five pages and aborts replaced queries", async () => {
  const requests: Array<{ query: GroupQuery; signal?: AbortSignal }> = [];
  const api = {
    listGroups: vi.fn((query: GroupQuery, signal?: AbortSignal) => {
      requests.push({ query, signal });
      return Promise.resolve(page(query.page));
    })
  } as Pick<AppApi, "listGroups">;
  const initialQuery: GroupQuery = { kind: "exact", page: 1, size: 100, q: "old" };
  const { result, rerender } = renderHook(
    ({ query }) => usePagedGroups(api, query),
    { initialProps: { query: initialQuery } }
  );

  await waitFor(() => expect(result.current.data?.page).toBe(1));
  for (let pageNumber = 2; pageNumber <= 6; pageNumber += 1) {
    rerender({ query: { ...initialQuery, page: pageNumber } });
    await waitFor(() => expect(result.current.data?.page).toBe(pageNumber));
  }
  rerender({ query: initialQuery });
  await waitFor(() => expect(api.listGroups).toHaveBeenCalledTimes(7));

  const staleSignal = requests.at(-1)?.signal;
  rerender({ query: { ...initialQuery, q: "new" } });
  expect(staleSignal?.aborted).toBe(true);
});
