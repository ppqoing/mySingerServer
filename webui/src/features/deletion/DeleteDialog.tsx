import { useEffect, useRef, useState, type ReactNode } from "react";
import { appApi } from "../../api/appApi";
import { ApiError, isAbortError } from "../../api/client";
import type {
  AppApi,
  DeleteMode,
  DeletePreparation,
  DeleteTaskStatus
} from "../../api/contracts";
import { apiErrorText } from "../../api/errorText";
import { CopyButton } from "../../components/CopyButton";
import { Modal } from "../../components/Modal";
import type { DeleteReviewMember } from "../groups/deleteReview";
import "./deletion.css";
import { errorCodeText } from "./errorCodes";
import { RecycledToList } from "./RecycledToList";

type DeletePhase =
  | { name: "idle" }
  | { name: "preparing"; memberIds: number[] }
  | {
      name: "confirming";
      memberIds: number[];
      preparation: DeletePreparation;
      mode: DeleteMode;
    }
  | {
      name: "executing";
      memberIds: number[];
      preparation: DeletePreparation;
      mode: DeleteMode;
    }
  | {
      name: "polling";
      taskId: string;
      lastStatus?: DeleteTaskStatus;
      pollError?: { failures: number; message: string };
    }
  | {
      name: "poll-error";
      taskId: string;
      lastStatus?: DeleteTaskStatus;
      error: ApiError | Error;
    }
  | { name: "terminal"; status: DeleteTaskStatus };

type DeleteFault =
  | { kind: "empty"; message: string }
  | { kind: "prepare"; message: string }
  | { kind: "expired-clock"; message: string }
  | { kind: "expired-api"; message: string }
  | { kind: "execute"; message: string };

export interface DeleteDialogProps {
  readonly open: boolean;
  readonly memberIds: number[];
  readonly initialTaskId?: string;
  readonly api?: AppApi;
  /** 确认页展示的保留文件（代表与未选中成员）；空数组表示无法穷举，仅展示口径说明。 */
  readonly keptMembers?: readonly DeleteReviewMember[];
  readonly onClose: () => void;
  readonly onAccepted?: (taskId: string) => void;
  readonly onExecutionRejected?: () => void;
  readonly onExecutionStarted?: (memberIds: number[]) => void;
  readonly onTerminal: (status: DeleteTaskStatus) => void;
  readonly onExecutionLockChange?: (locked: boolean) => void;
  readonly onExecutionPendingChange?: (pending: boolean) => void;
}

interface OpenDeleteDialogSessionProps {
  readonly memberIds: number[];
  readonly selectionKey: string;
  readonly initialTaskId?: string;
  readonly api: AppApi;
  readonly keptMembers?: readonly DeleteReviewMember[];
  readonly onClose: () => void;
  readonly onAccepted?: (taskId: string) => void;
  readonly onExecutionRejected?: () => void;
  readonly onExecutionStarted?: (memberIds: number[]) => void;
  readonly onTerminal: (status: DeleteTaskStatus) => void;
  readonly onExecutionLockChange?: (locked: boolean) => void;
  readonly onExecutionPendingChange?: (pending: boolean) => void;
}

interface DeleteConfirmationSessionProps {
  readonly memberIds: number[];
  readonly selectionKey: string;
  readonly api: AppApi;
  readonly keptMembers: readonly DeleteReviewMember[];
  readonly onClose: () => void;
  readonly onExecutionRejected: () => void;
  readonly onExecutionStarted: (memberIds: number[], selectionKey: string) => void;
  readonly onExecutionPendingChange?: (pending: boolean) => void;
  readonly onReprepareRequested: () => void;
  readonly onTaskAccepted: (taskId: string) => void;
  readonly onExecutionLockChange?: (locked: boolean) => void;
}

interface DeleteTaskSessionProps {
  readonly api: AppApi;
  readonly onClose: () => void;
  readonly onTerminal: (status: DeleteTaskStatus) => void;
  readonly taskId: string;
}

function normalizeMemberIds(memberIds: number[]): number[] {
  return [...new Set(memberIds.filter(id => Number.isSafeInteger(id) && id > 0))]
    .sort((left, right) => left - right);
}

