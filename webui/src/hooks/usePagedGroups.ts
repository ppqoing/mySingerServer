import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { isAbortError } from "../api/client";
import type { AppApi, GroupPage, GroupQuery } from "../api/contracts";

export interface PagedGroupsState {
  data: GroupPage | undefined;
  error: Error | undefined;
  loading: boolean;
  reload(): void;
  invalidateAll(): void;
}

type GroupApi = Pick<AppApi, "listGroups">;
type PageCache = Map<string, Map<number, GroupPage>>;
interface InternalPagedGroupsState {
  pageKey: string | undefined;
  data: GroupPage | undefined;
  error: Error | undefined;
  loading: boolean;
}

export function usePagedGroups(api: GroupApi, query: GroupQuery): PagedGroupsState {
  const cache = useRef<PageCache>(new Map());
  const consumedReloadVersion = useRef(0);
  const reloadTarget = useRef<string | undefined>(undefined);
  const [reloadVersion, setReloadVersion] = useState(0);
  const [state, setState] = useState<InternalPagedGroupsState>({
    pageKey: undefined,
    data: undefined,
    error: undefined,
    loading: true
  });
  const scope = serializeGroupQuery(query);
  const pageNumber = query.page;
  const pageKey = `${scope}\n${pageNumber}`;
  const querySnapshot = useMemo(
    () => groupQueryFromScope(scope, pageNumber),
    [pageNumber, scope]
  );

  useEffect(() => {
    const hasPendingReload = reloadVersion > consumedReloadVersion.current;
    const forceReload = hasPendingReload && reloadTarget.current === pageKey;
    if (hasPendingReload) {
      consumedReloadVersion.current = reloadVersion;
    }
    const cached = readCachedPage(cache.current, scope, pageNumber);
    if (cached !== undefined && !forceReload) {
      setState({ pageKey, data: cached, error: undefined, loading: false });
      return undefined;
    }

    let active = true;
    const controller = new AbortController();
    setState(previous => ({
      pageKey,
      data: previous.pageKey === pageKey ? previous.data : undefined,
      error: undefined,
      loading: true
    }));
    void api.listGroups(querySnapshot, controller.signal).then(
      page => {
        if (!active || controller.signal.aborted) {
          return;
        }
        writeCachedPage(cache.current, scope, pageNumber, page);
        setState({ pageKey, data: page, error: undefined, loading: false });
      },
      error => {
        if (!active || controller.signal.aborted || isAbortError(error)) {
          return;
        }
        setState(previous => ({
          ...previous,
          pageKey,
          error: error instanceof Error ? error : new Error("读取重复组失败"),
          loading: false
        }));
      }
    );
    return () => {
      active = false;
      controller.abort();
    };
  }, [api, pageKey, pageNumber, querySnapshot, reloadVersion, scope]);

  const reload = useCallback(() => {
    reloadTarget.current = pageKey;
    setReloadVersion(version => version + 1);
  }, [pageKey]);
  // 写操作（如删除）完成后调用：丢弃全部 scope 的缓存页，避免翻页读到删除前的陈旧数据。
  const invalidateAll = useCallback(() => {
    cache.current.clear();
    reloadTarget.current = pageKey;
    setReloadVersion(version => version + 1);
  }, [pageKey]);
  const isCurrentPageKey = state.pageKey === pageKey;
  return {
    data: isCurrentPageKey ? state.data : undefined,
    error: isCurrentPageKey ? state.error : undefined,
    loading: isCurrentPageKey ? state.loading : true,
    reload,
    invalidateAll
  };
}

export function serializeGroupQuery(query: GroupQuery): string {
  return JSON.stringify({
    kind: query.kind,
    size: query.size,
    q: query.q,
    machine: query.machine,
    minMembers: query.minMembers,
    sort: query.sort
  });
}

function groupQueryFromScope(scope: string, page: number): GroupQuery {
  const values = JSON.parse(scope) as Omit<GroupQuery, "page">;
  return { ...values, page };
}

function readCachedPage(cache: PageCache, scope: string, page: number): GroupPage | undefined {
  const pages = cache.get(scope);
  const value = pages?.get(page);
  if (pages !== undefined && value !== undefined) {
    pages.delete(page);
    pages.set(page, value);
  }
  return value;
}

function writeCachedPage(cache: PageCache, scope: string, page: number, value: GroupPage): void {
  const pages = cache.get(scope) ?? new Map<number, GroupPage>();
  cache.set(scope, pages);
  pages.delete(page);
  pages.set(page, value);
  while (pages.size > 5) {
    const oldest = pages.keys().next().value;
    if (oldest === undefined) {
      return;
    }
    pages.delete(oldest);
  }
}
