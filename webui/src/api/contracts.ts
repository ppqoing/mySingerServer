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

export interface ScanTask {
  taskId: string;
  machineId: string;
  phase: number;
  roots: string[];
  rescan: boolean;
  status: string;
  ackReason?: string;
  done: number;
  total: number;
  skipped: number;
  failed: number;
  scanErrors: number;
  elapsedMs: number;
  speed: number;
  lastErr?: string;
  recent: unknown[];
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

export interface GroupDetail {
  id: number;
  kind: GroupKind;
  representativeFileId: number | null;
  memberTotal: number;
  memberPage: number;
  memberSize: number;
  members: GroupMember[];
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
  getAnalysisStatus(signal?: AbortSignal): Promise<AnalysisStatus>;
  runAnalysis(signal?: AbortSignal): Promise<void>;
  listGroups(query: GroupQuery, signal?: AbortSignal): Promise<GroupPage>;
  getGroup(id: number, memberPage: number, memberSize: number, signal?: AbortSignal): Promise<GroupDetail>;
  prepareDelete(memberIds: number[], signal?: AbortSignal): Promise<DeletePreparation>;
  executeDelete(confirmToken: string, mode: DeleteMode, signal?: AbortSignal): Promise<{ taskId: string }>;
  getDeleteStatus(taskId: string, signal?: AbortSignal): Promise<DeleteTaskStatus>;
  loadGUIConfig(signal?: AbortSignal): Promise<GUIConfigSnapshot>;
  saveGUIConfig(config: GUIConfig, signal?: AbortSignal): Promise<GUIConfigSaveResult>;
}
