export type GroupKind = "exact" | "image" | "video";
export type DeleteMode = "soft" | "hard";
export type GroupSort = "members_desc" | "newest" | "reclaim_desc";
export type AgentIdentityState = "pending" | "claimed" | "conflict";

export interface AgentStatus {
  machineId: string;
  addr: string;
  online: boolean;
  identityState: AgentIdentityState;
  lastErr?: string;
}

/**
 * 最近处理的文件条目（proto.FeatureItem 的 JSON 形状）。
 * 后端 FeatureItem 只有 msgpack tag，encoding/json 回退为 Go 字段名（PascalCase）。
 */
export interface FeatureItem {
  path: string;
  status: string;
  err?: string;
}

export interface ScanTask {
  taskId: string;
  machineId: string;
  phase: number;
  roots: string[];
  rescan: boolean;
  /** sent/acked/running/done/failed，另有内存中间态 "cancelling"（取消已下发待终态回执）。 */
  status: string;
  /** 取消完成的任务 status=failed 且 ackReason="cancelled"，前端据此显示"已取消"。 */
  ackReason?: string;
  done: number;
  total: number;
  skipped: number;
  failed: number;
  scanErrors: number;
  elapsedMs: number;
  speed: number;
  lastErr?: string;
  recent: FeatureItem[];
  lastSeq?: number;
  updatedAt: string;
}

export interface StartScanInput {
  taskId?: string;
  machineId: string;
  roots: string[];
  phase?: number;
  rescan: boolean;
}

export interface FilesystemEntry {
  name: string;
  path: string;
  kind: "drive" | "directory" | "file";
  hidden: boolean;
  system: boolean;
  selectable: boolean;
}

export interface FilesystemPage {
  currentPath: string;
  parentPath: string;
  entries: FilesystemEntry[];
  nextCursor: string;
}

export interface BrowseAgentFilesystemInput {
  path: string;
  showHidden: boolean;
  cursor: string;
  limit: number;
}

export interface AnalysisStats {
  filesScanned: number;
  exactGroups: number;
  exactMembers: number;
  imageFeatures: number;
  imagePairs: number;
  videoFeatures: number;
  videoPairs: number;
  badRows: number;
  skippedPairs: number;
  groupsWritten: number;
  membersWritten: number;
  stageElapsedMs: Record<string, number>;
  heapAllocBytes: number;
}

export interface AnalysisStatus {
  running: boolean;
  last: AnalysisStats | null;
  lastErr: string;
}

export interface GroupSummary {
  id: number;
  kind: GroupKind;
  memberCount: number;
  repMachine: string;
  repPath: string;
  machines: string[];
  createdAt: string;
  totalBytes: number;
  wastedBytes: number;
}

export interface GroupMember {
  fileId: number;
  machineId: string;
  path: string;
  size: number;
  mtime: number;
  score: unknown;
}

export interface GroupQuery {
  kind: GroupKind;
  page: number;
  size: number;
  q?: string;
  machine?: string;
  minMembers?: number;
  sort?: GroupSort;
}

export interface GroupPage {
  kind: GroupKind;
  page: number;
  size: number;
  total: number;
  groups: GroupSummary[];
}

/** 组聚合统计查询（GET /api/groups/stats）：全部可选，kind 缺省聚合全部三类。 */
export interface GroupsStatsQuery {
  kind?: GroupKind;
  q?: string;
  machine?: string;
  minMembers?: number;
}

export interface GroupsStats {
  /** 回显查询 kind；未按 kind 筛选时为空字符串。 */
  kind: GroupKind | "";
  groups: number;
  totalBytes: number;
  wastedBytes: number;
}

export interface GroupDetail {
  id: number;
  kind: GroupKind;
  representativeFileId: number | null;
  memberTotal: number;
  memberPage: number;
  memberSize: number;
  members: GroupMember[];
}

/** 保留策略（POST /api/groups/select-by-strategy）：newest=mtime 最大者保留，
 * oldest=mtime 最小者保留，largest=size 最大者保留，shortest_path=路径最短者保留；
 * 各组的 effective 代表文件永远不在返回的选择集中。 */
export type GroupSelectStrategy = "newest" | "oldest" | "largest" | "shortest_path";

export interface GroupSelectByStrategyInput {
  kind: GroupKind;
  q?: string;
  machine?: string;
  minMembers?: number;
  strategy: GroupSelectStrategy;
  /** 返回条数上限，缺省/上限 50000；超出时 truncated=true。 */
  limit?: number;
}

export interface GroupSelectByStrategyResult {
  fileIds: number[];
  /** 本次筛选命中并参与策略计算的组数。 */
  groups: number;
  truncated: boolean;
}

export interface DeleteSummary {
  totalFiles: number;
  totalBytes: number;
  byMachine: Record<string, number>;
  samples: string[];
}

export interface DeletePreparation {
  confirmToken: string;
  expiresInSeconds: number;
  summary: DeleteSummary;
}

export interface DeleteSequenceStatus {
  sequence: number;
  lastSeq: number;
  received: boolean;
  total: number;
  ok: number;
  failed: number;
  uncertain: number;
}

