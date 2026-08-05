import { GetOverview } from '../../wailsjs/go/main/Backend'
import { EventsOn } from '../../wailsjs/runtime/runtime'

export const SUMMARY_LIMIT = 240

export type ComponentName = 'agent' | 'helper'
export type Lifecycle = 'stopped' | 'starting' | 'running' | 'stopping' | 'failed'

export type ComponentState = {
  lifecycle: string
  healthy: boolean
  ready: boolean
  pid: number
  startedAtUnixMs: number
  uptimeSeconds: number
  workerReady: number
  workerExpected: number
  activeRequests: number
  errorCode: string
  errorSummary: string
  needsAttention: boolean
  runtimeConfigSha256: string
  savedConfigSha256: string
  needsRestart: boolean
}

export type WorkerState = {
  index: number
  pid: number
  ready: boolean
  currentTaskSummary: string
  lastErrorSummary: string
}

export type NodeOverview = {
  machineId: string
  agent: ComponentState
  workers: WorkerState[]
  helper: ComponentState
  agentStartMode: string
  helperStartMode: string
  helperEnabled: boolean
  helperTaskDrift: boolean
  loginStartDrift: boolean
}

export type OperationProgress = {
  operation: string
  summary: string
}

export type AttentionRequired = {
  component: ComponentName | 'tray'
  code: string
  summary: string
}

export type NodeSnapshot = {
  overview: NodeOverview | null
  operation: OperationProgress | null
  attention: AttentionRequired | null
  loading: boolean
  errorSummary: string
}

type EventHandler = (payload: unknown) => void

export type NodeStoreDependencies = {
  getOverview: () => Promise<NodeOverview>
  onEvent: (name: 'component-state' | 'operation-progress' | 'attention-required', handler: EventHandler) => () => void
}

export type NodeStore = {
  start: () => Promise<void>
  dispose: () => void
  subscribe: (listener: () => void) => () => void
  getSnapshot: () => NodeSnapshot
}

const componentFields = [
  'lifecycle',
  'healthy',
  'ready',
  'pid',
  'startedAtUnixMs',
  'uptimeSeconds',
  'workerReady',
  'workerExpected',
  'activeRequests',
  'errorCode',
  'errorSummary',
  'needsAttention',
  'runtimeConfigSha256',
  'savedConfigSha256',
  'needsRestart',
] as const

const lifecycleValues = new Set<Lifecycle>(['stopped', 'starting', 'running', 'stopping', 'failed'])

const productionDependencies: NodeStoreDependencies = {
  getOverview: GetOverview,
  onEvent: (name, handler) => EventsOn(name, (payload: unknown) => handler(payload)),
}

export function sanitizeSummary(value: string): string {
  const withoutControls = Array.from(value, (character) => {
    const point = character.codePointAt(0) ?? 0
    return point < 32 || point === 127 ? ' ' : character
  }).join('')
  const withoutDatabaseURLs = withoutControls.replace(/\bpostgres(?:ql)?:\/\/[^\s;,]+/gi, '[REDACTED]')
  const withoutAssignments = withoutDatabaseURLs.replace(
    /\b(?:password|passwd|pwd|[\w-]*(?:credential|secret|token)[\w-]*)\s*[:=]\s*(?:"[^"]*"|'[^']*'|[^\s;,]+)/gi,
    '[REDACTED]',
  )
  const withoutPaths = withoutAssignments.replace(/(?:\b[A-Za-z]:\\|\\\\)[^\s]+/g, '[REDACTED_PATH]')
  const compact = withoutPaths.replace(/\s+/g, ' ').trim()
  return Array.from(compact).slice(0, SUMMARY_LIMIT).join('')
}

function isDisabledHelperWithoutProcess(overview: NodeOverview): boolean {
  return !overview.helperEnabled && overview.helper.pid <= 0
}

function normalizeDisabledHelper(value: ComponentState): ComponentState {
  return {
    lifecycle: 'stopped',
    healthy: false,
    ready: false,
    pid: 0,
    startedAtUnixMs: 0,
    uptimeSeconds: 0,
    workerReady: 0,
    workerExpected: 0,
    activeRequests: 0,
    errorCode: '',
    errorSummary: '',
    needsAttention: false,
    runtimeConfigSha256: '',
    savedConfigSha256: /^[0-9a-f]{64}$/.test(value.savedConfigSha256) ? value.savedConfigSha256 : '',
    needsRestart: false,
  }
}

