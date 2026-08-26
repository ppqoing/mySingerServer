import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { appApi, type AppApi } from "../../api/appApi";
import { ApiError } from "../../api/client";
import { apiErrorText } from "../../api/errorText";
import type { AnalysisStats } from "../../api/contracts";
import { AsyncState } from "../../components/AsyncState";
import { usePolling } from "../../hooks/usePolling";
import "../operational-pages.css";

export interface AnalysisPageProps {
  readonly api?: AppApi;
}

function heapText(bytes: number) {
  return `${(bytes / 1_048_576).toFixed(1)} MB`;
}

function metrics(stats: AnalysisStats) {
  return [
    ["扫描文件", stats.filesScanned], ["精确重复组", stats.exactGroups], ["精确重复成员", stats.exactMembers],
    ["图片特征", stats.imageFeatures], ["图片候选对", stats.imagePairs], ["视频特征", stats.videoFeatures],
    ["视频候选对", stats.videoPairs], ["写入重复组", stats.groupsWritten], ["写入重复成员", stats.membersWritten],
    ["坏行", stats.badRows], ["跳过候选对", stats.skippedPairs], ["堆内存", heapText(stats.heapAllocBytes)]
  ] as const;
}

function stageElapsedText(milliseconds: number) {
  return `${(milliseconds / 1_000).toFixed(1)} 秒`;
}

export function AnalysisPage({ api = appApi }: AnalysisPageProps) {
  const [refreshVersion, setRefreshVersion] = useState(0);
  const [starting, setStarting] = useState(false);
  const [awaitingStatus, setAwaitingStatus] = useState(false);
  const [cancelling, setCancelling] = useState(false);
  const [runError, setRunError] = useState<string>();
  const [runConflict, setRunConflict] = useState(false);
  const runController = useRef<AbortController | undefined>(undefined);
  const cancelController = useRef<AbortController | undefined>(undefined);
  const confirmationRevision = useRef<number | undefined>(undefined);
  const request = useCallback((signal: AbortSignal) => api.getAnalysisStatus(signal), [api]);
  const state = usePolling(request, {
    dependencies: [api, refreshVersion],
    isTerminal: status => !status.running
  });
  const status = state.data;
  const refresh = () => setRefreshVersion(version => version + 1);
  const confirmStartedRun = () => {
    confirmationRevision.current = state.successRevision;
    setAwaitingStatus(true);
    refresh();
  };
  useEffect(() => {
    const baseline = confirmationRevision.current;
    if (baseline !== undefined && state.successRevision > baseline) {
      confirmationRevision.current = undefined;
      setAwaitingStatus(false);
    }
  }, [state.successRevision]);
  useEffect(() => () => {
    runController.current?.abort();
    runController.current = undefined;
    cancelController.current?.abort();
    cancelController.current = undefined;
  }, []);
  // 取消请求发出后按钮保持禁用；轮询确认状态回到空闲时在渲染期比对复位（避免 effect 内 setState 级联渲染）。
  const [settledRunning, setSettledRunning] = useState(status?.running);
  if (status?.running !== settledRunning) {
    setSettledRunning(status?.running);
    if (status?.running === false) setCancelling(false);
  }
  const run = async () => {
    runController.current?.abort();
    const controller = new AbortController();
    runController.current = controller;
    setStarting(true);
    setRunError(undefined);
    setRunConflict(false);
    try {
      await api.runAnalysis(controller.signal);
      if (controller.signal.aborted || runController.current !== controller) return;
      confirmStartedRun();
    } catch (error) {
      if (controller.signal.aborted || runController.current !== controller) return;
      if (error instanceof ApiError && error.status === 409) {
        setRunConflict(true);
        confirmStartedRun();
      } else {
        setRunError(apiErrorText(error, "启动分析失败。"));
      }
    } finally {
      if (runController.current === controller) {
        runController.current = undefined;
        if (!controller.signal.aborted) setStarting(false);
      }
    }
  };
  const cancel = async () => {
    cancelController.current?.abort();
    const controller = new AbortController();
    cancelController.current = controller;
    setCancelling(true);
    setRunError(undefined);
    try {
      await api.cancelAnalysis(controller.signal);
      if (controller.signal.aborted || cancelController.current !== controller) return;
      refresh();
    } catch (error) {
      if (controller.signal.aborted || cancelController.current !== controller) return;
      if (error instanceof ApiError && error.status === 409) {
        setRunError("没有正在运行的分析。");
      } else {
        setRunError(apiErrorText(error, "取消分析失败。"));
      }
      setCancelling(false);
      refresh();
    } finally {
      if (cancelController.current === controller) cancelController.current = undefined;
    }
  };
  const exportMetrics = () => {
    if (!status?.last) return;
    const blob = new Blob([JSON.stringify(status.last, null, 2)], { type: "application/json" });
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement("a");
    anchor.href = url;
    anchor.download = "firstscreen-analysis-metrics.json";
    anchor.click();
    URL.revokeObjectURL(url);
  };
  const statusLabel = status === undefined ? "未知" : status.running ? "运行中" : "空闲";
  // 409 提示派生展示而非独立清理：仅在等待确认/运行中可见，状态回到空闲即不再显示，避免常驻误导。
  const conflictVisible = runConflict && (awaitingStatus || status?.running === true);

  return (
    <section aria-labelledby="analysis-heading" className="operational-page">
      <header className="operational-page__header operational-surface">
        <h1 id="analysis-heading">一筛分析</h1>
        <p>执行数据库中的首轮重复分析；同一时间只能运行一个任务。</p>
        <p>状态：{statusLabel}</p>
        <button disabled={starting || awaitingStatus || status?.running === true} onClick={() => void run()} type="button">
          {starting ? "正在启动…" : "开始一筛分析"}
        </button>
        {status?.running === true ? (
          <button disabled={cancelling} onClick={() => void cancel()} type="button">
            {cancelling ? "正在取消…" : "取消分析"}
          </button>
        ) : null}
        <button disabled={state.loading} onClick={refresh} type="button">刷新状态</button>
        <button disabled={!status?.last} onClick={exportMetrics} type="button">导出指标 JSON</button>
        {status && !status.running && status.last ? (
          <p><Link to="/groups">检出 {status.last.groupsWritten} 个重复组，前往查看 →</Link></p>
        ) : null}
      </header>
      {conflictVisible ? <p role="alert">已有分析正在运行</p> : null}
      {runError ? <p role="alert">{runError}</p> : null}
      {state.error ? <AsyncState error={state.error.message} onRetry={refresh} state="error" /> : null}
      {!status && state.loading ? <AsyncState state="loading" /> : null}
      {status?.lastErr ? <p role="alert">{apiErrorText(new Error(status.lastErr))}</p> : null}
      {status?.last ? (
        <section aria-label="上次分析指标" className="operational-surface">
          <h2>最近一次分析</h2>
          <dl>{metrics(status.last).map(([label, value]) => <div key={label}><dt>{label}</dt><dd>{value}</dd></div>)}</dl>
          {Object.entries(status.last.stageElapsedMs).length > 0 ? (
            <section aria-label="阶段耗时"><h3>阶段耗时</h3><dl>{Object.entries(status.last.stageElapsedMs).map(([stage, elapsed]) => <div key={stage}><dt>{stage}</dt><dd>{stageElapsedText(elapsed)}</dd></div>)}</dl></section>
          ) : null}
        </section>
      ) : null}
    </section>
  );
}
