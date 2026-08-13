import { useState, type KeyboardEvent, type ReactNode } from 'react'

export type TabKey = 'overview' | 'agent' | 'helper' | 'settings' | 'local-tasks' | 'analysis' | 'review' | 'deletions'

export type AppShellPanels = Partial<Record<TabKey, ReactNode>>

type TabDefinition = {
  key: TabKey
  label: string
  summary: string
}

const tabs: readonly TabDefinition[] = [
  { key: 'overview', label: '总览', summary: '节点组件状态将在这里显示。' },
  { key: 'agent', label: 'Agent', summary: 'Agent 交互式配置将在这里显示。' },
  { key: 'helper', label: '删除 Helper', summary: '删除 Helper 配置将在这里显示。' },
  { key: 'settings', label: '程序设置', summary: '托盘程序设置将在这里显示。' },
  { key: 'local-tasks', label: '本地任务', summary: '创建与跟踪本机扫描任务。' },
  { key: 'analysis', label: '去重分析', summary: '查看一筛、二筛、三筛计算进度。' },
  { key: 'review', label: '结果审核', summary: '审核本机去重结果。' },
  { key: 'deletions', label: '删除记录', summary: '预览删除并查看执行结果。' },
]

function tabFromHash(hash: string): TabKey {
  const candidate = hash.replace(/^#\/?/, '')
  return tabs.some((tab) => tab.key === candidate) ? (candidate as TabKey) : 'overview'
}

export function AppShell({ panels }: { panels?: AppShellPanels }): ReactNode {
  const [active, setActive] = useState<TabKey>(() => tabFromHash(window.location.hash))

  const select = (index: number, focus: boolean): void => {
    const normalized = (index + tabs.length) % tabs.length
    const selected = tabs[normalized]
    setActive(selected.key)
    if (focus) {
      document.getElementById(`tab-${selected.key}`)?.focus()
    }
  }

  const onKeyDown = (event: KeyboardEvent<HTMLButtonElement>, index: number): void => {
    let next: number | undefined
    switch (event.key) {
      case 'ArrowLeft':
        next = index - 1
        break
      case 'ArrowRight':
        next = index + 1
        break
      case 'Home':
        next = 0
        break
      case 'End':
        next = tabs.length - 1
        break
      default:
        return
    }
    event.preventDefault()
    select(next, true)
  }

  const activeTab = tabs.find((tab) => tab.key === active) ?? tabs[0]

  return (
    <main className="app-shell" style={{ overflowX: 'hidden' }}>
      <header className="app-shell__header">
        <h1>媒体节点控制台</h1>
      </header>
      <nav className="app-shell__navigation" aria-label="节点控制台页面">
        <div className="app-shell__tabs" role="tablist" aria-label="节点控制台">
          {tabs.map((tab, index) => {
            const selected = active === tab.key
            return (
              <button
                className="app-shell__tab"
                key={tab.key}
                id={`tab-${tab.key}`}
                type="button"
                role="tab"
                aria-selected={selected}
                aria-controls={`panel-${tab.key}`}
                tabIndex={selected ? 0 : -1}
                onClick={() => select(index, true)}
                onKeyDown={(event) => onKeyDown(event, index)}
              >
                {tab.label}
              </button>
            )
          })}
        </div>
      </nav>
      <p className="app-shell__current" role="status" aria-live="polite">
        当前页面：{activeTab.label}
      </p>
      <div className="app-shell__content">
        {tabs.map((tab) => (
          <section
            className="app-shell__panel"
            key={tab.key}
            id={`panel-${tab.key}`}
            role="tabpanel"
            aria-labelledby={`tab-${tab.key}`}
            hidden={active !== tab.key}
          >
            {panels?.[tab.key] ?? <><h2>{tab.label}</h2><p>{tab.summary}</p></>}
          </section>
        ))}
      </div>
    </main>
  )
}
