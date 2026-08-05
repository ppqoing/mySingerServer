import type { NodeSnapshot, NodeStore } from '../state/nodeStore'

export type TestNodeStore = NodeStore & {
  publish: (change: Partial<NodeSnapshot>) => void
}

export function createTestNodeStore(start: NodeStore['start'], initial: Partial<NodeSnapshot> = {}): TestNodeStore {
  let snapshot: NodeSnapshot = {
    overview: null, operation: null, attention: null,
    loading: false, errorSummary: '',
    ...initial,
  }
  const listeners = new Set<() => void>()
  return {
    start,
    dispose: () => listeners.clear(),
    subscribe: (listener) => {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
    getSnapshot: () => snapshot,
    publish: (change) => {
      snapshot = { ...snapshot, ...change }
      listeners.forEach((listener) => listener())
    },
  }
}