function errorValue(error: unknown, fallback: string): ApiError | Error {
  return error instanceof Error ? error : new Error(fallback);
}

function isExpiredConfirmation(error: unknown): boolean {
  if (!(error instanceof ApiError)) return false;
  if (error.status === 400) return /invalid confirmation|expired/i.test(error.message);
  // A consumed token can never succeed again; treat it like an expired one so
  // the user is forced back to an explicit re-prepare instead of retrying a
  // dead confirmation forever.
  if (error.status === 409) return /already used/i.test(error.message);
  return false;
}

function StatusDetails({ status }: { readonly status: DeleteTaskStatus }) {
  const machines = Object.values(status.byMachine)
    .sort((left, right) => left.machineId.localeCompare(right.machineId));
  const errorCodes = Object.entries(status.errorCodes)
    .sort(([left], [right]) => left.localeCompare(right));

  return <>
    <p>模式：{status.mode}</p>
    <p>总数：{status.total}</p>
    <p>成功：{status.ok}</p>
    <p>失败：{status.failed}</p>
    <p>不确定：{status.uncertain}</p>
    <p>待处理：{status.pending}</p>
    <p>状态同步失败：{status.stateSyncFailures}</p>
    <p>状态：{status.complete ? "已完成" : "进行中"}</p>

    <section aria-label="按 Agent 删除进度">
      <h3>按 Agent 删除进度</h3>
      <ul>
        {machines.map(machine => {
          const sequences = Object.values(machine.sequences)
            .sort((left, right) => left.sequence - right.sequence);
          return <li key={machine.machineId}>
            <p>
              Agent：{machine.machineId}；总数：{machine.total}；成功：{machine.ok}；
              失败：{machine.failed}；不确定：{machine.uncertain}；待处理：{machine.pending}；
              状态同步失败：{machine.stateSyncFailures}；状态：{machine.complete ? "已完成" : "进行中"}
            </p>
            {sequences.length === 0
              ? <p>序列进度：无</p>
              : <section aria-label={`Agent ${machine.machineId} 序列进度`}>
                  <h4>序列进度</h4>
                  <ul>
                    {sequences.map(sequence => <li data-testid="delete-sequence-status" key={sequence.sequence}>
                      {`序列：${sequence.sequence}；最后序列：${sequence.lastSeq}；已接收：${sequence.received ? "是" : "否"}；总数：${sequence.total}；成功：${sequence.ok}；失败：${sequence.failed}；不确定：${sequence.uncertain}`}
                    </li>)}
                  </ul>
                </section>}
            {machine.recycledTo === undefined ? null : (
              <RecycledToList machineId={machine.machineId} recycledTo={machine.recycledTo} />
            )}
          </li>;
        })}
      </ul>
    </section>

    <section aria-label="删除错误代码">
      <h3>错误代码</h3>
      {errorCodes.length === 0
        ? <p>无</p>
        : <ul>{errorCodes.map(([code, count]) => <li key={code}>{errorCodeText(code)}：{count}</li>)}</ul>}
    </section>

    <section aria-label="删除问题项目">
      <h3>问题项目</h3>
      {status.problems.length === 0
        ? <p>无</p>
        : <ul>{status.problems.map((problem, index) => {
          const detail = problem.errorMessage ?? problem.stateSyncErr;
          return <li key={`${problem.machineId}-${problem.sequence}-${index}`}>
            Agent：{problem.machineId}；序列：{problem.sequence}；路径：{problem.path}
            {" "}<CopyButton label="复制路径" text={problem.path} />
            ；错误代码：{errorCodeText(problem.errorCode)}；错误消息：{problem.errorMessage ?? "未提供"}；
            不确定：{problem.uncertain ? "是" : "否"}；状态同步错误：{problem.stateSyncErr ?? "无"}
            {detail ? <> <CopyButton label="复制错误详情" text={detail} /></> : null}
          </li>;
        })}</ul>}
    </section>
  </>;
}