export function createNodeStore(dependencies: NodeStoreDependencies = productionDependencies): NodeStore {
  let snapshot: NodeSnapshot = {
    overview: null,
    operation: null,
    attention: null,
    loading: false,
    errorSummary: '',
  }
  let generation = 0
  let cancellations: Array<() => void> = []
  const listeners = new Set<() => void>()
  const newestRun = new Map<ComponentName, number>()

  const publish = (next: NodeSnapshot) => {
    snapshot = next
    for (const listener of listeners) {
      listener()
    }
  }

  const cancelEvents = () => {
    const current = cancellations
    cancellations = []
    for (const cancel of current) {
      cancel()
    }
  }

  const handleComponentState = (payload: unknown) => {
    const parsed = parseComponentStateEvent(payload)
    const overview = snapshot.overview
    if (!parsed || !overview) {
      return
    }
    if (
      parsed.component === 'helper' &&
      isDisabledHelperWithoutProcess(overview) &&
      parsed.state.pid <= 0
    ) {
      return
    }
    const watermark = newestRun.get(parsed.component) ?? 0
    const terminal = parsed.state.lifecycle === 'stopped' || parsed.state.lifecycle === 'failed'
    if (!terminal && parsed.state.startedAtUnixMs > 0 && parsed.state.startedAtUnixMs < watermark) {
      return
    }
    newestRun.set(parsed.component, Math.max(watermark, parsed.state.startedAtUnixMs))
    const existing = overview[parsed.component]
    const merged = { ...existing, ...sanitizeComponent(parsed.state) }
    const disabledHelperStopped = (
      parsed.component === 'helper' &&
      !overview.helperEnabled &&
      merged.pid <= 0
    )
    publish({
      ...snapshot,
      attention: disabledHelperStopped && snapshot.attention?.component === 'helper'
        ? null
        : snapshot.attention,
      overview: {
        ...overview,
        [parsed.component]: disabledHelperStopped ? normalizeDisabledHelper(merged) : merged,
      },
    })
  }

  const handleOperationProgress = (payload: unknown) => {
    const parsed = parseOperationProgress(payload)
    if (!parsed) {
      return
    }
    publish({ ...snapshot, operation: parsed })
  }

  const handleAttentionRequired = (payload: unknown) => {
    const parsed = parseAttentionRequired(payload)
    if (!parsed) {
      return
    }
    const overview = snapshot.overview
    if (
      parsed.component === 'helper' &&
      overview &&
      isDisabledHelperWithoutProcess(overview)
    ) {
      return
    }
    publish({ ...snapshot, attention: parsed })
  }

  return {
    async start() {
      const activeGeneration = ++generation
      cancelEvents()
      publish({ ...snapshot, loading: true, errorSummary: '' })
      try {
        const overview = sanitizeOverview(await dependencies.getOverview())
        if (activeGeneration !== generation) {
          return
        }
        newestRun.set('agent', Math.max(0, overview.agent.startedAtUnixMs))
        newestRun.set('helper', Math.max(0, overview.helper.startedAtUnixMs))
        const attention = (
          isDisabledHelperWithoutProcess(overview) &&
          snapshot.attention?.component === 'helper'
        ) ? null : snapshot.attention
        publish({ ...snapshot, overview, attention, loading: false })
      } catch {
        if (activeGeneration !== generation) {
          return
        }
        publish({ ...snapshot, loading: false, errorSummary: '无法读取节点状态，请稍后重试。' })
      }
      if (activeGeneration !== generation) {
        return
      }
      cancellations = [
        dependencies.onEvent('component-state', handleComponentState),
        dependencies.onEvent('operation-progress', handleOperationProgress),
        dependencies.onEvent('attention-required', handleAttentionRequired),
      ]
    },
    dispose() {
      generation++
      cancelEvents()
    },
    subscribe(listener) {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
    getSnapshot() {
      return snapshot
    },
  }
}

function sanitizeOverview(value: NodeOverview): NodeOverview {
  return {
    ...value,
    agent: sanitizeComponent(value.agent),
    helper: sanitizeComponent(value.helper),
    workers: value.workers.map((worker) => ({
      ...worker,
      currentTaskSummary: sanitizeSummary(worker.currentTaskSummary),
      lastErrorSummary: sanitizeSummary(worker.lastErrorSummary),
    })),
  }
}

function sanitizeComponent(value: ComponentState): ComponentState {
  return {
    ...value,
    errorCode: sanitizeSummary(value.errorCode),
    errorSummary: sanitizeSummary(value.errorSummary),
  }
}

function parseComponentStateEvent(payload: unknown): { component: ComponentName; state: ComponentState } | null {
  if (!isExactRecord(payload, ['component', 'state'])) {
    return null
  }
  if (payload.component !== 'agent' && payload.component !== 'helper') {
    return null
  }
  if (!isExactRecord(payload.state, componentFields)) {
    return null
  }
  const state = payload.state
  if (
    typeof state.lifecycle !== 'string' ||
    !lifecycleValues.has(state.lifecycle as Lifecycle) ||
    typeof state.healthy !== 'boolean' ||
    typeof state.ready !== 'boolean' ||
    !isNonNegativeNumber(state.pid) ||
    !isNonNegativeNumber(state.startedAtUnixMs) ||
    !isNonNegativeNumber(state.uptimeSeconds) ||
    !isNonNegativeNumber(state.workerReady) ||
    !isNonNegativeNumber(state.workerExpected) ||
    !isNonNegativeNumber(state.activeRequests) ||
    typeof state.errorCode !== 'string' ||
    typeof state.errorSummary !== 'string' ||
    typeof state.needsAttention !== 'boolean' ||
    typeof state.runtimeConfigSha256 !== 'string' ||
    typeof state.savedConfigSha256 !== 'string' ||
    typeof state.needsRestart !== 'boolean'
  ) {
    return null
  }
  return { component: payload.component, state: state as ComponentState }
}

function parseOperationProgress(payload: unknown): OperationProgress | null {
  if (
    !isExactRecord(payload, ['operation', 'summary']) ||
    typeof payload.operation !== 'string' ||
    typeof payload.summary !== 'string'
  ) {
    return null
  }
  return { operation: sanitizeSummary(payload.operation), summary: sanitizeSummary(payload.summary) }
}

function parseAttentionRequired(payload: unknown): AttentionRequired | null {
  if (
    !isExactRecord(payload, ['component', 'code', 'summary']) ||
    (payload.component !== 'agent' && payload.component !== 'helper' && payload.component !== 'tray') ||
    typeof payload.code !== 'string' ||
    typeof payload.summary !== 'string'
  ) {
    return null
  }
  return {
    component: payload.component,
    code: sanitizeSummary(payload.code),
    summary: sanitizeSummary(payload.summary),
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function isExactRecord<const Field extends string>(value: unknown, fields: readonly Field[]): value is Record<Field, unknown> {
  if (!isRecord(value)) {
    return false
  }
  const keys = Object.keys(value)
  return keys.length === fields.length && keys.every((key) => (fields as readonly string[]).includes(key))
}

function isNonNegativeNumber(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0
}
