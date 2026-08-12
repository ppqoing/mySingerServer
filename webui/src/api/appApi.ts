import { ApiError, isAbortError, requestJson, requestVoid } from "./client";
import type {
  AgentStatus,
  AnalysisStats,
  AnalysisStatus,
  AppApi,
  DeleteMachineStatus,
  DeleteMode,
  DeletePreparation,
  DeleteProblem,
  DeleteSequenceStatus,
  DeleteSummary,
  DeleteTaskStatus,
  ConfigFieldError,
  GUIConfig,
  GUIConfigSaveResult,
  GUIConfigSnapshot,
  GroupDetail,
  GroupKind,
  GroupMember,
  GroupPage,
  GroupQuery,
  GroupSummary,
  RuntimeStatus,
  ScanTask,
  StartScanInput
} from "./contracts";

export type { AppApi } from "./contracts";

export class GUIConfigValidationError extends ApiError {
  readonly fields: readonly ConfigFieldError[];

  constructor(fields: readonly ConfigFieldError[]) {
    super(400, "配置校验失败", false);
    this.name = "GUIConfigValidationError";
    this.fields = fields;
  }
}

export function createAppApi(): AppApi {
  return {
    listAgents: signal => requestJson("/api/agents", get(signal), agents),
    listTasks: signal => requestJson("/api/tasks", get(signal), tasks),
    startScan: (input, signal) => requestJson("/api/scan", jsonPost(scanInput(input), signal), taskId),
    getAnalysisStatus: signal => requestJson(
      "/api/analysis/firstscreen/status",
      { ...get(signal), decodeStatuses: [503] },
      analysisStatus
    ),
    runAnalysis: signal => requestVoid(
      "/api/analysis/firstscreen/run",
      { method: "POST", signal, allowNoContent: true }
    ),
    listGroups: (query, signal) => requestJson(groupListUrl(query), get(signal), groupPage),
    getGroup: (id, memberPage, memberSize, signal) => requestJson(
      `/api/groups/${positiveInteger(id, "group id")}?${new URLSearchParams({
        member_page: String(positiveInteger(memberPage, "member page")),
        member_size: String(positiveInteger(memberSize, "member size"))
      })}`,
      get(signal),
      groupDetail
    ),
    prepareDelete: (memberIds, signal) => requestJson(
      "/api/delete/prepare",
      jsonPost({ member_ids: normalizedMemberIds(memberIds) }, signal),
      deletePreparation
    ),
    executeDelete: (confirmToken, mode, signal) => requestJson(
      "/api/delete/execute",
      jsonPost({ confirm_token: requiredText(confirmToken, "confirm token"), mode }, signal),
      taskId
    ),
    getDeleteStatus: (taskIdValue, signal) => requestJson(
      `/api/delete/tasks/${encodeURIComponent(requiredText(taskIdValue, "task id"))}`,
      get(signal),
      deleteTaskStatus
    ),
    loadGUIConfig: signal => requestJson("/api/config", get(signal), guiConfigSnapshot),
    saveGUIConfig: (configValue, signal) => requestJson(
      "/api/config",
      { ...jsonPut(guiConfigInput(configValue), signal), decodeStatuses: [400, 500] },
      guiConfigSaveResponse
    ),
    getRuntimeStatus: signal => requestJson("/api/runtime/status", get(signal), runtimeStatus)
  };
}

export class GUIConfigRestartError extends ApiError {
  readonly saved: boolean;
  readonly restartRequired: boolean;

  constructor(saved: boolean, restartRequired: boolean) {
    super(500, "restart_launch_failed", false);
    this.name = "GUIConfigRestartError";
    this.saved = saved;
    this.restartRequired = restartRequired;
  }
}

export const appApi: AppApi = createAppApi();

const managerRecoveryPollIntervalMs = 250;
const managerRecoveryTimeoutMs = 30_000;

