import '@testing-library/jest-dom/vitest'
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach } from 'vitest'
import { LocalTasksPage } from './LocalTasksPage'
import type { LocalTasksAPI } from './LocalTasksPage'
import type {
  LocalTask,
  LocalTaskControl,
  LocalTaskPage,
  LocalTaskResult,
} from '../api/localAgent'
import type { LocalTaskOperation } from './localTaskLifecycle'

type ControlAPI = Record<LocalTaskOperation, (control: LocalTaskControl) => Promise<LocalTaskResult>>
type TestAPI = LocalTasksAPI & ControlAPI

const path = (value: string, cancelled = false) => ({ ok: true, path: value, cancelled })

const task = (overrides: Partial<LocalTask> = {}): LocalTask => ({
  taskId: 'task-1', instanceId: 'instance-1', revision: 1, source: 'D:\\Media',
  mode: '扫描并自动三筛', stage: 1, status: 'running', phase: 'scan', roots: ['D:\\Media'],
  progressComplete: 10, progressTotal: 100, progressTotalKnown: true,
  speed: '10 文件/秒', failures: 0, duration: '00:00:01', syncStatus: '本机已保存',
  createdAt: 1_725_000_000_000, updatedAt: 1_725_000_001_000,
  startedAt: 1_725_000_000_500, completedAt: 0,
  ...overrides,
})

const page = (...tasks: LocalTask[]): LocalTaskPage => ({ ok: true, tasks })

const api = (overrides: Partial<TestAPI> = {}): TestAPI => ({
  choose: vi.fn(async () => path('')),
  create: vi.fn(async () => ({ ok: true, task: task() })),
  list: vi.fn(async () => page()),
  pause: vi.fn(async () => ({ ok: true, task: task({ status: 'pausing', revision: 2 }) })),
  resume: vi.fn(async () => ({ ok: true, task: task({ status: 'running', revision: 2 }) })),
  cancel: vi.fn(async () => ({ ok: true, task: task({ status: 'stopping', revision: 2 }) })),
  delete: vi.fn(async () => ({ ok: true, deleted: true })),
  retry: vi.fn(async () => ({ ok: true, task: task({ status: 'pending', revision: 2 }) })),
  ...overrides,
})

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise
    reject = rejectPromise
  })
  return { promise, resolve, reject }
}

const flushPromises = async (): Promise<void> => {
  await act(async () => {
    await Promise.resolve()
    await Promise.resolve()
  })
}

const taskList = (): HTMLElement => screen.getByRole('list', { name: '本地任务列表' })

afterEach(() => {
  vi.useRealTimers()
})

