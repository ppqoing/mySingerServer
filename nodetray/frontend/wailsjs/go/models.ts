export namespace config {

	export class AgentDeleteForm {
	    pipeName: string;
	    maxEntriesPerFrame: number;
	    dialTimeoutMs: number;
	    helloTimeoutS: number;
	    reportTimeoutS: number;

	    static createFrom(source: any = {}) {
	        return new AgentDeleteForm(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.pipeName = source["pipeName"];
	        this.maxEntriesPerFrame = source["maxEntriesPerFrame"];
	        this.dialTimeoutMs = source["dialTimeoutMs"];
	        this.helloTimeoutS = source["helloTimeoutS"];
	        this.reportTimeoutS = source["reportTimeoutS"];
	    }
	}
	export class TuningForm {
	    statsEnabled: boolean;
	    statsIntervalS: number;
	    statsHistoryS: number;
	    pendingBytesMb: number;
	    statsLogMb: number;
	    pprofAddr: string;

	    static createFrom(source: any = {}) {
	        return new TuningForm(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.statsEnabled = source["statsEnabled"];
	        this.statsIntervalS = source["statsIntervalS"];
	        this.statsHistoryS = source["statsHistoryS"];
	        this.pendingBytesMb = source["pendingBytesMb"];
	        this.statsLogMb = source["statsLogMb"];
	        this.pprofAddr = source["pprofAddr"];
	    }
	}
	export class IPCForm {
	    maxFrameMb: number;

	    static createFrom(source: any = {}) {
	        return new IPCForm(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.maxFrameMb = source["maxFrameMb"];
	    }
	}
	export class ThumbForm {
	    cacheDir: string;
	    tileMaxSide: number;
	    probeTimeoutS: number;
	    nativeTimeoutS: number;
	    frameTimeoutS: number;

	    static createFrom(source: any = {}) {
	        return new ThumbForm(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.cacheDir = source["cacheDir"];
	        this.tileMaxSide = source["tileMaxSide"];
	        this.probeTimeoutS = source["probeTimeoutS"];
	        this.nativeTimeoutS = source["nativeTimeoutS"];
	        this.frameTimeoutS = source["frameTimeoutS"];
	    }
	}
	export class PipelineForm {
	    readChunkKb: number;

	    static createFrom(source: any = {}) {
	        return new PipelineForm(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.readChunkKb = source["readChunkKb"];
	    }
	}
	export class WorkerForm {
	    count: number;
	    exePath: string;
	    imageTimeoutS: number;
	    videoTimeoutS: number;
	    imageMemoryMb: number;
	    respawnDelayMs: number;

	    static createFrom(source: any = {}) {
	        return new WorkerForm(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.count = source["count"];
	        this.exePath = source["exePath"];
	        this.imageTimeoutS = source["imageTimeoutS"];
	        this.videoTimeoutS = source["videoTimeoutS"];
	        this.imageMemoryMb = source["imageMemoryMb"];
	        this.respawnDelayMs = source["respawnDelayMs"];
	    }
	}
	export class ProtoForm {
	    heartbeatS: number;

	    static createFrom(source: any = {}) {
	        return new ProtoForm(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.heartbeatS = source["heartbeatS"];
	    }
	}
	export class SyncForm {
	    intervalS: number;
	    triggerRows: number;
	    upsertBatch: number;

	    static createFrom(source: any = {}) {
	        return new SyncForm(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.intervalS = source["intervalS"];
	        this.triggerRows = source["triggerRows"];
	        this.upsertBatch = source["upsertBatch"];
	    }
	}
	export class ScanForm {
	    hddReadBlockMb: number;
	    hddStreamsPerDisk: number;
	    ssdStreamsPerDisk: number;
	    imageMemResidentMb: number;
	    imageTimeoutS: number;
	    videoTimeoutS: number;
	    imageExts: string[];
	    videoExts: string[];

	    static createFrom(source: any = {}) {
	        return new ScanForm(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.hddReadBlockMb = source["hddReadBlockMb"];
	        this.hddStreamsPerDisk = source["hddStreamsPerDisk"];
	        this.ssdStreamsPerDisk = source["ssdStreamsPerDisk"];
	        this.imageMemResidentMb = source["imageMemResidentMb"];
	        this.imageTimeoutS = source["imageTimeoutS"];
	        this.videoTimeoutS = source["videoTimeoutS"];
	        this.imageExts = source["imageExts"];
	        this.videoExts = source["videoExts"];
	    }
	}
	export class DatabaseForm {
	    host: string;
	    port: number;
	    database: string;
	    user: string;
	    password: string;
	    passwordStored: boolean;
	    replacePassword: boolean;
	    sslMode: string;

	    static createFrom(source: any = {}) {
	        return new DatabaseForm(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.host = source["host"];
	        this.port = source["port"];
	        this.database = source["database"];
	        this.user = source["user"];
	        this.password = source["password"];
	        this.passwordStored = source["passwordStored"];
	        this.replacePassword = source["replacePassword"];
	        this.sslMode = source["sslMode"];
	    }
	}
	export class AgentForm {
	    listenHost: string;
	    listenPort: number;
	    dataDir: string;
	    database: DatabaseForm;
	    useEverything: boolean;
	    scan: ScanForm;
	    sync: SyncForm;
	    proto: ProtoForm;
	    worker: WorkerForm;
	    pipeline: PipelineForm;
	    thumb: ThumbForm;
	    ipc: IPCForm;
	    delete: AgentDeleteForm;
	    tuning: TuningForm;

	    static createFrom(source: any = {}) {
	        return new AgentForm(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.listenHost = source["listenHost"];
	        this.listenPort = source["listenPort"];
	        this.dataDir = source["dataDir"];
	        this.database = this.convertValues(source["database"], DatabaseForm);
	        this.useEverything = source["useEverything"];
	        this.scan = this.convertValues(source["scan"], ScanForm);
	        this.sync = this.convertValues(source["sync"], SyncForm);
	        this.proto = this.convertValues(source["proto"], ProtoForm);
	        this.worker = this.convertValues(source["worker"], WorkerForm);
	        this.pipeline = this.convertValues(source["pipeline"], PipelineForm);
	        this.thumb = this.convertValues(source["thumb"], ThumbForm);
	        this.ipc = this.convertValues(source["ipc"], IPCForm);
	        this.delete = this.convertValues(source["delete"], AgentDeleteForm);
	        this.tuning = this.convertValues(source["tuning"], TuningForm);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

	export class FieldError {
	    field: string;
	    code: string;
	    message: string;

	    static createFrom(source: any = {}) {
	        return new FieldError(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.field = source["field"];
	        this.code = source["code"];
	        this.message = source["message"];
	    }
	}
	export class HelperForm {
	    pipeName: string;
	    allowedRoots: string[];
	    deniedRoots: string[];
	    defaultMode: string;
	    allowHardDelete: boolean;
	    recycleDirName: string;
	    maxEntriesPerFrame: number;
	    frameReadTimeoutSec: number;
	    frameWriteTimeoutSec: number;
	    logDir: string;

	    static createFrom(source: any = {}) {
	        return new HelperForm(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.pipeName = source["pipeName"];
	        this.allowedRoots = source["allowedRoots"];
	        this.deniedRoots = source["deniedRoots"];
	        this.defaultMode = source["defaultMode"];
	        this.allowHardDelete = source["allowHardDelete"];
	        this.recycleDirName = source["recycleDirName"];
	        this.maxEntriesPerFrame = source["maxEntriesPerFrame"];
	        this.frameReadTimeoutSec = source["frameReadTimeoutSec"];
	        this.frameWriteTimeoutSec = source["frameWriteTimeoutSec"];
	        this.logDir = source["logDir"];
	    }
	}








}

export namespace main {

	export class BackendStartup {
	    Ready: boolean;
	    Duplicate: boolean;
	    ErrorCode: string;

	    static createFrom(source: any = {}) {
	        return new BackendStartup(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.Ready = source["Ready"];
	        this.Duplicate = source["Duplicate"];
	        this.ErrorCode = source["ErrorCode"];
	    }
	}

}

export namespace traymodel {

	export class ComponentState {
	    lifecycle: string;
	    healthy: boolean;
	    ready: boolean;
	    pid: number;
	    startedAtUnixMs: number;
	    uptimeSeconds: number;
	    workerReady: number;
	    workerExpected: number;
	    activeRequests: number;
	    errorCode: string;
	    errorSummary: string;
	    needsAttention: boolean;
	    runtimeConfigSha256: string;
	    savedConfigSha256: string;
	    needsRestart: boolean;

	    static createFrom(source: any = {}) {
	        return new ComponentState(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.lifecycle = source["lifecycle"];
	        this.healthy = source["healthy"];
	        this.ready = source["ready"];
	        this.pid = source["pid"];
	        this.startedAtUnixMs = source["startedAtUnixMs"];
	        this.uptimeSeconds = source["uptimeSeconds"];
	        this.workerReady = source["workerReady"];
	        this.workerExpected = source["workerExpected"];
	        this.activeRequests = source["activeRequests"];
	        this.errorCode = source["errorCode"];
	        this.errorSummary = source["errorSummary"];
	        this.needsAttention = source["needsAttention"];
	        this.runtimeConfigSha256 = source["runtimeConfigSha256"];
	        this.savedConfigSha256 = source["savedConfigSha256"];
	        this.needsRestart = source["needsRestart"];
	    }
	}
	export class ConfigApplyResult {
	    ok: boolean;
	    saved: boolean;
	    restarted: boolean;
	    sha256: string;
	    needsRestart: boolean;
	    errorCode: string;
	    errorSummary: string;

	    static createFrom(source: any = {}) {
	        return new ConfigApplyResult(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ok = source["ok"];
	        this.saved = source["saved"];
	        this.restarted = source["restarted"];
	        this.sha256 = source["sha256"];
	        this.needsRestart = source["needsRestart"];
	        this.errorCode = source["errorCode"];
	        this.errorSummary = source["errorSummary"];
	    }
	}
	export class ForceExitResult {
	    ok: boolean;
	    failedComponents: string[];
	    errorCode: string;
	    errorSummary: string;

	    static createFrom(source: any = {}) {
	        return new ForceExitResult(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ok = source["ok"];
	        this.failedComponents = source["failedComponents"];
	        this.errorCode = source["errorCode"];
	        this.errorSummary = source["errorSummary"];
	    }
	}
	export class ImagePreview {
	    ok: boolean;
	    mime: string;
	    width: number;
	    height: number;
	    dataBase64: string;
	    errorCode: string;
	    errorSummary: string;

	    static createFrom(source: any = {}) {
	        return new ImagePreview(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ok = source["ok"];
	        this.mime = source["mime"];
	        this.width = source["width"];
	        this.height = source["height"];
	        this.dataBase64 = source["dataBase64"];
	        this.errorCode = source["errorCode"];
	        this.errorSummary = source["errorSummary"];
	    }
	}
	export class LocalDeleteItem {
	    fileId: number;
	    result: string;
	    errorCode: string;
	    uncertain: boolean;

	    static createFrom(source: any = {}) {
	        return new LocalDeleteItem(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.fileId = source["fileId"];
	        this.result = source["result"];
	        this.errorCode = source["errorCode"];
	        this.uncertain = source["uncertain"];
	    }
	}
	export class LocalDeleteBatch {
	    ok: boolean;
	    batchId: string;
	    status: string;
	    requested: number;
	    succeeded: number;
	    failed: number;
	    uncertain: number;
	    items: LocalDeleteItem[];
	    errorCode: string;
	    errorSummary: string;

	    static createFrom(source: any = {}) {
	        return new LocalDeleteBatch(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ok = source["ok"];
	        this.batchId = source["batchId"];
	        this.status = source["status"];
	        this.requested = source["requested"];
	        this.succeeded = source["succeeded"];
	        this.failed = source["failed"];
	        this.uncertain = source["uncertain"];
	        this.items = this.convertValues(source["items"], LocalDeleteItem);
	        this.errorCode = source["errorCode"];
	        this.errorSummary = source["errorSummary"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class LocalDeleteExecute {
	    batchId: string;
	    selectionDigest: string;

	    static createFrom(source: any = {}) {
	        return new LocalDeleteExecute(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.batchId = source["batchId"];
	        this.selectionDigest = source["selectionDigest"];
	    }
	}
	export class LocalDeleteFile {
	    fileId: number;
	    path: string;
	    size: number;

	    static createFrom(source: any = {}) {
	        return new LocalDeleteFile(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.fileId = source["fileId"];
	        this.path = source["path"];
	        this.size = source["size"];
	    }
	}

	export class LocalDeletePrepare {
	    runId: string;
	    groupId: string;

	    static createFrom(source: any = {}) {
	        return new LocalDeletePrepare(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.runId = source["runId"];
	        this.groupId = source["groupId"];
	    }
	}
	export class LocalDeletePreview {
	    ok: boolean;
	    batchId: string;
	    selectionDigest: string;
	    count: number;
	    totalSize: number;
	    expiresAt: number;
	    files: LocalDeleteFile[];
	    errorCode: string;
	    errorSummary: string;

	    static createFrom(source: any = {}) {
	        return new LocalDeletePreview(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ok = source["ok"];
	        this.batchId = source["batchId"];
	        this.selectionDigest = source["selectionDigest"];
	        this.count = source["count"];
	        this.totalSize = source["totalSize"];
	        this.expiresAt = source["expiresAt"];
	        this.files = this.convertValues(source["files"], LocalDeleteFile);
	        this.errorCode = source["errorCode"];
	        this.errorSummary = source["errorSummary"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class LocalGroupMember {
	    fileId: number;
	    path: string;
	    fileName: string;
	    size: number;
	    status: string;
	    decision: string;

	    static createFrom(source: any = {}) {
	        return new LocalGroupMember(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.fileId = source["fileId"];
	        this.path = source["path"];
	        this.fileName = source["fileName"];
	        this.size = source["size"];
	        this.status = source["status"];
	        this.decision = source["decision"];
	    }
	}
	export class LocalGroup {
	    runId: string;
	    generation: number;
	    groupId: string;
	    category: string;
	    verdict: string;
	    reviewStatus: string;
	    stageOne: string;
	    stageTwo: string;
	    stageThree: string;
	    members: LocalGroupMember[];

	    static createFrom(source: any = {}) {
	        return new LocalGroup(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.runId = source["runId"];
	        this.generation = source["generation"];
	        this.groupId = source["groupId"];
	        this.category = source["category"];
	        this.verdict = source["verdict"];
	        this.reviewStatus = source["reviewStatus"];
	        this.stageOne = source["stageOne"];
	        this.stageTwo = source["stageTwo"];
	        this.stageThree = source["stageThree"];
	        this.members = this.convertValues(source["members"], LocalGroupMember);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

	export class LocalGroupPage {
	    ok: boolean;
	    groups: LocalGroup[];
	    offset: number;
	    nextOffset: number;
	    errorCode: string;
	    errorSummary: string;

	    static createFrom(source: any = {}) {
	        return new LocalGroupPage(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ok = source["ok"];
	        this.groups = this.convertValues(source["groups"], LocalGroup);
	        this.offset = source["offset"];
	        this.nextOffset = source["nextOffset"];
	        this.errorCode = source["errorCode"];
	        this.errorSummary = source["errorSummary"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class LocalGroupQuery {
	    scope: string;
	    runId: string;
	    category: string;
	    pathContains: string;
	    fileNameContains: string;
	    reviewStatus: string;
	    offset: number;
	    limit: number;

	    static createFrom(source: any = {}) {
	        return new LocalGroupQuery(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.scope = source["scope"];
	        this.runId = source["runId"];
	        this.category = source["category"];
	        this.pathContains = source["pathContains"];
	        this.fileNameContains = source["fileNameContains"];
	        this.reviewStatus = source["reviewStatus"];
	        this.offset = source["offset"];
	        this.limit = source["limit"];
	    }
	}
	export class LocalReviewDecision {
	    fileId: number;
	    decision: string;

	    static createFrom(source: any = {}) {
	        return new LocalReviewDecision(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.fileId = source["fileId"];
	        this.decision = source["decision"];
	    }
	}
	export class LocalReviewSave {
	    runId: string;
	    groupId: string;
	    reviewer: string;
	    note: string;
	    decisions: LocalReviewDecision[];

	    static createFrom(source: any = {}) {
	        return new LocalReviewSave(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.runId = source["runId"];
	        this.groupId = source["groupId"];
	        this.reviewer = source["reviewer"];
	        this.note = source["note"];
	        this.decisions = this.convertValues(source["decisions"], LocalReviewDecision);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class LocalTaskIOStats {
	    diskConcurrency: number;
	    effectiveReadBps: number;
	    leaseWaitMs: number;
	    sequentialBytes: number;
	    seekCount: number;
	    busyWorkers: number;
	    ioWaitWorkers: number;

	    static createFrom(source: any = {}) {
	        return new LocalTaskIOStats(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.diskConcurrency = source["diskConcurrency"];
	        this.effectiveReadBps = source["effectiveReadBps"];
	        this.leaseWaitMs = source["leaseWaitMs"];
	        this.sequentialBytes = source["sequentialBytes"];
	        this.seekCount = source["seekCount"];
	        this.busyWorkers = source["busyWorkers"];
	        this.ioWaitWorkers = source["ioWaitWorkers"];
	    }
	}
	export class LocalTask {
	    taskId: string;
	    instanceId: string;
	    revision: number;
	    source: string;
	    mode: string;
	    stage: number;
	    status: string;
	    phase: string;
	    roots: string[];
	    progressComplete: number;
	    progressTotal: number;
	    progressTotalKnown: boolean;
	    speed: string;
	    failures: number;
	    duration: string;
	    io: LocalTaskIOStats;
	    syncStatus: string;
	    errorCode: string;
	    errorSummary: string;
	    createdAt: number;
	    updatedAt: number;
	    startedAt: number;
	    completedAt: number;

	    static createFrom(source: any = {}) {
	        return new LocalTask(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.taskId = source["taskId"];
	        this.instanceId = source["instanceId"];
	        this.revision = source["revision"];
	        this.source = source["source"];
	        this.mode = source["mode"];
	        this.stage = source["stage"];
	        this.status = source["status"];
	        this.phase = source["phase"];
	        this.roots = source["roots"];
	        this.progressComplete = source["progressComplete"];
	        this.progressTotal = source["progressTotal"];
	        this.progressTotalKnown = source["progressTotalKnown"];
	        this.speed = source["speed"];
	        this.failures = source["failures"];
	        this.duration = source["duration"];
	        this.io = this.convertValues(source["io"], LocalTaskIOStats);
	        this.syncStatus = source["syncStatus"];
	        this.errorCode = source["errorCode"];
	        this.errorSummary = source["errorSummary"];
	        this.createdAt = source["createdAt"];
	        this.updatedAt = source["updatedAt"];
	        this.startedAt = source["startedAt"];
	        this.completedAt = source["completedAt"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class LocalTaskControl {
	    taskId: string;
	    instanceId: string;
	    expectedRevision: number;

	    static createFrom(source: any = {}) {
	        return new LocalTaskControl(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.taskId = source["taskId"];
	        this.instanceId = source["instanceId"];
	        this.expectedRevision = source["expectedRevision"];
	    }
	}
	export class LocalTaskCreate {
	    taskId: string;
	    roots: string[];
	    mode: string;
	    rescan: boolean;
	    extensions: string[];

	    static createFrom(source: any = {}) {
	        return new LocalTaskCreate(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.taskId = source["taskId"];
	        this.roots = source["roots"];
	        this.mode = source["mode"];
	        this.rescan = source["rescan"];
	        this.extensions = source["extensions"];
	    }
	}
	export class LocalTaskPage {
	    ok: boolean;
	    tasks: LocalTask[];
	    offset: number;
	    nextOffset: number;
	    errorCode: string;
	    errorSummary: string;

	    static createFrom(source: any = {}) {
	        return new LocalTaskPage(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ok = source["ok"];
	        this.tasks = this.convertValues(source["tasks"], LocalTask);
	        this.offset = source["offset"];
	        this.nextOffset = source["nextOffset"];
	        this.errorCode = source["errorCode"];
	        this.errorSummary = source["errorSummary"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class LocalTaskResult {
	    ok: boolean;
	    task: LocalTask;
	    deleted: boolean;
	    errorCode: string;
	    errorSummary: string;

	    static createFrom(source: any = {}) {
	        return new LocalTaskResult(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ok = source["ok"];
	        this.task = this.convertValues(source["task"], LocalTask);
	        this.deleted = source["deleted"];
	        this.errorCode = source["errorCode"];
	        this.errorSummary = source["errorSummary"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class OperationResult {
	    ok: boolean;
	    errorCode: string;
	    errorSummary: string;
	    uacCancelled: boolean;

	    static createFrom(source: any = {}) {
	        return new OperationResult(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ok = source["ok"];
	        this.errorCode = source["errorCode"];
	        this.errorSummary = source["errorSummary"];
	        this.uacCancelled = source["uacCancelled"];
	    }
	}
	export class WorkerState {
	    index: number;
	    pid: number;
	    ready: boolean;
	    currentTaskSummary: string;
	    lastErrorSummary: string;

	    static createFrom(source: any = {}) {
	        return new WorkerState(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.index = source["index"];
	        this.pid = source["pid"];
	        this.ready = source["ready"];
	        this.currentTaskSummary = source["currentTaskSummary"];
	        this.lastErrorSummary = source["lastErrorSummary"];
	    }
	}
	export class Overview {
	    machineId: string;
	    agent: ComponentState;
	    workers: WorkerState[];
	    helper: ComponentState;
	    agentStartMode: string;
	    helperStartMode: string;
	    helperEnabled: boolean;
	    helperTaskDrift: boolean;
	    loginStartDrift: boolean;

	    static createFrom(source: any = {}) {
	        return new Overview(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.machineId = source["machineId"];
	        this.agent = this.convertValues(source["agent"], ComponentState);
	        this.workers = this.convertValues(source["workers"], WorkerState);
	        this.helper = this.convertValues(source["helper"], ComponentState);
	        this.agentStartMode = source["agentStartMode"];
	        this.helperStartMode = source["helperStartMode"];
	        this.helperEnabled = source["helperEnabled"];
	        this.helperTaskDrift = source["helperTaskDrift"];
	        this.loginStartDrift = source["loginStartDrift"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class PageRequest {
	    offset: number;
	    limit: number;

	    static createFrom(source: any = {}) {
	        return new PageRequest(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.offset = source["offset"];
	        this.limit = source["limit"];
	    }
	}
	export class PathSelectionResult {
	    ok: boolean;
	    path: string;
	    cancelled: boolean;
	    errorCode: string;
	    errorSummary: string;

	    static createFrom(source: any = {}) {
	        return new PathSelectionResult(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ok = source["ok"];
	        this.path = source["path"];
	        this.cancelled = source["cancelled"];
	        this.errorCode = source["errorCode"];
	        this.errorSummary = source["errorSummary"];
	    }
	}
	export class TraySettings {
	    loginStartTray: boolean;
	    agentStartMode: string;
	    helperEnabled: boolean;
	    helperStartMode: string;
	    closeToTray: boolean;
	    refreshIntervalSeconds: number;
	    notificationLevel: string;

	    static createFrom(source: any = {}) {
	        return new TraySettings(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.loginStartTray = source["loginStartTray"];
	        this.agentStartMode = source["agentStartMode"];
	        this.helperEnabled = source["helperEnabled"];
	        this.helperStartMode = source["helperStartMode"];
	        this.closeToTray = source["closeToTray"];
	        this.refreshIntervalSeconds = source["refreshIntervalSeconds"];
	        this.notificationLevel = source["notificationLevel"];
	    }
	}

}

