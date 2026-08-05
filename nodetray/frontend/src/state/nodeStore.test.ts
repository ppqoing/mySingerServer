import { act } from '@testing-library/react'
import type { MockInstance } from 'vitest'
import {
  SUMMARY_LIMIT,
  createNodeStore,
  sanitizeSummary,
  type NodeOverview,
  type NodeStoreDependencies,
} from './nodeStore'

function component(lifecycle: string, pid: number, startedAtUnixMs: number) {
  return {
    lifecycle,
    healthy: lifecycle === 'running',
    ready: lifecycle === 'running',
    pid,
    startedAtUnixMs,
    uptimeSeconds: lifecycle === 'running' ? 61 : 0,
    workerReady: 1,
    workerExpected: 2,
    activeRequests: 1,
    errorCode: '',
    errorSummary: '',
    needsAttention: false,
    runtimeConfigSha256: lifecycle === 'running' ? 'b'.repeat(64) : '',
    savedConfigSha256: 'a'.repeat(64),
    needsRestart: lifecycle === 'running',
  }
}

function initialOverview(): NodeOverview {
  return {
    machineId: 'node-a',
    agent: component('running', 101, 200),
    workers: [
      {
        index: 0,
        pid: 201,
        ready: true,
        currentTaskSummary: '空闲',
        lastErrorSummary: '',
      },
    ],
    helper: component('stopped', 0, 0),
    agentStartMode: 'automatic',
    helperStartMode: 'manual',
    helperEnabled: true,
    helperTaskDrift: false,
    loginStartDrift: false,
  }
}

type EventHandler = (payload: unknown) => void

function createHarness(overview: NodeOverview = initialOverview()) {
  const order: string[] = []
  const handlers = new Map<string, EventHandler>()
  const cancellations: MockInstance[] = []
  let resolveOverview!: (value: NodeOverview) => void
  const overviewPromise = new Promise<NodeOverview>((resolve) => {
    resolveOverview = resolve
  })
  const dependencies: NodeStoreDependencies = {
    getOverview: () => {
      order.push('get-overview')
      return overviewPromise
    },
    onEvent: (name, handler) => {
      order.push(`subscribe:${name}`)
      handlers.set(name, handler)
      const cancel = vi.fn(() => order.push(`cancel:${name}`))
      cancellations.push(cancel)
      return cancel
    },
  }

  return { cancellations, dependencies, handlers, order, overview, resolveOverview }
}