export async function waitForManager(recoveryURL: string, signal?: AbortSignal): Promise<void> {
  const expectedRestartToken = new URL(recoveryURL).searchParams.get("restart_token");
  const healthURL = recoveryURL;
  const controller = new AbortController();
  let timedOut = false;
  const timeout = setTimeout(() => { timedOut = true; controller.abort(); }, managerRecoveryTimeoutMs);
  const abort = () => controller.abort();
  signal?.addEventListener("abort", abort, { once: true });
  if (signal?.aborted) controller.abort();
  try {
    while (true) {
      try {
        const response = await fetch(healthURL, { credentials: "omit", signal: controller.signal });
        if (response.ok && await recoveryHealthReady(response, expectedRestartToken)) return;
      } catch (error) {
        if (timedOut) throw new Error("Manager restart timed out", { cause: error });
        if (isAbortError(error) || signal?.aborted) throw error;
      }
      await waitFor(managerRecoveryPollIntervalMs, controller.signal);
    }
  } catch (error) {
    if (timedOut) throw new Error("Manager restart timed out", { cause: error });
    throw error;
  } finally {
    clearTimeout(timeout);
    signal?.removeEventListener("abort", abort);
  }
}

async function recoveryHealthReady(response: Response, expectedRestartToken: string | null): Promise<boolean> {
  try {
    const value: unknown = await response.json();
    if (typeof value !== "object" || value === null || Array.isArray(value)) return false;
    const health = value as { ok?: unknown; restart_token?: unknown; restarting?: unknown };
    return expectedRestartToken !== null && expectedRestartToken !== "" &&
      health.ok === true && health.restart_token === expectedRestartToken && health.restarting === false;
  } catch {
    return false;
  }
}

function waitFor(milliseconds: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(complete, milliseconds);
    const abort = () => {
      clearTimeout(timer);
      reject(new DOMException("The operation was aborted", "AbortError"));
    };
    function complete() {
      signal?.removeEventListener("abort", abort);
      resolve();
    }
    if (signal) signal.addEventListener("abort", abort, { once: true });
  });
}

function get(signal?: AbortSignal): RequestInit {
  return { method: "GET", body: undefined, signal };
}

function jsonPost(body: unknown, signal?: AbortSignal): RequestInit {
  return {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
    signal
  };
}

function jsonPut(body: unknown, signal?: AbortSignal): RequestInit {
  return {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
    signal
  };
}

function scanInput(input: StartScanInput): Record<string, unknown> {
  const roots = input.roots.map(root => requiredText(root, "scan root"));
  const body: Record<string, unknown> = {
    machine_id: requiredText(input.machineId, "machine id"),
    roots,
    rescan: input.rescan
  };
  if (roots.length === 0) {
    throw new Error("at least one scan root is required");
  }
  if (input.taskId !== undefined) {
    body.task_id = requiredText(input.taskId, "task id");
  }
  if (input.phase !== undefined) {
    body.phase = positiveInteger(input.phase, "phase");
  }
  return body;
}

function groupListUrl(query: GroupQuery): string {
  const params = new URLSearchParams({
    kind: groupKind(query.kind),
    page: String(positiveInteger(query.page, "page")),
    size: String(positiveInteger(query.size, "size"))
  });
  if (query.q !== undefined) {
    params.set("q", query.q);
  }
  if (query.machine !== undefined) {
    params.set("machine", query.machine);
  }
  if (query.minMembers !== undefined) {
    params.set("min_members", String(positiveInteger(query.minMembers, "min members")));
  }
  if (query.sort !== undefined) {
    params.set("sort", query.sort);
  }
  return `/api/groups?${params}`;
}

function normalizedMemberIds(memberIds: number[]): number[] {
  if (memberIds.length === 0) {
    throw new Error("at least one member id is required");
  }
  return [...new Set(memberIds.map(id => positiveInteger(id, "member id")))].sort((left, right) => left - right);
}

function agents(value: unknown): AgentStatus[] {
  return array(value, "agents").map(agent => {
    const raw = record(agent, "agent");
    return {
      machineId: text(raw.machine_id, "agent.machine_id"),
      addr: text(raw.addr, "agent.addr"),
      online: boolean(raw.online, "agent.online"),
      identityState: agentIdentityState(raw.identity_state),
      lastErr: optionalText(raw.last_err, "agent.last_err")
    };
  });
}

