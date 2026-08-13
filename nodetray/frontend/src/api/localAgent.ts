export type LocalTaskCreate = { taskId: string; roots: string[]; mode: string; rescan: boolean; extensions: string[] }
export type LocalTask = { taskId: string; source: string; stage: number; status: string; speed?: string; failures?: number; duration?: string; syncStatus?: string }
export type LocalTaskPage = { ok: boolean; tasks: LocalTask[]; errorCode?: string; errorSummary?: string }
export type LocalGroupMember = { fileId: number; fileName: string; size: number; decision: string }
export type LocalGroup = { runId: string; groupId: string; category: string; verdict: string; stageOne?: string; stageTwo?: string; stageThree?: string; members: LocalGroupMember[] }
export type LocalGroupPage = { ok: boolean; groups: LocalGroup[]; errorCode?: string; errorSummary?: string }
export type DeletePreview = { ok: boolean; batchId: string; selectionDigest: string; count: number; totalSize: number; files: unknown[]; errorSummary?: string }
export type DeleteBatch = { ok: boolean; succeeded: number; failed: number; uncertain: number; items: Array<{ fileId: number; result: string; uncertain: boolean }>; errorSummary?: string }
export type ImagePreview = { ok: boolean; mime: string; width: number; height: number; dataBase64: string; errorSummary?: string }
export type PathSelectionResult = { ok: boolean; path: string; cancelled: boolean; errorCode?: string; errorSummary?: string }

type Backend = Record<string, (...args: unknown[]) => Promise<unknown>>
function backend(): Backend | undefined { return window.go?.main?.Backend as Backend | undefined }
async function call<T>(method: string, fallback: T, ...args: unknown[]): Promise<T> {
  const fn = backend()?.[method]
  if (!fn) return fallback
  return await fn(...args) as T
}

export const createLocalTask = (request: LocalTaskCreate) => call('CreateLocalTask', { ok: false, task: {} as LocalTask, errorSummary: 'Agent 暂不可用' }, request)
export const chooseLocalTaskRoot = (currentPath: string) => call<PathSelectionResult>('ChooseLocalTaskRoot', { ok: false, path: '', cancelled: false, errorCode: 'backend_unavailable' }, currentPath)
export const listLocalTasks = () => call<LocalTaskPage>('ListLocalTasks', { ok: false, tasks: [], errorSummary: 'Agent 暂不可用' }, { offset: 0, limit: 100 })
export const startLocalAnalysis = (request: LocalTaskCreate) => call('StartLocalAnalysis', { ok: false, errorSummary: 'Agent 暂不可用' }, request)
export const listLocalGroups = (category = '') => call<LocalGroupPage>('ListLocalGroups', { ok: false, groups: [], errorSummary: 'Agent 暂不可用' }, { scope: 'current', category, offset: 0, limit: 100 })
export const saveLocalReview = (request: unknown) => call('SaveLocalReview', { ok: false, errorSummary: 'Agent 暂不可用' }, request)
export const prepareLocalDelete = (request: { runId: string; groupId: string }) => call<DeletePreview>('PrepareLocalDelete', { ok: false, batchId: '', selectionDigest: '', count: 0, totalSize: 0, files: [], errorSummary: 'Agent 暂不可用' }, request)
export const executeLocalDelete = (request: { batchId: string; selectionDigest: string }) => call<DeleteBatch>('ExecuteLocalDelete', { ok: false, succeeded: 0, failed: 0, uncertain: 0, items: [], errorSummary: 'Agent 暂不可用' }, request)
export const getLocalImagePreview = (fileId: number) => call<ImagePreview>('GetLocalImagePreview', { ok: false, mime: '', width: 0, height: 0, dataBase64: '', errorSummary: 'Agent 暂不可用' }, fileId)

declare global {
  interface Window { go?: { main?: { Backend?: Backend } } }
}
