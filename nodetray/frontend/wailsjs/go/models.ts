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