function tasks(value: unknown): ScanTask[] {
  return array(value, "tasks").map(task => {
    const raw = record(task, "task");
    return {
      taskId: text(raw.task_id, "task.task_id"),
      machineId: text(raw.machine_id, "task.machine_id"),
      phase: number(raw.phase, "task.phase"),
      roots: stringArray(raw.roots, "task.roots"),
      rescan: boolean(raw.rescan, "task.rescan"),
      status: text(raw.status, "task.status"),
      ackReason: optionalText(raw.ack_reason, "task.ack_reason"),
      done: number(raw.done, "task.done"),
      total: number(raw.total, "task.total"),
      skipped: number(raw.skipped, "task.skipped"),
      failed: number(raw.failed, "task.failed"),
      scanErrors: number(raw.scan_errors, "task.scan_errors"),
      elapsedMs: number(raw.elapsed_ms, "task.elapsed_ms"),
      speed: number(raw.speed, "task.speed"),
      lastErr: optionalText(raw.last_err, "task.last_err"),
      recent: nullableArray(raw.recent, "task.recent"),
      lastSeq: optionalNumber(raw.last_seq, "task.last_seq"),
      updatedAt: text(raw.updated_at, "task.updated_at")
    };
  });
}

function taskId(value: unknown): { taskId: string } {
  return { taskId: text(record(value, "task response").task_id, "task_id") };
}

function analysisStatus(value: unknown): AnalysisStatus {
  const raw = record(value, "analysis status");
  return {
    running: boolean(raw.running, "analysis.running"),
    last: raw.last === null ? null : analysisStats(raw.last),
    lastErr: text(raw.last_err, "analysis.last_err")
  };
}

function analysisStats(value: unknown): AnalysisStats {
  const raw = record(value, "analysis stats");
  return {
    filesScanned: number(raw.files_scanned, "stats.files_scanned"),
    exactGroups: number(raw.exact_groups, "stats.exact_groups"),
    exactMembers: number(raw.exact_members, "stats.exact_members"),
    imageFeatures: number(raw.image_features, "stats.image_features"),
    imagePairs: number(raw.image_pairs, "stats.image_pairs"),
    videoFeatures: number(raw.video_features, "stats.video_features"),
    videoPairs: number(raw.video_pairs, "stats.video_pairs"),
    badRows: number(raw.bad_rows, "stats.bad_rows"),
    skippedPairs: number(raw.skipped_pairs, "stats.skipped_pairs"),
    groupsWritten: number(raw.groups_written, "stats.groups_written"),
    membersWritten: number(raw.members_written, "stats.members_written"),
    stageElapsedMs: numberMap(raw.stage_elapsed_ms, "stats.stage_elapsed_ms"),
    heapAllocBytes: number(raw.heap_alloc_bytes, "stats.heap_alloc_bytes")
  };
}

function groupPage(value: unknown): GroupPage {
  const raw = record(value, "group page");
  return {
    kind: groupKind(raw.kind),
    page: number(raw.page, "groups.page"),
    size: number(raw.size, "groups.size"),
    total: number(raw.total, "groups.total"),
    groups: array(raw.groups, "groups.groups").map(groupSummary)
  };
}

function groupSummary(value: unknown): GroupSummary {
  const raw = record(value, "group summary");
  return {
    id: number(raw.id, "group.id"),
    kind: groupKind(raw.kind),
    memberCount: number(raw.member_count, "group.member_count"),
    repMachine: text(raw.rep_machine, "group.rep_machine"),
    repPath: text(raw.rep_path, "group.rep_path"),
    machines: stringArray(raw.machines, "group.machines"),
    createdAt: text(raw.created_at, "group.created_at"),
    totalBytes: number(raw.total_bytes, "group.total_bytes"),
    wastedBytes: number(raw.wasted_bytes, "group.wasted_bytes")
  };
}

function groupDetail(value: unknown): GroupDetail {
  const raw = record(value, "group detail");
  return {
    id: number(raw.id, "detail.id"),
    kind: groupKind(raw.kind),
    representativeFileId: raw.representative_file_id === null
      ? null
      : number(raw.representative_file_id, "detail.representative_file_id"),
    memberTotal: number(raw.member_total, "detail.member_total"),
    memberPage: number(raw.member_page, "detail.member_page"),
    memberSize: number(raw.member_size, "detail.member_size"),
    members: array(raw.members, "detail.members").map(groupMember)
  };
}

function groupMember(value: unknown): GroupMember {
  const raw = record(value, "group member");
  return {
    fileId: number(raw.file_id, "member.file_id"),
    machineId: text(raw.machine_id, "member.machine_id"),
    path: text(raw.path, "member.path"),
    size: number(raw.size, "member.size"),
    mtime: number(raw.mtime, "member.mtime"),
    score: raw.score_json
  };
}