describe('本地任务表单', () => {
  it('重复打开原生窗口添加多个目录后提交', async () => {
    const choose = vi.fn().mockResolvedValueOnce(path('D:\\Media')).mockResolvedValueOnce(path('E:\\Photos'))
    const create = vi.fn(async () => ({ ok: true, task: task() }))
    render(<LocalTasksPage api={api({ choose, create })} />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: '选择目录…' }))
    await user.click(screen.getByRole('button', { name: '选择目录…' }))
    await user.click(screen.getByRole('button', { name: '创建任务' }))
    expect(create).toHaveBeenCalledWith(expect.objectContaining({ roots: ['D:\\Media', 'E:\\Photos'] }))
  })

  it('取消原生窗口不改变已有目录列表', async () => {
    const choose = vi.fn().mockResolvedValueOnce(path('D:\\Media')).mockResolvedValueOnce(path('', true))
    render(<LocalTasksPage api={api({ choose })} />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: '选择目录…' }))
    await user.click(screen.getByRole('button', { name: '选择目录…' }))
    expect(screen.getByText('D:\\Media')).toBeVisible()
    expect(within(screen.getByRole('list', { name: '扫描目录列表' })).getAllByRole('listitem')).toHaveLength(1)
  })

  it('手工添加和逐项移除目录', async () => {
    render(<LocalTasksPage api={api()} />)
    const user = userEvent.setup()
    await user.type(screen.getByLabelText('手工目录'), 'D:\\Media')
    await user.click(screen.getByRole('button', { name: '添加目录' }))
    expect(screen.getByText('D:\\Media')).toBeVisible()
    await user.click(screen.getByRole('button', { name: '移除 D:\\Media' }))
    expect(screen.queryByText('D:\\Media')).not.toBeInTheDocument()
  })

  it('拒绝相对手工目录且保持目录列表不变', async () => {
    render(<LocalTasksPage api={api()} />)
    const user = userEvent.setup()
    await user.type(screen.getByLabelText('手工目录'), '.\\media')
    await user.click(screen.getByRole('button', { name: '添加目录' }))
    expect(screen.getByRole('status')).toHaveTextContent('请输入有效的绝对 Windows 或 UNC 目录')
    expect(screen.getByRole('list', { name: '扫描目录列表' })).toBeEmptyDOMElement()
  })

  it('父目录替换子目录前使用可注入确认函数', async () => {
    const confirmReplace = vi.fn(async () => false)
    const choose = vi.fn().mockResolvedValueOnce(path('D:\\Media\\Photos')).mockResolvedValueOnce(path('D:\\Media'))
    render(<LocalTasksPage api={api({ choose })} confirmReplace={confirmReplace} />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: '选择目录…' }))
    await user.click(screen.getByRole('button', { name: '选择目录…' }))
    await waitFor(() => expect(confirmReplace).toHaveBeenCalledWith(['D:\\Media\\Photos'], 'D:\\Media'))
    expect(screen.getByText('D:\\Media\\Photos')).toBeVisible()
    expect(screen.queryByText('D:\\Media')).not.toBeInTheDocument()
  })

  it('创建扫描或自动三筛任务且一筛特征固定启用', async () => {
    const create = vi.fn(async () => ({ ok: true, task: task() }))
    render(<LocalTasksPage api={api({ create })} />)
    const user = userEvent.setup()
    expect(screen.getByText(/一筛基础特征.*默认计算/)).toBeVisible()
    expect(screen.queryByRole('checkbox', { name: /一筛/ })).not.toBeInTheDocument()
    await user.type(screen.getByLabelText('手工目录'), 'D:\\media')
    await user.click(screen.getByRole('button', { name: '添加目录' }))
    await user.selectOptions(screen.getByLabelText('任务模式'), 'scan_then_analysis')
    await user.click(screen.getByRole('button', { name: '创建任务' }))
    expect(create).toHaveBeenCalledWith(expect.objectContaining({ roots: ['D:\\media'], mode: 'scan_then_analysis' }))
  })

  it('创建成功立即插入返回快照、重置表单并发起即时刷新', async () => {
    const created = task({ taskId: 'created-now', instanceId: 'created-instance' })
    const refresh = deferred<LocalTaskPage>()
    const list = vi.fn().mockResolvedValueOnce(page()).mockImplementationOnce(() => refresh.promise)
    render(<LocalTasksPage api={api({ list, create: vi.fn(async () => ({ ok: true, task: created })) })} />)
    await flushPromises()
    fireEvent.change(screen.getByLabelText('手工目录'), { target: { value: 'D:\\Media' } })
    fireEvent.click(screen.getByRole('button', { name: '添加目录' }))
    fireEvent.click(screen.getByRole('button', { name: '创建任务' }))
    await flushPromises()
    expect(within(taskList()).getByTitle('created-now')).toBeVisible()
    expect(screen.getByRole('list', { name: '扫描目录列表' })).toBeEmptyDOMElement()
    expect(list).toHaveBeenCalledTimes(2)
  })

  it('创建失败保留表单且不触发额外刷新', async () => {
    const list = vi.fn(async () => page())
    const create = vi.fn(async () => ({ ok: false, errorSummary: '创建失败' }))
    render(<LocalTasksPage api={api({ list, create })} />)
    await flushPromises()
    fireEvent.change(screen.getByLabelText('手工目录'), { target: { value: 'D:\\Media' } })
    fireEvent.click(screen.getByRole('button', { name: '添加目录' }))
    fireEvent.click(screen.getByRole('button', { name: '创建任务' }))
    await flushPromises()
    expect(screen.getByText('D:\\Media')).toBeVisible()
    expect(screen.getByRole('status')).toHaveTextContent('创建失败')
    expect(list).toHaveBeenCalledTimes(1)
  })

  it('创建期间列表已接受同 task_id 新实例时忽略迟到创建实例并立即刷新', async () => {
    vi.useFakeTimers()
    const created = deferred<LocalTaskResult>()
    const convergence = deferred<LocalTaskPage>()
    let requestedTaskID = ''
    const create = vi.fn((request: { taskId: string }) => {
      requestedTaskID = request.taskId
      return created.promise
    })
    const list = vi.fn()
      .mockResolvedValueOnce(page())
      .mockImplementationOnce(async () => page(task({
        taskId: requestedTaskID, instanceId: 'new-instance', revision: 5, progressComplete: 50,
      })))
      .mockImplementationOnce(() => convergence.promise)
    render(<LocalTasksPage api={api({ list, create })} />)
    await flushPromises()

    fireEvent.change(screen.getByLabelText('手工目录'), { target: { value: 'D:\\Media' } })
    fireEvent.click(screen.getByRole('button', { name: '添加目录' }))
    fireEvent.click(screen.getByRole('button', { name: '创建任务' }))
    await flushPromises()
    await act(async () => { await vi.advanceTimersByTimeAsync(5_000) })
    expect(taskList().querySelector('[data-instance-id="new-instance"]')).not.toBeNull()

    created.resolve({ ok: true, task: task({
      taskId: requestedTaskID, instanceId: 'old-instance', revision: 1, progressComplete: 1,
    }) })
    await flushPromises()
    expect(taskList().querySelector('[data-instance-id="new-instance"]')).not.toBeNull()
    expect(taskList().querySelector('[data-instance-id="old-instance"]')).toBeNull()
    expect(within(taskList()).getByText('50 / 100')).toBeVisible()
    expect(list).toHaveBeenCalledTimes(3)
  })

  it.each(['success', 'failure'] as const)('API generation 变化后忽略旧 create %s', async (settlement) => {
    const oldCreate = deferred<LocalTaskResult>()
    const first = api({ create: vi.fn(() => oldCreate.promise), list: vi.fn(async () => page()) })
    const current = task({ taskId: 'current-task', instanceId: 'current-instance', revision: 4 })
    const second = api({ list: vi.fn(async () => page(current)) })
    const { rerender } = render(<LocalTasksPage api={first} />)
    await flushPromises()

    fireEvent.change(screen.getByLabelText('手工目录'), { target: { value: 'D:\\Media' } })
    fireEvent.click(screen.getByRole('button', { name: '添加目录' }))
    fireEvent.click(screen.getByRole('button', { name: '创建任务' }))
    await flushPromises()
    rerender(<LocalTasksPage api={second} />)
    await flushPromises()

    if (settlement === 'success') {
      oldCreate.resolve({ ok: true, task: task({ taskId: 'old-task', instanceId: 'old-instance' }) })
    } else {
      oldCreate.reject(new Error('old create failed with private detail'))
    }
    await flushPromises()
    expect(taskList().querySelector('[data-instance-id="current-instance"]')).not.toBeNull()
    expect(screen.queryByTitle('old-task')).not.toBeInTheDocument()
    expect(screen.queryByText('创建任务失败')).not.toBeInTheDocument()
  })
})

