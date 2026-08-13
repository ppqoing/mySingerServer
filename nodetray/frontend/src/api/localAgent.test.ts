import { executeLocalDelete, prepareLocalDelete } from './localAgent'

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
})