function DeleteConfirmationSession({
  memberIds,
  selectionKey,
  api,
  keptMembers,
  onClose,
  onExecutionRejected,
  onExecutionStarted,
  onExecutionPendingChange,
  onReprepareRequested,
  onTaskAccepted,
  onExecutionLockChange
}: DeleteConfirmationSessionProps) {
  const [phase, setPhase] = useState<Extract<
    DeletePhase,
    { name: "idle" | "preparing" | "confirming" | "executing" }
  >>(() => memberIds.length > 0 ? { name: "preparing", memberIds } : { name: "idle" });
  const [fault, setFault] = useState<DeleteFault | undefined>(() => memberIds.length > 0
    ? undefined
    : { kind: "empty", message: "没有可删除的已选文件" });
  const [prepareVersion, setPrepareVersion] = useState(0);
  const [remainingSeconds, setRemainingSeconds] = useState(0);
  const prepareControllerRef = useRef<AbortController | undefined>(undefined);
  const executeStartedRef = useRef(false);
  const expiresAtRef = useRef(0);
  const locked = phase.name === "preparing" || phase.name === "executing";

  useEffect(() => {
    onExecutionLockChange?.(locked);
    return () => onExecutionLockChange?.(false);
  }, [locked, onExecutionLockChange]);

  useEffect(() => () => prepareControllerRef.current?.abort(), []);

  useEffect(() => {
    if (selectionKey === "") return;
    const snapshot = selectionKey.split(",").map(Number);
    const controller = new AbortController();
    prepareControllerRef.current = controller;

    void api.prepareDelete(snapshot, controller.signal).then(
      preparation => {
        if (controller.signal.aborted) return;
        const seconds = Math.max(0, Math.floor(preparation.expiresInSeconds));
        if (seconds === 0) {
          setRemainingSeconds(0);
          setPhase({ name: "idle" });
          setFault({ kind: "expired-clock", message: "确认已过期，请重新准备" });
          return;
        }
        expiresAtRef.current = Date.now() + seconds * 1_000;
        setRemainingSeconds(seconds);
        setPhase({ name: "confirming", memberIds: snapshot, preparation, mode: "soft" });
      },
      error => {
        if (controller.signal.aborted || isAbortError(error)) return;
        setPhase({ name: "idle" });
        setFault({ kind: "prepare", message: apiErrorText(error, "准备删除失败") });
      }
    );

    return () => controller.abort();
  }, [api, prepareVersion, selectionKey]);

  useEffect(() => {
    if (phase.name !== "confirming" || remainingSeconds <= 0) return;
    const timer = window.setTimeout(() => {
      const next = Math.max(0, Math.ceil((expiresAtRef.current - Date.now()) / 1_000));
      if (next > 0) {
        setRemainingSeconds(next);
        return;
      }
      setRemainingSeconds(0);
      setPhase({ name: "idle" });
      setFault({ kind: "expired-clock", message: "确认已过期，请重新准备" });
    }, 1_000);
    return () => window.clearTimeout(timer);
  }, [phase.name, remainingSeconds]);

  function close() {
    if (locked) return;
    prepareControllerRef.current?.abort();
    onClose();
  }

  function retryPrepare() {
    onReprepareRequested();
    executeStartedRef.current = false;
    setFault(undefined);
    setRemainingSeconds(0);
    setPhase({ name: "preparing", memberIds });
    setPrepareVersion(version => version + 1);
  }

  function setMode(mode: DeleteMode) {
    setPhase(current => current.name === "confirming" ? { ...current, mode } : current);
  }

  function execute() {
    if (phase.name !== "confirming" || executeStartedRef.current || remainingSeconds <= 0) return;
    if (Date.now() >= expiresAtRef.current) {
      setRemainingSeconds(0);
      setPhase({ name: "idle" });
      setFault({ kind: "expired-clock", message: "确认已过期，请重新准备" });
      return;
    }

    executeStartedRef.current = true;
    const executing = {
      name: "executing" as const,
      memberIds: phase.memberIds,
      preparation: phase.preparation,
      mode: phase.mode
    };
    const controller = new AbortController();
    setFault(undefined);
    setPhase(executing);
    onExecutionStarted(executing.memberIds, selectionKey);
    onExecutionPendingChange?.(true);

    void api.executeDelete(executing.preparation.confirmToken, executing.mode, controller.signal).then(
      ({ taskId }) => {
        if (controller.signal.aborted) return;
        onTaskAccepted(taskId);
        onExecutionPendingChange?.(false);
      },
      error => {
        onExecutionPendingChange?.(false);
        if (controller.signal.aborted || isAbortError(error)) return;
        executeStartedRef.current = false;
        if (isExpiredConfirmation(error)) {
          setPhase({ name: "idle" });
          setFault({
            kind: "expired-api",
            message: error instanceof ApiError && error.status === 409
              ? "该确认已被使用，请重新准备"
              : "确认已过期，请重新准备"
          });
          return;
        }
        onExecutionRejected();
        setPhase({
          name: "confirming",
          memberIds: executing.memberIds,
          preparation: executing.preparation,
          mode: executing.mode
        });
        setFault({ kind: "execute", message: apiErrorText(error, "提交删除失败") });
      }
    );
  }

  let content: ReactNode;
  if (phase.name === "preparing") {
    content = <p role="status">正在准备删除</p>;
  } else if (phase.name === "confirming" || phase.name === "executing") {
    const busy = phase.name === "executing";
    const { preparation } = phase;
    const machineCounts = Object.entries(preparation.summary.byMachine)
      .sort(([left], [right]) => left.localeCompare(right));
    content = <>
      <p>{preparation.summary.totalFiles.toLocaleString("zh-CN")} 个文件</p>
      <p>{preparation.summary.totalBytes.toLocaleString("zh-CN")} 字节</p>
      <ul aria-label="按 Agent 确认数量">
        {machineCounts.map(([machineId, count]) => <li data-testid="delete-machine-summary" key={machineId}>
          {machineId}：{count}
        </li>)}
      </ul>
      <ul aria-label="路径样本">
        {preparation.summary.samples.map((sample, index) => <li key={`${sample}-${index}`}>{sample}</li>)}
      </ul>
      <section aria-label="本次保留的文件">
        <h3>本次保留的文件</h3>
        {keptMembers.length === 0
          ? <p>未选中的成员与各组代表文件将全部保留。</p>
          : <ul>{keptMembers.map(kept =>
              <li data-testid="kept-member" key={kept.fileId}>{kept.machineId}：{kept.path}</li>)}</ul>}
      </section>
      <p>确认令牌剩余 {remainingSeconds} 秒</p>
      <p>硬删除会永久删除文件且不可恢复。</p>
      {fault?.kind === "execute" ? <p role="alert">{fault.message}</p> : null}
      {phase.mode === "hard" ? <p role="alert">硬删除将永久删除文件，无法恢复。</p> : null}
      {busy ? <p role="status">正在提交删除，请勿关闭或切换视图</p> : null}
      <fieldset disabled={busy}>
        <legend>删除模式</legend>
        <label>
          <input checked={phase.mode === "soft"} name="delete-mode" onChange={() => setMode("soft")} type="radio" />
          软删除
        </label>
        <label>
          <input checked={phase.mode === "hard"} name="delete-mode" onChange={() => setMode("hard")} type="radio" />
          硬删除
        </label>
      </fieldset>
      <button disabled={busy} onClick={close} type="button">取消</button>
      <button
        className={phase.mode === "hard" ? "danger-action" : undefined}
        disabled={busy || remainingSeconds <= 0}
        onClick={execute}
        type="button"
      >
        {phase.mode === "hard" ? "最终确认硬删除" : "最终确认删除"}
      </button>
    </>;
  } else if (fault) {
    content = <>
      <p role="alert">{fault.message}</p>
      {fault.kind === "expired-clock"
        ? <button disabled type="button">最终确认删除</button>
        : null}
      {fault.kind === "prepare" || fault.kind === "expired-clock" || fault.kind === "expired-api"
        ? <button onClick={retryPrepare} type="button">重新准备</button>
        : null}
    </>;
  } else {
    content = <p role="status">等待删除准备</p>;
  }

  return <Modal disableClose={locked} onClose={close} open title="确认删除">
    {content}
  </Modal>;
}