export interface DeleteMachineStatus {
  machineId: string;
  total: number;
  ok: number;
  failed: number;
  uncertain: number;
  pending: number;
  complete: boolean;
  stateSyncFailures: number;
  sequences: Record<string, DeleteSequenceStatus>;
  /** 软删除时各源路径对应的回收去向；硬删除或无回收时缺省。 */
  recycledTo?: Record<string, string>;
}

export interface DeleteProblem {
  machineId: string;
  sequence: number;
  path: string;
  errorCode?: string;
  errorMessage?: string;
  uncertain: boolean;
  stateSyncErr?: string;
}

export interface DeleteTaskStatus {
  taskId: string;
  mode: DeleteMode;
  total: number;
  ok: number;
  failed: number;
  uncertain: number;
  pending: number;
  complete: boolean;
  stateSyncFailures: number;
  byMachine: Record<string, DeleteMachineStatus>;
  errorCodes: Record<string, number>;
  problems: DeleteProblem[];
}

/** 删除任务列表条目（GET /api/delete/tasks）：仅计数摘要，无问题明细。 */
export interface DeleteTaskSummary {
  taskId: string;
  mode: DeleteMode;
  total: number;
  ok: number;
  failed: number;
  uncertain: number;
  pending: number;
  complete: boolean;
  createdAt: string;
}

export interface GUIAgentConfig {
  addr: string;
}

export interface GUIFirstScreenConfig {
  hammingMax: number;
  aspectTolerance: number;
  videoDurationWindowMs: number;
  imageQualityMin: number;
  readPageSize: number;
  groupInsertBatch: number;
  shaResolveChunk: number;
}

export interface GUIPhase2Config {
  phashPassT2: number;
  phashPartThreshold: number;
  sobelT3: number;
  videoFrames: number;
  videoAvgT4: number;
  videoMinPassed: number;
  videoMinValid: number;
  videoFileTimeoutS: number;
  videoFrameCommandTimeoutS: number;
  imageFileTimeoutS: number;
  taskShardSize: number;
  autoDispatch: boolean;
}

export interface GUIConfig {
  listenAddr: string;
  pgDsn: string;
  agents: GUIAgentConfig[];
  heartbeatS: number;
  firstScreen: GUIFirstScreenConfig;
  phase2: GUIPhase2Config;
}

export interface GUIConfigSnapshot {
  config: GUIConfig;
  restartRequired: boolean;
}

export interface GUIConfigSaveResult {
  saved: boolean;
  restartRequired: boolean;
  restarting: boolean;
  recoveryURL: string;
}

export type RuntimeDatabaseState = "connecting" | "connected" | "error";

export interface RuntimeStatus {
  databaseState: RuntimeDatabaseState;
  databaseErrorCode: string;
  agents: AgentStatus[];
  restarting: boolean;
  recoveryURL: string;
}

export interface ConfigFieldError {
  field: string;
  code: string;
  message: string;
}

export interface AppApi {
  listAgents(signal?: AbortSignal): Promise<AgentStatus[]>;
  listTasks(signal?: AbortSignal): Promise<ScanTask[]>;
  startScan(input: StartScanInput, signal?: AbortSignal): Promise<{ taskId: string }>;
  browseAgentFilesystem(machineID: string, input: BrowseAgentFilesystemInput, signal?: AbortSignal): Promise<FilesystemPage>;
  getAnalysisStatus(signal?: AbortSignal): Promise<AnalysisStatus>;
  runAnalysis(signal?: AbortSignal): Promise<void>;
  listGroups(query: GroupQuery, signal?: AbortSignal): Promise<GroupPage>;
  getGroupsStats(query?: GroupsStatsQuery, signal?: AbortSignal): Promise<GroupsStats>;
  getGroup(id: number, memberPage: number, memberSize: number, signal?: AbortSignal): Promise<GroupDetail>;
  /** 指定组的保留副本（POST /api/groups/{id}/representative）；成功后调用方应刷新组详情。 */
  setGroupRepresentative(groupId: number, fileId: number, signal?: AbortSignal): Promise<void>;
  /** 按保留策略批量选择应删除的成员（POST /api/groups/select-by-strategy）。 */
  selectGroupsByStrategy(input: GroupSelectByStrategyInput, signal?: AbortSignal): Promise<GroupSelectByStrategyResult>;
  /** 取消扫描任务（POST /api/tasks/{id}/cancel）：任务不存在 404、已终态 409、Agent 离线 503。 */
  cancelTask(taskId: string, signal?: AbortSignal): Promise<void>;
  /** 取消当前运行的分析（POST /api/analysis/firstscreen/cancel）；无运行中任务 409。 */
  cancelAnalysis(signal?: AbortSignal): Promise<void>;
  prepareDelete(memberIds: number[], signal?: AbortSignal): Promise<DeletePreparation>;
  executeDelete(confirmToken: string, mode: DeleteMode, signal?: AbortSignal): Promise<{ taskId: string }>;
  getDeleteStatus(taskId: string, signal?: AbortSignal): Promise<DeleteTaskStatus>;
  listDeleteTasks(signal?: AbortSignal): Promise<DeleteTaskSummary[]>;
  loadGUIConfig(signal?: AbortSignal): Promise<GUIConfigSnapshot>;
  saveGUIConfig(config: GUIConfig, signal?: AbortSignal): Promise<GUIConfigSaveResult>;
  getRuntimeStatus(signal?: AbortSignal): Promise<RuntimeStatus>;
}