describe('自适应轮询与列表世代', () => {
  it('初始加载按最新创建优先渲染任务', async () => {
    const older = task({ taskId: 'older', instanceId: 'older-i', createdAt: 100 })
    const newer = task({ taskId: 'newer', instanceId: 'newer-i', createdAt: 200 })
    render(<LocalTasksPage api={api({ list: vi.fn(async () => page(older, newer)) })} />)
    await screen.findByTitle('newer')
    const items = within(taskList()).getAllByRole('listitem')
    expect(items.map((item) => within(item).getByTitle(/older|newer/).getAttribute('title'))).toEqual(['newer', 'older'])
  })

  it('活动任务只维持一条 1 秒 setTimeout 轮询链', async () => {
    vi.useFakeTimers()
    const list = vi.fn(async () => page(task()))
    render(<LocalTasksPage api={api({ list })} />)
    await flushPromises()
    expect(screen.getByText('运行中', { exact: false })).toBeVisible()
    expect(vi.getTimerCount()).toBe(1)
    await act(async () => { await vi.advanceTimersByTimeAsync(999) })
    expect(list).toHaveBeenCalledTimes(1)
    await act(async () => { await vi.advanceTimersByTimeAsync(1) })
    expect(list).toHaveBeenCalledTimes(2)
    expect(vi.getTimerCount()).toBe(1)
  })

  it('稳定任务只维持一条 5 秒链并发现外部创建的活动任务', async () => {
    vi.useFakeTimers()
    const external = task({ taskId: 'external', instanceId: 'external-i' })
    const list = vi.fn().mockResolvedValueOnce(page(task({ status: 'succeeded', completedAt: 10 }))).mockResolvedValueOnce(page(external))
    render(<LocalTasksPage api={api({ list })} />)
    await flushPromises()
    expect(vi.getTimerCount()).toBe(1)
    await act(async () => { await vi.advanceTimersByTimeAsync(4_999) })
    expect(list).toHaveBeenCalledTimes(1)
    await act(async () => { await vi.advanceTimersByTimeAsync(1) })
    expect(list).toHaveBeenCalledTimes(2)
    expect(within(taskList()).getByTitle('external')).toBeVisible()
    expect(vi.getTimerCount()).toBe(1)
  })

  it('业务错误和 rejection 都保留最后可信列表、标记过期并在成功后恢复', async () => {
    vi.useFakeTimers()
    const trusted = task({ taskId: 'trusted', instanceId: 'trusted-i', status: 'succeeded' })
    const recovered = task({ ...trusted, revision: 2, progressComplete: 99 })
    const list = vi.fn()
      .mockResolvedValueOnce(page(trusted))
      .mockResolvedValueOnce({ ok: false, tasks: [], errorSummary: 'Agent 忙碌' })
      .mockRejectedValueOnce(new Error('transport down'))
      .mockResolvedValueOnce(page(recovered))
    render(<LocalTasksPage api={api({ list })} />)
    await flushPromises()
    await act(async () => { await vi.advanceTimersByTimeAsync(5_000) })
    expect(within(taskList()).getByTitle('trusted')).toBeVisible()
    expect(screen.getByText('状态可能已过期', { exact: false })).toBeVisible()
    expect(vi.getTimerCount()).toBe(1)
    await act(async () => { await vi.advanceTimersByTimeAsync(5_000) })
    expect(within(taskList()).getByTitle('trusted')).toBeVisible()
    expect(screen.getByText('状态可能已过期', { exact: false })).toBeVisible()
    await act(async () => { await vi.advanceTimersByTimeAsync(5_000) })
    expect(within(taskList()).getByText('99 / 100')).toBeVisible()
    expect(screen.queryByText('状态可能已过期', { exact: false })).not.toBeInTheDocument()
  })

  it('卸载时清除定时器并忽略迟到列表响应', async () => {
    vi.useFakeTimers()
    const late = deferred<LocalTaskPage>()
    const { unmount } = render(<LocalTasksPage api={api({ list: vi.fn(() => late.promise) })} />)
    await flushPromises()
    unmount()
    late.resolve(page(task({ taskId: 'too-late' })))
    await flushPromises()
    expect(vi.getTimerCount()).toBe(0)
    expect(screen.queryByTitle('too-late')).not.toBeInTheDocument()
  })

  it('API generation 变化后忽略旧列表错误', async () => {
    const oldList = deferred<LocalTaskPage>()
    const current = task({ taskId: 'current', instanceId: 'current-i' })
    const { rerender } = render(<LocalTasksPage api={api({ list: vi.fn(() => oldList.promise) })} />)
    await flushPromises()
    rerender(<LocalTasksPage api={api({ list: vi.fn(async () => page(current)) })} />)
    await flushPromises()
    oldList.reject(new Error('old generation failed'))
    await flushPromises()
    expect(within(taskList()).getByTitle('current')).toBeVisible()
    expect(screen.queryByText('状态可能已过期', { exact: false })).not.toBeInTheDocument()
  })

  it('同一 generation 的较旧列表迟到时不覆盖新实例快照', async () => {
    const initial = deferred<LocalTaskPage>()
    const replacement = task({ taskId: 'same-task', instanceId: 'new-instance', revision: 3, progressComplete: 30 })
    const list = vi.fn().mockImplementationOnce(() => initial.promise).mockResolvedValueOnce(page(replacement))
    render(<LocalTasksPage api={api({
      list,
      create: vi.fn(async () => ({ ok: true, task: task({ taskId: 'same-task', instanceId: 'new-instance', revision: 2, progressComplete: 20 }) })),
    })} />)
    await flushPromises()
    fireEvent.change(screen.getByLabelText('手工目录'), { target: { value: 'D:\\Media' } })
    fireEvent.click(screen.getByRole('button', { name: '添加目录' }))
    fireEvent.click(screen.getByRole('button', { name: '创建任务' }))
    await flushPromises()
    expect(within(taskList()).getByText('30 / 100')).toBeVisible()
    initial.resolve(page(task({ taskId: 'same-task', instanceId: 'old-instance', progressComplete: 1 })))
    await flushPromises()
    expect(within(taskList()).getAllByRole('listitem')).toHaveLength(1)
    expect(within(taskList()).getByText('30 / 100')).toBeVisible()
    expect(taskList().querySelector('[data-instance-id="new-instance"]')).not.toBeNull()
  })

  it('成功列表保留同实例较高 revision，同时仍权威删除消失任务并替换新实例', async () => {
    vi.useFakeTimers()
    const refresh = deferred<LocalTaskPage>()
    const revision1 = task({ taskId: 'kept', instanceId: 'same-instance', revision: 1, progressComplete: 10 })
    const disappearing = task({ taskId: 'gone', instanceId: 'gone-instance', revision: 1, createdAt: revision1.createdAt - 1 })
    const revision3 = task({ ...revision1, revision: 3, status: 'pausing', progressComplete: 30 })
    const revision2 = task({ ...revision1, revision: 2, progressComplete: 20 })
    const replacement = task({ taskId: 'replacement', instanceId: 'new-instance', revision: 1, createdAt: revision1.createdAt + 1 })
    const oldReplacement = task({ ...replacement, instanceId: 'old-instance', createdAt: replacement.createdAt - 1 })
    const pause = vi.fn(async () => ({ ok: true, task: revision3 }))
    const list = vi.fn()
      .mockResolvedValueOnce(page(revision1, disappearing, oldReplacement))
      .mockImplementationOnce(() => refresh.promise)
    render(<LocalTasksPage api={api({ list, pause })} />)
    await flushPromises()

    const keptItem = taskList().querySelector<HTMLElement>('[data-instance-id="same-instance"]')!
    fireEvent.click(within(keptItem).getByRole('button', { name: '暂停' }))
    await flushPromises()
    expect(within(taskList()).getByText('30 / 100')).toBeVisible()

    refresh.resolve(page(revision2, replacement))
    await flushPromises()
    expect(within(taskList()).getByText('30 / 100')).toBeVisible()
    expect(screen.getByText('正在暂停', { exact: false })).toBeVisible()
    expect(screen.queryByTitle('gone')).not.toBeInTheDocument()
    expect(taskList().querySelector('[data-instance-id="new-instance"]')).not.toBeNull()
    expect(taskList().querySelector('[data-instance-id="old-instance"]')).toBeNull()
  })
})

