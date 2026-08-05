import { useCallback, useEffect, useRef, useState } from "react";
import { appApi, type AppApi } from "../../api/appApi";
import { ApiError } from "../../api/client";
import type { AnalysisStats } from "../../api/contracts";
import { AsyncState } from "../../components/AsyncState";
import { usePolling } from "../../hooks/usePolling";
import "../operational-pages.css";

export interface AnalysisPageProps {
  readonly api?: AppApi;
}

function metrics(stats: AnalysisStats) {
  return [
    ["扫描文件", stats.filesScanned], ["精确重复组", stats.exactGroups], ["精确重复成员", stats.exactMembers],
    ["图片特征", stats.imageFeatures], ["图片候选对", stats.imagePairs], ["视频特征", stats.videoFeatures],
    ["视频候选对", stats.videoPairs], ["写入重复组", stats.groupsWritten], ["写入重复成员", stats.membersWritten],
    ["坏行", stats.badRows], ["跳过候选对", stats.skippedPairs], ["堆内存（字节）", stats.heapAllocBytes]
  ] as const;
}

export function AnalysisPage({ api = appApi }: AnalysisPageProps) {
  const [refreshVersion, setRefreshVersion] = useState(0);
  const [starting, setStarting] = useState(false);
  const [awaitingStatus, setAwaitingStatus] = useState(false);
  const [runError, setRunError] = useState<string>();
  const runController = useRef<AbortController | undefined>(undefined);
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
  }, []);
  const run = async () => {
    runController.current?.abort();
    const controller = new AbortController();
    runController.current = controller;
    setStarting(true);
    setRunError(undefined);
    try {
      await api.runAnalysis(controller.signal);
      if (controller.signal.aborted || runController.current !== controller) return;
      confirmStartedRun();
    } catch (error) {
      if (controller.signal.aborted || runController.current !== controller) return;
      if (error instanceof ApiError && error.status === 409) {
        setRunError("已有分析正在运行");
        confirmStartedRun();
      } else {
        setRunError(error instanceof Error ? error.message : "启动分析失败。");
      }
    } finally {
      if (runController.current === controller) {
        runController.current = undefined;
        if (!controller.signal.aborted) setStarting(false);
      }
    }
  };
  const statusLabel = status === undefined ? "未知" : status.running ? "运行中" : "空闲";

  return (
    <section aria-labelledby="analysis-heading" className="operational-page">
      <header className="operational-page__header operational-surface">
        <h1 id="analysis-heading">一筛分析</h1>
        <p>执行数据库中的首轮重复分析；同一时间只能运行一个任务。</p>
        <p>状态：{statusLabel}</p>
        <button disabled={starting || awaitingStatus || status?.running === true} onClick={() => void run()} type="button">
          {starting ? "正在启动…" : "开始一筛分析"}
        </button>
        <button disabled={state.loading} onClick={refresh} type="button">刷新状态</button>
      </header>
      {runError ? <p role="alert">{runError}</p> : null}
      {state.error ? <AsyncState error={state.error.message} onRetry={refresh} state="error" /> : null}
      {!status && state.loading ? <AsyncState state="loading" /> : null}
      {status?.lastErr ? <p role="alert">{status.lastErr}</p> : null}
      {status?.last ? (
        <section aria-label="上次分析指标" className="operational-surface">
          <h2>最近一次分析</h2>
          <dl>{metrics(status.last).map(([label, value]) => <div key={label}><dt>{label}</dt><dd>{value}</dd></div>)}</dl>
          {Object.entries(status.last.stageElapsedMs).length > 0 ? (
            <section aria-label="阶段耗时"><h3>阶段耗时</h3><dl>{Object.entries(status.last.stageElapsedMs).map(([stage, elapsed]) => <div key={stage}><dt>{stage}</dt><dd>{elapsed} 毫秒</dd></div>)}</dl></section>
          ) : null}
        </section>
      ) : null}
    </section>
  );
}
