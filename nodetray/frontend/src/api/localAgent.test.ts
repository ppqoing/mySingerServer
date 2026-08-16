import {
  cancelLocalTask,
  chooseLocalTaskRoot,
  deleteLocalTask,
  executeLocalDelete,
  pauseLocalTask,
  prepareLocalDelete,
  resumeLocalTask,
  retryLocalTask,
} from './localAgent'

describe('local Agent API', () => {
  it('keeps one-time delete tokens outside frontend state', async () => {
    const backend = {
      PrepareLocalDelete: vi.fn(async () => ({ ok: true, batchId: 'b1', selectionDigest: 'd1', count: 1, files: [] })),
      ExecuteLocalDelete: vi.fn(async (request: unknown) => ({ ok: true, requested: 1, succeeded: 1, failed: 0, uncertain: 0, items: [], request })),
    }
    Object.assign(window, { go: { main: { Backend: backend } } })
    const preview = await prepareLocalDelete({ runId: 'r1', groupId: 'g1' })
    expect(JSON.stringify(preview)).not.toMatch(/token|dsn/i)
    await executeLocalDelete({ batchId: preview.batchId, selectionDigest: preview.selectionDigest })
    expect(backend.ExecuteLocalDelete).toHaveBeenCalledWith({ batchId: 'b1', selectionDigest: 'd1' })
  })

  it('calls the Wails directory picker with the previous root and has a safe unavailable fallback', async () => {
    Object.assign(window, { go: undefined })
    await expect(chooseLocalTaskRoot('D:\\Media')).resolves.toEqual({
      ok: false, path: '', cancelled: false, errorCode: 'backend_unavailable',
    })

    const backend = { ChooseLocalTaskRoot: vi.fn(async (currentPath: string) => ({ ok: true, path: currentPath + '\\Photos', cancelled: false })) }
    Object.assign(window, { go: { main: { Backend: backend } } })
    await expect(chooseLocalTaskRoot('D:\\Media')).resolves.toEqual({ ok: true, path: 'D:\\Media\\Photos', cancelled: false })
    expect(backend.ChooseLocalTaskRoot).toHaveBeenCalledWith('D:\\Media')
  })

  it('forwards the current task instance and revision to every lifecycle control', async () => {
    const result = { ok: false, errorCode: 'stale_task', errorSummary: '任务快照已变化' }
    const backend = {
      PauseLocalTask: vi.fn(async () => result),
      ResumeLocalTask: vi.fn(async () => result),
      CancelLocalTask: vi.fn(async () => result),
      DeleteLocalTask: vi.fn(async () => result),
      RetryLocalTask: vi.fn(async () => result),
    }
    Object.assign(window, { go: { main: { Backend: backend } } })
    const control = { taskId: 'task-1', instanceId: 'instance-7', expectedRevision: 11 }

    await expect(Promise.all([
      pauseLocalTask(control),
      resumeLocalTask(control),
      cancelLocalTask(control),
      deleteLocalTask(control),
      retryLocalTask(control),
    ])).resolves.toEqual([result, result, result, result, result])
    expect(backend.PauseLocalTask).toHaveBeenCalledWith(control)
    expect(backend.ResumeLocalTask).toHaveBeenCalledWith(control)
    expect(backend.CancelLocalTask).toHaveBeenCalledWith(control)
    expect(backend.DeleteLocalTask).toHaveBeenCalledWith(control)
    expect(backend.RetryLocalTask).toHaveBeenCalledWith(control)
  })
})