// 轮询容错上限：瞬时故障按 1s→2s→4s→8s（封顶）退避自动恢复，
// 连续失败 MAX_POLL_FAILURES 次后进入手动重试态
const MAX_POLL_FAILURES = 5;
const MAX_POLL_RETRY_DELAY_MS = 8_000;

function DeleteTaskSession({ api, onClose, onTerminal, taskId }: DeleteTaskSessionProps) {
  const [phase, setPhase] = useState<Extract<
    DeletePhase,
    { name: "polling" | "poll-error" | "terminal" }
  >>({ name: "polling", taskId });
  const [pollVersion, setPollVersion] = useState(0);
  const lastStatusRef = useRef<DeleteTaskStatus | undefined>(undefined);
  const consecutiveFailuresRef = useRef(0);
  const terminalNotifiedRef = useRef(false);
  const onTerminalRef = useRef(onTerminal);

  useEffect(() => {
    onTerminalRef.current = onTerminal;
  }, [onTerminal]);

  useEffect(() => {
    const controller = new AbortController();
    let timer: number | undefined;
    let active = true;

    const poll = async () => {
      try {
        const status = await api.getDeleteStatus(taskId, controller.signal);
        if (!active || controller.signal.aborted) return;
        consecutiveFailuresRef.current = 0;
        lastStatusRef.current = status;
        if (status.complete) {
          setPhase({ name: "terminal", status });
          return;
        }
        setPhase({ name: "polling", taskId, lastStatus: status });
        timer = window.setTimeout(() => void poll(), 1_000);
      } catch (error) {
        if (!active || controller.signal.aborted || isAbortError(error)) return;
        const failure = errorValue(error, "读取删除进度失败");
        // 仅可重试错误（408/429/5xx 与网络错误，ApiError.retryable=true）做退避恢复
        const retryable = failure instanceof ApiError && failure.retryable;
        consecutiveFailuresRef.current += 1;
        if (retryable && consecutiveFailuresRef.current < MAX_POLL_FAILURES) {
          const delay = Math.min(
            1_000 * 2 ** (consecutiveFailuresRef.current - 1),
            MAX_POLL_RETRY_DELAY_MS
          );
          setPhase({
            name: "polling",
            taskId,
            lastStatus: lastStatusRef.current,
            pollError: {
              failures: consecutiveFailuresRef.current,
              message: apiErrorText(failure, "读取删除进度失败")
            }
          });
          timer = window.setTimeout(() => void poll(), delay);
          return;
        }
        setPhase({
          name: "poll-error",
          taskId,
          lastStatus: lastStatusRef.current,
          error: failure
        });
      }
    };

    void poll();
    return () => {
      active = false;
      controller.abort();
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [api, pollVersion, taskId]);

  const terminalStatus = phase.name === "terminal" ? phase.status : undefined;
  useEffect(() => {
    if (!terminalStatus || terminalNotifiedRef.current) return;
    terminalNotifiedRef.current = true;
    onTerminalRef.current(terminalStatus);
  }, [terminalStatus]);

  function retryPoll() {
    if (phase.name !== "poll-error") return;
    consecutiveFailuresRef.current = 0;
    lastStatusRef.current = phase.lastStatus;
    setPhase({ name: "polling", taskId, lastStatus: phase.lastStatus });
    setPollVersion(version => version + 1);
  }

  let content: ReactNode;
  if (phase.name === "polling" || phase.name === "poll-error") {
    content = <>
      <p><span>任务 ID：{taskId}</span> <CopyButton text={taskId} /></p>
      {phase.lastStatus ? <StatusDetails status={phase.lastStatus} /> : <p role="status">正在获取删除进度</p>}
      {phase.name === "polling" && phase.pollError
        ? <p role="status">{phase.pollError.message} 正在自动重试（连续第 {phase.pollError.failures} 次失败）</p>
        : null}
      {phase.name === "poll-error" ? <>
        <p role="alert">{apiErrorText(phase.error, "读取删除进度失败")}</p>
        <button onClick={retryPoll} type="button">重试获取进度</button>
      </> : null}
    </>;
  } else {
    content = <>
      <p><span>任务 ID：{phase.status.taskId}</span> <CopyButton text={phase.status.taskId} /></p>
      <StatusDetails status={phase.status} />
      {phase.status.failed > 0 || phase.status.uncertain > 0 || phase.status.stateSyncFailures > 0
        ? <p role="status">失败或不确定项已保留；关闭后可重新检查并明确重试。</p>
        : null}
      <p><a href={`#/audit?task=${encodeURIComponent(phase.status.taskId)}`}>前往删除审计页查看 →</a></p>
      <button onClick={onClose} type="button">关闭</button>
    </>;
  }

  return <Modal onClose={onClose} open title="确认删除">
    {content}
  </Modal>;
}

function OpenDeleteDialogSession({
  memberIds,
  selectionKey,
  initialTaskId,
  api,
  keptMembers,
  onClose,
  onAccepted,
  onExecutionRejected,
  onExecutionStarted,
  onTerminal,
  onExecutionLockChange,
  onExecutionPendingChange
}: OpenDeleteDialogSessionProps) {
  const restoredTaskId = initialTaskId?.trim() || undefined;
  const [locallyAcceptedTaskId, setLocallyAcceptedTaskId] = useState<string>();
  const acceptedTaskId = restoredTaskId ?? locallyAcceptedTaskId;
  const [frozenSelection, setFrozenSelection] = useState<{
    readonly memberIds: number[];
    readonly selectionKey: string;
  }>();
  const mountedRef = useRef(true);
  const onAcceptedRef = useRef(onAccepted);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  useEffect(() => {
    onAcceptedRef.current = onAccepted;
  }, [onAccepted]);

  if (acceptedTaskId) {
    return <DeleteTaskSession
      api={api}
      key={acceptedTaskId}
      onClose={onClose}
      onTerminal={onTerminal}
      taskId={acceptedTaskId}
    />;
  }

  const confirmationSelection = frozenSelection ?? { memberIds, selectionKey };
  return <DeleteConfirmationSession
    api={api}
    keptMembers={keptMembers ?? []}
    key={confirmationSelection.selectionKey}
    memberIds={confirmationSelection.memberIds}
    onClose={onClose}
    onExecutionRejected={() => {
      setFrozenSelection(undefined);
      onExecutionRejected?.();
    }}
    onExecutionStarted={(executingMemberIds, executingSelectionKey) => {
      setFrozenSelection({
        memberIds: executingMemberIds,
        selectionKey: executingSelectionKey
      });
      onExecutionStarted?.(executingMemberIds);
    }}
    onExecutionLockChange={onExecutionLockChange}
    onExecutionPendingChange={onExecutionPendingChange}
    onReprepareRequested={() => setFrozenSelection(undefined)}
    onTaskAccepted={taskId => {
      onAcceptedRef.current?.(taskId);
      if (mountedRef.current) setLocallyAcceptedTaskId(taskId);
    }}
    selectionKey={confirmationSelection.selectionKey}
  />;
}

export function DeleteDialog({
  open,
  memberIds,
  initialTaskId,
  api = appApi,
  keptMembers,
  onClose,
  onAccepted,
  onExecutionRejected,
  onExecutionStarted,
  onTerminal,
  onExecutionLockChange,
  onExecutionPendingChange
}: DeleteDialogProps) {
  if (!open) return null;
  const normalizedMemberIds = normalizeMemberIds(memberIds);
  const selectionKey = normalizedMemberIds.join(",");
  return <OpenDeleteDialogSession
    api={api}
    initialTaskId={initialTaskId}
    keptMembers={keptMembers}
    memberIds={normalizedMemberIds}
    onAccepted={onAccepted}
    onClose={onClose}
    onExecutionRejected={onExecutionRejected}
    onExecutionStarted={onExecutionStarted}
    onExecutionLockChange={onExecutionLockChange}
    onExecutionPendingChange={onExecutionPendingChange}
    onTerminal={onTerminal}
    selectionKey={selectionKey}
  />;
}