function deletePreparation(value: unknown): DeletePreparation {
  const raw = record(value, "delete preparation");
  return {
    confirmToken: text(raw.confirm_token, "confirm_token"),
    expiresInSeconds: number(raw.expires_in_seconds, "expires_in_seconds"),
    summary: deleteSummary(raw.summary)
  };
}

function deleteSummary(value: unknown): DeleteSummary {
  const raw = record(value, "delete summary");
  return {
    totalFiles: number(raw.total_files, "summary.total_files"),
    totalBytes: number(raw.total_bytes, "summary.total_bytes"),
    byMachine: numberMap(raw.by_machine, "summary.by_machine"),
    samples: stringArray(raw.samples, "summary.samples")
  };
}

function deleteTaskStatus(value: unknown): DeleteTaskStatus {
  const raw = record(value, "delete task status");
  return {
    taskId: text(raw.task_id, "delete.task_id"),
    mode: deleteMode(raw.mode),
    total: number(raw.total, "delete.total"),
    ok: number(raw.ok, "delete.ok"),
    failed: number(raw.failed, "delete.failed"),
    uncertain: number(raw.uncertain, "delete.uncertain"),
    pending: number(raw.pending, "delete.pending"),
    complete: boolean(raw.complete, "delete.complete"),
    stateSyncFailures: number(raw.state_sync_failures, "delete.state_sync_failures"),
    byMachine: objectMap(raw.by_machine, deleteMachineStatus, "delete.by_machine"),
    errorCodes: numberMap(raw.error_codes, "delete.error_codes"),
    problems: nullableArray(raw.problems, "delete.problems").map(deleteProblem)
  };
}

function guiConfigSnapshot(value: unknown): GUIConfigSnapshot {
  const raw = record(value, "GUI config snapshot");
  return {
    config: guiConfig(raw.config),
    restartRequired: boolean(raw.restart_required, "GUI config restart_required")
  };
}

function guiConfig(value: unknown): GUIConfig {
  const raw = record(value, "GUI config");
  const firstScreen = record(raw.firstscreen, "GUI config firstscreen");
  const phase2 = record(raw.phase2, "GUI config phase2");
  return {
    listenAddr: text(raw.listen_addr, "GUI config listen_addr"),
    pgDsn: text(raw.pg_dsn, "GUI config pg_dsn"),
    agents: array(raw.agents, "GUI config agents").map((value, index) => {
      const agent = record(value, `GUI config agents[${index}]`);
      return {
        addr: text(agent.addr, `GUI config agents[${index}].addr`)
      };
    }),
    heartbeatS: number(raw.heartbeat_s, "GUI config heartbeat_s"),
    firstScreen: {
      hammingMax: number(firstScreen.hamming_max, "GUI config firstscreen.hamming_max"),
      aspectTolerance: number(firstScreen.aspect_tolerance, "GUI config firstscreen.aspect_tolerance"),
      videoDurationWindowMs: number(
        firstScreen.video_duration_window_ms,
        "GUI config firstscreen.video_duration_window_ms"
      ),
      imageQualityMin: number(firstScreen.image_quality_min, "GUI config firstscreen.image_quality_min"),
      readPageSize: number(firstScreen.read_page_size, "GUI config firstscreen.read_page_size"),
      groupInsertBatch: number(firstScreen.group_insert_batch, "GUI config firstscreen.group_insert_batch"),
      shaResolveChunk: number(firstScreen.sha_resolve_chunk, "GUI config firstscreen.sha_resolve_chunk")
    },
    phase2: {
      phashPassT2: number(phase2.phash_pass_t2, "GUI config phase2.phash_pass_t2"),
      phashPartThreshold: number(phase2.phash_part_threshold, "GUI config phase2.phash_part_threshold"),
      sobelT3: number(phase2.sobel_t3, "GUI config phase2.sobel_t3"),
      videoFrames: number(phase2.video_frames, "GUI config phase2.video_frames"),
      videoAvgT4: number(phase2.video_avg_t4, "GUI config phase2.video_avg_t4"),
      videoMinPassed: number(phase2.video_min_passed, "GUI config phase2.video_min_passed"),
      videoMinValid: number(phase2.video_min_valid, "GUI config phase2.video_min_valid"),
      videoFileTimeoutS: number(phase2.video_file_timeout_s, "GUI config phase2.video_file_timeout_s"),
      videoFrameCommandTimeoutS: number(
        phase2.video_frame_command_timeout_s,
        "GUI config phase2.video_frame_command_timeout_s"
      ),
      imageFileTimeoutS: number(phase2.image_file_timeout_s, "GUI config phase2.image_file_timeout_s"),
      taskShardSize: number(phase2.task_shard_size, "GUI config phase2.task_shard_size"),
      autoDispatch: boolean(phase2.auto_dispatch, "GUI config phase2.auto_dispatch")
    }
  };
}

