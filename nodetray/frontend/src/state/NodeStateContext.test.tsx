import '@testing-library/jest-dom/vitest'
import { render, screen, waitFor } from '@testing-library/react'
import type { NodeSnapshot, NodeStore } from './nodeStore'
import { NodeStateProvider, useNodeState } from './NodeStateContext'

function Consumer({ label }: { label: string }) {
  const { snapshot } = useNodeState()
  return <output aria-label={label}>{snapshot.overview?.agent.lifecycle ?? 'missing'}</output>
}

it('在应用根部只启动一个 store，所有页面共享同一快照', async () => {
  const snapshot: NodeSnapshot = {
    overview: {
      machineId: 'node-a',
      agent: component('running'),
      helper: component('stopped'),
      workers: [],
      agentStartMode: 'manual',
      helperStartMode: 'manual',
      helperEnabled: true,
      helperTaskDrift: false,
      loginStartDrift: false,
    },
    operation: null,
    attention: null,
    loading: false,
    errorSummary: '',
  }
  const store: NodeStore = {
    start: vi.fn(async () => undefined),
    dispose: vi.fn(),
    subscribe: () => () => undefined,
    getSnapshot: () => snapshot,
  }

  render(<NodeStateProvider store={store}><Consumer label="overview" /><Consumer label="agent" /></NodeStateProvider>)

  await waitFor(() => expect(store.start).toHaveBeenCalledOnce())
  expect(screen.getByLabelText('overview')).toHaveTextContent('running')
  expect(screen.getByLabelText('agent')).toHaveTextContent('running')
})

function component(lifecycle: string) {
  return {
    lifecycle, healthy: lifecycle === 'running', ready: lifecycle === 'running', pid: 0,
    startedAtUnixMs: 0, uptimeSeconds: 0, workerReady: 0, workerExpected: 0,
    activeRequests: 0, errorCode: '', errorSummary: '', needsAttention: false,
    runtimeConfigSha256: '', savedConfigSha256: '', needsRestart: false,
  }
}