describe('nodeStore', () => {
  it('先加载类型化总览，再订阅三个事件，并在销毁时逐一取消订阅', async () => {
    const harness = createHarness()
    const store = createNodeStore(harness.dependencies)
    const started = store.start()

    expect(harness.order).toEqual(['get-overview'])
    harness.resolveOverview(harness.overview)
    await started

    expect(harness.order).toEqual([
      'get-overview',
      'subscribe:component-state',
      'subscribe:operation-progress',
      'subscribe:attention-required',
    ])
    expect(store.getSnapshot().overview).toEqual(harness.overview)

    store.dispose()
    expect(harness.cancellations).toHaveLength(3)
    for (const cancel of harness.cancellations) {
      expect(cancel).toHaveBeenCalledOnce()
    }
  })

  it('按组件合并状态、保留 Worker，并拒绝明确旧的运行态事件但接受终态', async () => {
    const harness = createHarness()
    const store = createNodeStore(harness.dependencies)
    const started = store.start()
    harness.resolveOverview(harness.overview)
    await started

    const stateHandler = harness.handlers.get('component-state')!
    stateHandler({ component: 'agent', state: component('running', 999, 100) })
    expect(store.getSnapshot().overview?.agent.pid).toBe(101)

    stateHandler({ component: 'agent', state: component('stopped', 0, 100) })
    expect(store.getSnapshot().overview?.agent.lifecycle).toBe('stopped')
    expect(store.getSnapshot().overview?.workers).toEqual(harness.overview.workers)
    expect(store.getSnapshot().overview?.helper).toEqual(harness.overview.helper)

    stateHandler({ component: 'helper', state: component('running', 301, 300) })
    expect(store.getSnapshot().overview?.helper.pid).toBe(301)
    expect(store.getSnapshot().overview?.agent.lifecycle).toBe('stopped')
    expect(store.getSnapshot().overview?.helper.runtimeConfigSha256).toBe('b'.repeat(64))
    expect(store.getSnapshot().overview?.helper.savedConfigSha256).toBe('a'.repeat(64))
    expect(store.getSnapshot().overview?.helper.needsRestart).toBe(true)
  })

  it('忽略禁用无 PID Helper 的 unavailable 和 attention，但接受真实 PID', async () => {
    const value = initialOverview()
    value.helperEnabled = false
    value.helper = component('stopped', 0, 0)
    const harness = createHarness(value)
    const store = createNodeStore(harness.dependencies)
    const started = store.start()
    harness.resolveOverview(value)
    await started

    const disabledState = store.getSnapshot().overview?.helper
    harness.handlers.get('component-state')!({
      component: 'helper',
      state: {
        ...component('failed', 0, 0),
        errorCode: 'unavailable',
        errorSummary: '组件不可用',
        needsAttention: true,
      },
    })
    harness.handlers.get('attention-required')!({
      component: 'helper', code: 'unavailable', summary: '组件不可用',
    })
    expect(store.getSnapshot().overview?.helper).toEqual(disabledState)
    expect(store.getSnapshot().attention).toBeNull()

    harness.handlers.get('component-state')!({
      component: 'helper',
      state: component('running', 3301, 300),
    })
    expect(store.getSnapshot().overview?.helper.pid).toBe(3301)
    harness.handlers.get('attention-required')!({
      component: 'helper', code: 'live_helper_warning', summary: '残留 Helper 仍在运行',
    })
    expect(store.getSnapshot().attention?.code).toBe('live_helper_warning')
  })

  it('禁用 Helper 从残留 PID 回到 PID 0 时原子归一化状态并清除 attention', async () => {
    const value = initialOverview()
    value.helperEnabled = false
    value.helper = component('stopped', 0, 0)
    const harness = createHarness(value)
    const store = createNodeStore(harness.dependencies)
    const started = store.start()
    harness.resolveOverview(value)
    await started

    harness.handlers.get('component-state')!({
      component: 'helper',
      state: component('running', 3301, 300),
    })
    harness.handlers.get('attention-required')!({
      component: 'helper', code: 'live_helper_warning', summary: '残留 Helper 仍在运行',
    })

    const published = [] as ReturnType<typeof store.getSnapshot>[]
    const unsubscribe = store.subscribe(() => published.push(store.getSnapshot()))
    harness.handlers.get('component-state')!({
      component: 'helper',
      state: {
        ...component('stopped', 0, 300),
        healthy: true,
        ready: true,
        uptimeSeconds: 99,
        workerReady: 4,
        workerExpected: 5,
        activeRequests: 2,
        errorCode: 'stale_error',
        errorSummary: '残留错误',
        needsAttention: true,
        runtimeConfigSha256: 'b'.repeat(64),
        savedConfigSha256: 'c'.repeat(64),
        needsRestart: true,
      },
    })
    unsubscribe()

    expect(published).toHaveLength(1)
    expect(published[0].attention).toBeNull()
    expect(published[0].overview?.helper).toEqual({
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
      savedConfigSha256: 'c'.repeat(64),
      needsRestart: false,
    })
  })

  it('刷新为禁用无 PID Overview 时清除旧 Helper attention', async () => {
    const enabled = initialOverview()
    const disabled = initialOverview()
    disabled.helperEnabled = false
    disabled.helper = component('stopped', 0, 0)
    const handlers = new Map<string, EventHandler>()
    const getOverview = vi.fn()
      .mockResolvedValueOnce(enabled)
      .mockResolvedValueOnce(disabled)
    const store = createNodeStore({
      getOverview,
      onEvent: (name, handler) => {
        handlers.set(name, handler)
        return () => undefined
      },
    })

    await store.start()
    handlers.get('attention-required')!({
      component: 'helper', code: 'unavailable', summary: '组件不可用',
    })
    expect(store.getSnapshot().attention?.component).toBe('helper')

    await store.start()
    expect(getOverview).toHaveBeenCalledTimes(2)
    expect(store.getSnapshot().overview?.helperEnabled).toBe(false)
    expect(store.getSnapshot().attention).toBeNull()
  })

  it('只接受窄事件、清理并截断摘要，且不写浏览器持久化', async () => {
    const firstStore = Reflect.get(window, ['local', 'Storage'].join('')) as Storage
    const secondStore = Reflect.get(window, ['session', 'Storage'].join('')) as Storage
    const firstWrite = vi.spyOn(firstStore, 'setItem')
    const secondWrite = vi.spyOn(secondStore, 'setItem')
    const harness = createHarness()
    const store = createNodeStore(harness.dependencies)
    const started = store.start()
    harness.resolveOverview(harness.overview)
    await started

    const longSummary = `token=visible\u0000\r\n${'x'.repeat(SUMMARY_LIMIT + 80)}`
    act(() => {
      harness.handlers.get('operation-progress')!({ operation: 'restart-agent', summary: longSummary })
      harness.handlers.get('attention-required')!({
        component: 'agent',
        code: 'restart_failed',
        summary: longSummary,
      })
    })

    const snapshot = store.getSnapshot()
    expect(snapshot.operation?.summary.length).toBeLessThanOrEqual(SUMMARY_LIMIT)
    expect(snapshot.attention?.summary.length).toBeLessThanOrEqual(SUMMARY_LIMIT)
    expect(snapshot.attention?.summary).toContain('[REDACTED]')
    expect(
      Array.from(snapshot.attention?.summary ?? '').some((character) => {
        const point = character.codePointAt(0) ?? 0
        return point < 32 || point === 127
      }),
    ).toBe(false)

    const beforeMalformed = store.getSnapshot()
    harness.handlers.get('operation-progress')!({ operation: 'save', summary: 'ok', extra: 'ignored' })
    harness.handlers.get('attention-required')!({ component: 'worker', code: 'bad', summary: 'bad' })
    harness.handlers.get('component-state')!({
      component: 'agent',
      state: { ...component('failed', 0, 400), arbitrary: 'value' },
    })
    expect(store.getSnapshot()).toBe(beforeMalformed)
    expect(firstWrite).not.toHaveBeenCalled()
    expect(secondWrite).not.toHaveBeenCalled()
  })

  it.each([
    'password=sample-value',
    'passwd: sample-value',
    "pwd='sample-value'",
    'postgres://sample-user:sample-value@example.invalid:5432/sampledb',
    'postgresql://sample-user:sample-value@example.invalid:5432/sampledb',
    'host=example.invalid user=sample password=sample-value dbname=sampledb',
  ])('脱敏常见凭据赋值和数据库连接串：%s', (value) => {
    const summary = sanitizeSummary(`连接失败 ${value}`)

    expect(summary).toContain('[REDACTED]')
    expect(summary).not.toContain('sample-value')
    expect(summary.length).toBeLessThanOrEqual(SUMMARY_LIMIT)
  })
})