function guiConfigInput(value: GUIConfig): Record<string, unknown> {
  return {
    listen_addr: value.listenAddr,
    pg_dsn: value.pgDsn,
    agents: value.agents.map(agent => ({ addr: agent.addr })),
    heartbeat_s: value.heartbeatS,
    firstscreen: {
      hamming_max: value.firstScreen.hammingMax,
      aspect_tolerance: value.firstScreen.aspectTolerance,
      video_duration_window_ms: value.firstScreen.videoDurationWindowMs,
      image_quality_min: value.firstScreen.imageQualityMin,
      read_page_size: value.firstScreen.readPageSize,
      group_insert_batch: value.firstScreen.groupInsertBatch,
      sha_resolve_chunk: value.firstScreen.shaResolveChunk
    },
    phase2: {
      phash_pass_t2: value.phase2.phashPassT2,
      phash_part_threshold: value.phase2.phashPartThreshold,
      sobel_t3: value.phase2.sobelT3,
      video_frames: value.phase2.videoFrames,
      video_avg_t4: value.phase2.videoAvgT4,
      video_min_passed: value.phase2.videoMinPassed,
      video_min_valid: value.phase2.videoMinValid,
      video_file_timeout_s: value.phase2.videoFileTimeoutS,
      video_frame_command_timeout_s: value.phase2.videoFrameCommandTimeoutS,
      image_file_timeout_s: value.phase2.imageFileTimeoutS,
      task_shard_size: value.phase2.taskShardSize,
      auto_dispatch: value.phase2.autoDispatch
    }
  };
}

function guiConfigSaveResponse(value: unknown, status = httpStatusOK): GUIConfigSaveResult {
  const raw = record(value, "GUI config save response");
  if (raw.error === "config_invalid") {
    throw new GUIConfigValidationError(configFieldErrors(raw.fields));
  }
  if (raw.error === "restart_launch_failed") {
    throw new GUIConfigRestartError(
      boolean(raw.saved, "GUI config saved"),
      boolean(raw.restart_required, "GUI config restart_required")
    );
  }
  if (typeof raw.error === "string") {
    throw new ApiError(status, raw.error, status >= 500);
  }
  return {
    saved: boolean(raw.saved, "GUI config saved"),
    restartRequired: boolean(raw.restart_required, "GUI config restart_required"),
    restarting: boolean(raw.restarting, "GUI config restarting"),
    recoveryURL: text(raw.recovery_url, "GUI config recovery_url")
  };
}

const httpStatusOK = 200;

function runtimeStatus(value: unknown): RuntimeStatus {
  const raw = record(value, "runtime status");
  return {
    databaseState: runtimeDatabaseState(raw.database_state),
    databaseErrorCode: text(raw.database_error_code, "runtime database_error_code"),
    agents: agents(raw.agents),
    restarting: boolean(raw.restarting, "runtime restarting"),
    recoveryURL: text(raw.recovery_url, "runtime recovery_url")
  };
}

function configFieldErrors(value: unknown): ConfigFieldError[] {
  return array(value, "GUI config fields").map((item, index) => {
    const raw = record(item, `GUI config fields[${index}]`);
    return {
      field: text(raw.field, `GUI config fields[${index}].field`),
      code: text(raw.code, `GUI config fields[${index}].code`),
      message: text(raw.message, `GUI config fields[${index}].message`)
    };
  });
}

