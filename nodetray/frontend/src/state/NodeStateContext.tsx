import { createContext, useContext, useEffect, useMemo, useSyncExternalStore, type ReactNode } from 'react'
import { createNodeStore, type NodeSnapshot, type NodeStore } from './nodeStore'

type NodeStateValue = {
  snapshot: NodeSnapshot
  refresh: () => Promise<void>
}

const NodeStateContext = createContext<NodeStateValue | null>(null)

export function NodeStateProvider({ children, store }: { children: ReactNode; store?: NodeStore }): ReactNode {
  const activeStore = useMemo(() => store ?? createNodeStore(), [store])
  const snapshot = useSyncExternalStore(activeStore.subscribe, activeStore.getSnapshot)

  useEffect(() => {
    void activeStore.start()
    return () => activeStore.dispose()
  }, [activeStore])

  const value = useMemo<NodeStateValue>(() => ({
    snapshot,
    refresh: () => activeStore.start(),
  }), [activeStore, snapshot])

  return <NodeStateContext.Provider value={value}>{children}</NodeStateContext.Provider>
}

export function useNodeState(): NodeStateValue {
  const value = useContext(NodeStateContext)
  if (!value) throw new Error('NodeStateProvider is required')
  return value
}

export function useOptionalNodeState(): NodeStateValue | null {
  return useContext(NodeStateContext)
}