describe('逐项生命周期操作', () => {
  it.each([
    ['pause', 'running', '暂停'],
    ['resume', 'paused', '继续'],
  ] as const)('%s 使用当前实例和 revision 直接提交并锁定按钮', async (operation, status, label) => {
    const pending = deferred<LocalTaskResult>()
    const operationMock = vi.fn(() => pending.promise)
    const current = task({ status, instanceId: `${operation}-instance`, revision: 7 })
    render(<LocalTasksPage api={api({ list: vi.fn(async () => page(current)), [operation]: operationMock })} />)
    await screen.findByRole('button', { name: label })
    fireEvent.click(screen.getByRole('button', { name: label }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(operationMock).toHaveBeenCalledWith({ taskId: 'task-1', instanceId: `${operation}-instance`, expectedRevision: 7 })
    expect(screen.getByRole('button', { name: label })).toBeDisabled()
  })

  it('停止先确认已完成结果会保留，再由后端推进状态', async () => {
    const cancelled = task({ status: 'cancelled', revision: 2, completedAt: 10 })
    const cancel = vi.fn(async () => ({ ok: true, task: cancelled }))
    const list = vi.fn().mockResolvedValueOnce(page(task())).mockResolvedValueOnce(page(cancelled))
    render(<LocalTasksPage api={api({ list, cancel })} />)
    await screen.findByRole('button', { name: '停止' })
    fireEvent.click(screen.getByRole('button', { name: '停止' }))
    expect(screen.getByRole('dialog', { name: '停止任务' })).toHaveTextContent('已完成结果会保留')
    expect(cancel).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole('button', { name: '确认停止' }))
    await waitFor(() => expect(screen.getByText('已停止', { exact: false })).toBeVisible())
    expect(cancel).toHaveBeenCalledWith({ taskId: 'task-1', instanceId: 'instance-1', expectedRevision: 1 })
  })

  it('活动任务删除确认列出四项边界，删除当前实例后不回退旧实例', async () => {
    const refresh = deferred<LocalTaskPage>()
    const remove = vi.fn(async () => ({ ok: true, deleted: true }))
    const current = task({ taskId: 'same-task', instanceId: 'current-instance', revision: 9 })
    const list = vi.fn().mockResolvedValueOnce(page(current)).mockImplementationOnce(() => refresh.promise)
    render(<LocalTasksPage api={api({ list, delete: remove })} />)
    await screen.findByRole('button', { name: '删除' })
    fireEvent.click(screen.getByRole('button', { name: '删除' }))
    const dialog = screen.getByRole('dialog', { name: '删除任务' })
    expect(dialog).toHaveTextContent('删除本机任务及本机分析')
    expect(dialog).toHaveTextContent('保留全局索引、特征与缓存')
    expect(dialog).toHaveTextContent('保留文件删除审计')
    expect(dialog).toHaveTextContent('不撤回已同步的中央数据')
    fireEvent.click(within(dialog).getByRole('button', { name: '确认删除' }))
    await waitFor(() => expect(within(taskList()).queryByTitle('same-task')).not.toBeInTheDocument())
    expect(remove).toHaveBeenCalledWith({ taskId: 'same-task', instanceId: 'current-instance', expectedRevision: 9 })
    expect(taskList().querySelector('[data-instance-id]')).toBeNull()
  })

  it('迟到 deleted:true 不删除已升 revision 的同实例并立即刷新', async () => {
    vi.useFakeTimers()
    const deletion = deferred<LocalTaskResult>()
    const convergence = deferred<LocalTaskPage>()
    const revision1 = task({ status: 'succeeded', revision: 1, progressComplete: 10, completedAt: 10 })
    const revision2 = task({ ...revision1, revision: 2, progressComplete: 20 })
    const list = vi.fn()
      .mockResolvedValueOnce(page(revision1))
      .mockResolvedValueOnce(page(revision2))
      .mockImplementationOnce(() => convergence.promise)
    render(<LocalTasksPage api={api({ list, delete: vi.fn(() => deletion.promise) })} />)
    await flushPromises()

    fireEvent.click(screen.getByRole('button', { name: '删除' }))
    fireEvent.click(screen.getByRole('button', { name: '确认删除' }))
    await act(async () => { await vi.advanceTimersByTimeAsync(5_000) })
    expect(within(taskList()).getByText('20 / 100')).toBeVisible()

    deletion.resolve({ ok: true, deleted: true })
    await flushPromises()
    expect(within(taskList()).getByText('20 / 100')).toBeVisible()
    expect(taskList().querySelector('[data-instance-id="instance-1"]')).not.toBeNull()
    expect(list).toHaveBeenCalledTimes(3)
  })

  it.each(['stale_task', 'task_instance_mismatch'] as const)('%s 安全失败保留 Item、释放匹配锁并即时刷新', async (errorCode) => {
    const current = task()
    const list = vi.fn().mockResolvedValueOnce(page(current)).mockResolvedValueOnce(page(current))
    const pause = vi.fn(async () => ({ ok: false, errorCode, errorSummary: '任务快照已变化' }))
    render(<LocalTasksPage api={api({ list, pause })} />)
    await screen.findByRole('button', { name: '暂停' })
    fireEvent.click(screen.getByRole('button', { name: '暂停' }))
    await waitFor(() => expect(list).toHaveBeenCalledTimes(2))
    expect(within(taskList()).getByTitle('task-1')).toBeVisible()
    expect(screen.getByRole('button', { name: '暂停' })).toBeEnabled()
  })

  it('旧实例控制响应不会覆盖新实例或解锁新实例操作', async () => {
    vi.useFakeTimers()
    const oldPause = deferred<LocalTaskResult>()
    const newPause = deferred<LocalTaskResult>()
    const pause = vi.fn().mockImplementationOnce(() => oldPause.promise).mockImplementationOnce(() => newPause.promise)
    const oldItem = task({ taskId: 'same-task', instanceId: 'old-instance', revision: 1 })
    const newItem = task({ taskId: 'same-task', instanceId: 'new-instance', revision: 1, progressComplete: 50 })
    const list = vi.fn().mockResolvedValueOnce(page(oldItem)).mockResolvedValueOnce(page(newItem))
    render(<LocalTasksPage api={api({ list, pause })} />)
    await flushPromises()
    fireEvent.click(screen.getByRole('button', { name: '暂停' }))
    expect(screen.getByRole('button', { name: '暂停' })).toBeDisabled()
    await act(async () => { await vi.advanceTimersByTimeAsync(1_000) })
    expect(taskList().querySelector('[data-instance-id="new-instance"]')).not.toBeNull()
    expect(screen.getByRole('button', { name: '暂停' })).toBeEnabled()
    fireEvent.click(screen.getByRole('button', { name: '暂停' }))
    expect(screen.getByRole('button', { name: '暂停' })).toBeDisabled()
    oldPause.resolve({ ok: true, task: task({ taskId: 'same-task', instanceId: 'old-instance', revision: 99, progressComplete: 99 }) })
    await flushPromises()
    expect(taskList().querySelector('[data-instance-id="new-instance"]')).not.toBeNull()
    expect(within(taskList()).getByText('50 / 100')).toBeVisible()
    expect(screen.getByRole('button', { name: '暂停' })).toBeDisabled()
  })

  it('API generation 变化后旧控制错误既不回流也不解锁新锁', async () => {
    const oldPause = deferred<LocalTaskResult>()
    const newPause = deferred<LocalTaskResult>()
    const first = api({ list: vi.fn(async () => page(task({ instanceId: 'old-i' }))), pause: vi.fn(() => oldPause.promise) })
    const secondTask = task({ instanceId: 'new-i' })
    const second = api({ list: vi.fn(async () => page(secondTask)), pause: vi.fn(() => newPause.promise) })
    const { rerender } = render(<LocalTasksPage api={first} />)
    await screen.findByRole('button', { name: '暂停' })
    fireEvent.click(screen.getByRole('button', { name: '暂停' }))
    rerender(<LocalTasksPage api={second} />)
    await waitFor(() => expect(taskList().querySelector('[data-instance-id="new-i"]')).not.toBeNull())
    fireEvent.click(screen.getByRole('button', { name: '暂停' }))
    expect(screen.getByRole('button', { name: '暂停' })).toBeDisabled()
    oldPause.resolve({ ok: false, errorCode: 'old_error', errorSummary: '旧错误不应显示' })
    await flushPromises()
    expect(screen.queryByText('旧错误不应显示')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '暂停' })).toBeDisabled()
  })

  it('同实例旧 revision 响应不覆盖列表中的新 revision，也不解锁新锁', async () => {
    vi.useFakeTimers()
    const firstPause = deferred<LocalTaskResult>()
    const secondPause = deferred<LocalTaskResult>()
    const pause = vi.fn().mockImplementationOnce(() => firstPause.promise).mockImplementationOnce(() => secondPause.promise)
    const revision1 = task({ revision: 1, progressComplete: 10 })
    const revision2 = task({ revision: 2, progressComplete: 20 })
    const list = vi.fn().mockResolvedValueOnce(page(revision1)).mockResolvedValueOnce(page(revision2))
    render(<LocalTasksPage api={api({ list, pause })} />)
    await flushPromises()
    fireEvent.click(screen.getByRole('button', { name: '暂停' }))
    await act(async () => { await vi.advanceTimersByTimeAsync(1_000) })
    expect(screen.getByRole('button', { name: '暂停' })).toBeEnabled()
    fireEvent.click(screen.getByRole('button', { name: '暂停' }))
    expect(screen.getByRole('button', { name: '暂停' })).toBeDisabled()
    firstPause.resolve({ ok: true, task: task({ revision: 1, progressComplete: 99 }) })
    await flushPromises()
    expect(within(taskList()).getByText('20 / 100')).toBeVisible()
    expect(screen.getByRole('button', { name: '暂停' })).toBeDisabled()
  })

  it('逐 Item 操作错误只留在对应 Item 且不清空列表', async () => {
    const first = task({ taskId: 'first', instanceId: 'first-i' })
    const second = task({ taskId: 'second', instanceId: 'second-i', createdAt: first.createdAt - 1 })
    const pause = vi.fn(async () => ({ ok: false, errorCode: 'pause_failed', errorSummary: '无法暂停此任务' }))
    render(<LocalTasksPage api={api({ list: vi.fn(async () => page(first, second)), pause })} />)
    await screen.findByTitle('first')
    const firstItem = taskList().querySelector<HTMLElement>('[data-instance-id="first-i"]')!
    fireEvent.click(within(firstItem).getByRole('button', { name: '暂停' }))
    await waitFor(() => expect(within(firstItem).getByRole('alert')).toHaveTextContent('无法暂停此任务'))
    expect(within(taskList()).getAllByRole('listitem')).toHaveLength(2)
    const secondItem = taskList().querySelector<HTMLElement>('[data-instance-id="second-i"]')!
    expect(within(secondItem).queryByRole('alert')).not.toBeInTheDocument()
  })
})