function deleteMachineStatus(value: unknown): DeleteMachineStatus {
  const raw = record(value, "delete machine status");
  return {
    machineId: text(raw.machine_id, "machine.machine_id"),
    total: number(raw.total, "machine.total"),
    ok: number(raw.ok, "machine.ok"),
    failed: number(raw.failed, "machine.failed"),
    uncertain: number(raw.uncertain, "machine.uncertain"),
    pending: number(raw.pending, "machine.pending"),
    complete: boolean(raw.complete, "machine.complete"),
    stateSyncFailures: number(raw.state_sync_failures, "machine.state_sync_failures"),
    sequences: objectMap(raw.sequences, deleteSequenceStatus, "machine.sequences")
  };
}

function deleteSequenceStatus(value: unknown): DeleteSequenceStatus {
  const raw = record(value, "delete sequence status");
  return {
    sequence: number(raw.sequence, "sequence.sequence"),
    lastSeq: number(raw.last_seq, "sequence.last_seq"),
    received: boolean(raw.received, "sequence.received"),
    total: number(raw.total, "sequence.total"),
    ok: number(raw.ok, "sequence.ok"),
    failed: number(raw.failed, "sequence.failed"),
    uncertain: number(raw.uncertain, "sequence.uncertain")
  };
}

function deleteProblem(value: unknown): DeleteProblem {
  const raw = record(value, "delete problem");
  return {
    machineId: text(raw.machine_id, "problem.machine_id"),
    sequence: number(raw.sequence, "problem.sequence"),
    path: text(raw.path, "problem.path"),
    errorCode: optionalText(raw.error_code, "problem.error_code"),
    errorMessage: optionalText(raw.error_message, "problem.error_message"),
    uncertain: boolean(raw.uncertain, "problem.uncertain"),
    stateSyncErr: optionalText(raw.state_sync_err, "problem.state_sync_err")
  };
}

function record(value: unknown, field: string): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new TypeError(`${field} must be an object`);
  }
  return value as Record<string, unknown>;
}

function array(value: unknown, field: string): unknown[] {
  if (!Array.isArray(value)) {
    throw new TypeError(`${field} must be an array`);
  }
  return value;
}

function nullableArray(value: unknown, field: string): unknown[] {
  return value === null ? [] : array(value, field);
}

function stringArray(value: unknown, field: string): string[] {
  return array(value, field).map((item, index) => text(item, `${field}[${index}]`));
}

function text(value: unknown, field: string): string {
  if (typeof value !== "string") {
    throw new TypeError(`${field} must be a string`);
  }
  return value;
}

function optionalText(value: unknown, field: string): string | undefined {
  return value === undefined ? undefined : text(value, field);
}

function number(value: unknown, field: string): number {
  if (typeof value !== "number" || !Number.isFinite(value)) {
    throw new TypeError(`${field} must be a finite number`);
  }
  return value;
}

function optionalNumber(value: unknown, field: string): number | undefined {
  return value === undefined ? undefined : number(value, field);
}

function boolean(value: unknown, field: string): boolean {
  if (typeof value !== "boolean") {
    throw new TypeError(`${field} must be a boolean`);
  }
  return value;
}

function numberMap(value: unknown, field: string): Record<string, number> {
  return objectMap(value, item => number(item, field), field);
}

function objectMap<T>(
  value: unknown,
  decode: (item: unknown) => T,
  field: string
): Record<string, T> {
  return Object.fromEntries(Object.entries(record(value, field)).map(([key, item]) => [key, decode(item)]));
}

function groupKind(value: unknown): GroupKind {
  if (value === "exact" || value === "image" || value === "video") {
    return value;
  }
  throw new TypeError("group kind is invalid");
}

function agentIdentityState(value: unknown): AgentStatus["identityState"] {
  if (value === "pending" || value === "claimed" || value === "conflict") {
    return value;
  }
  throw new TypeError("agent.identity_state is invalid");
}

function runtimeDatabaseState(value: unknown): RuntimeStatus["databaseState"] {
  if (value === "connecting" || value === "connected" || value === "error") {
    return value;
  }
  throw new TypeError("runtime database_state is invalid");
}

function deleteMode(value: unknown): DeleteMode {
  if (value === "soft" || value === "hard") {
    return value;
  }
  throw new TypeError("delete mode is invalid");
}

function requiredText(value: string, field: string): string {
  if (value.trim() === "") {
    throw new Error(`${field} is required`);
  }
  return value;
}

function positiveInteger(value: number, field: string): number {
  if (!Number.isSafeInteger(value) || value <= 0) {
    throw new Error(`${field} must be a positive integer`);
  }
  return value;
}
