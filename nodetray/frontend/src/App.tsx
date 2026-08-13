import { useCallback, useEffect, useState, type ReactNode } from 'react'
import { EventsOn } from '../wailsjs/runtime/runtime'
import { AppShell } from './components/AppShell'
import { ExitDialog } from './components/ExitDialog'
import { AgentPage } from './pages/AgentPage'
import { HelperPage } from './pages/HelperPage'
import { OverviewPage } from './pages/OverviewPage'
import { SettingsPage } from './pages/SettingsPage'
import { LocalTasksPage } from './pages/LocalTasksPage'
import { AnalysisPage } from './pages/AnalysisPage'
import { ReviewPage } from './pages/ReviewPage'
import { DeleteHistoryPage } from './pages/DeleteHistoryPage'
import { NodeStateProvider } from './state/NodeStateContext'

export type SubscribeWindowClose = (handler: () => void) => () => void

function defaultSubscribeWindowClose(handler: () => void): () => void {
	const unsubscribers: Array<() => void> = []
	for (const eventName of ['window-close-requested', 'force-exit-requested']) {
		try {
			unsubscribers.push(EventsOn(eventName, handler))
		} catch {
			// Wails runtime is unavailable in static browser previews.
		}
	}
	return () => unsubscribers.forEach((unsubscribe) => unsubscribe())
}

export function App({ subscribeWindowClose = defaultSubscribeWindowClose }: { subscribeWindowClose?: SubscribeWindowClose }): ReactNode {
  const [dirty, setDirty] = useState({ agent: false, helper: false, settings: false })
  const [exitOpen, setExitOpen] = useState(false)
  const agentDirty = useCallback((value: boolean) => setDirty((current) => current.agent === value ? current : { ...current, agent: value }), [])
  const helperDirty = useCallback((value: boolean) => setDirty((current) => current.helper === value ? current : { ...current, helper: value }), [])
  const settingsDirty = useCallback((value: boolean) => setDirty((current) => current.settings === value ? current : { ...current, settings: value }), [])

  useEffect(() => subscribeWindowClose(() => setExitOpen(true)), [subscribeWindowClose])

  return (
    <NodeStateProvider>
      <AppShell panels={{
        overview: <OverviewPage />,
        agent: <AgentPage onDirtyChange={agentDirty} />,
        helper: <HelperPage onDirtyChange={helperDirty} onRequestExit={() => setExitOpen(true)} />,
        settings: <SettingsPage onDirtyChange={settingsDirty} onRequestExit={() => setExitOpen(true)} />,
        'local-tasks': <LocalTasksPage />,
        analysis: <AnalysisPage />,
        review: <ReviewPage />,
        deletions: <DeleteHistoryPage />,
      }} />
      <ExitDialog open={exitOpen} dirty={dirty.agent || dirty.helper || dirty.settings} onReturn={() => setExitOpen(false)} />
    </NodeStateProvider>
  )
}
