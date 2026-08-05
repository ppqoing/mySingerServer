import { useEffect, useEffectEvent, useState } from "react";
import { ApiError, isAbortError } from "../api/client";

export interface PollingOptions<T> {
  enabled?: boolean;
  dependencies?: readonly unknown[];
  isTerminal?: (value: T) => boolean;
}

export interface PollingState<T> {
  data: T | undefined;
  error: Error | undefined;
  loading: boolean;
  successRevision: number;
}

export function usePolling<T>(
  request: (signal: AbortSignal) => Promise<T>,
  options: PollingOptions<T> = {}
): PollingState<T> {
  const requestLatest = useEffectEvent(request);
  const terminalLatest = useEffectEvent((value: T) => options.isTerminal?.(value) ?? false);
  const [state, setState] = useState<PollingState<T>>({
    data: undefined,
    error: undefined,
    loading: false,
    successRevision: 0
  });
  const dependencies = options.dependencies ?? [];

  useEffect(() => {
    if (options.enabled === false) {
      return undefined;
    }
    let disposed = false;
    let running = false;
    let terminal = false;
    let hasAttempted = false;
    let controller: AbortController | undefined;
    let timer: ReturnType<typeof setTimeout> | undefined;

    const interval = () => document.visibilityState === "visible" && document.hasFocus()
      ? 2_000
      : 10_000;
    const clearTimer = () => {
      if (timer !== undefined) {
        clearTimeout(timer);
        timer = undefined;
      }
    };
    const schedule = () => {
      clearTimer();
      timer = setTimeout(() => { void poll(); }, interval());
    };
    const poll = async () => {
      if (disposed || running || terminal) {
        return;
      }
      running = true;
      controller = new AbortController();
      const requestController = controller;
      const shouldResetPriorError = !hasAttempted;
      hasAttempted = true;
      setState(previous => ({
        ...previous,
        loading: true,
        error: shouldResetPriorError ? undefined : previous.error
      }));
      try {
        const value = await requestLatest(requestController.signal);
        if (disposed || requestController.signal.aborted) {
          return;
        }
        setState(previous => ({
          data: value,
          error: undefined,
          loading: false,
          successRevision: previous.successRevision + 1
        }));
        terminal = terminalLatest(value);
        if (!terminal) {
          schedule();
        }
      } catch (error) {
        if (disposed || requestController.signal.aborted || isAbortError(error)) {
          return;
        }
        const pollingError = error instanceof Error ? error : new Error("轮询请求失败");
        setState(previous => ({
          ...previous,
          error: pollingError,
          loading: false
        }));
        if (error instanceof ApiError && !error.retryable) {
          terminal = true;
        } else {
          schedule();
        }
      } finally {
        running = false;
      }
    };
    const refreshForEnvironment = () => {
      if (!running && !terminal) {
        clearTimer();
        void poll();
      }
    };

    document.addEventListener("visibilitychange", refreshForEnvironment);
    window.addEventListener("focus", refreshForEnvironment);
    window.addEventListener("blur", refreshForEnvironment);
    void poll();
    return () => {
      disposed = true;
      clearTimer();
      controller?.abort();
      document.removeEventListener("visibilitychange", refreshForEnvironment);
      window.removeEventListener("focus", refreshForEnvironment);
      window.removeEventListener("blur", refreshForEnvironment);
    };
  // The caller opts into restart values through options.dependencies.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [options.enabled, ...dependencies]);

  return state;
}
