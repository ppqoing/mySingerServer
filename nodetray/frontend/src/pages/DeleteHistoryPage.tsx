import { useState, type ReactNode } from 'react'
import { executeLocalDelete, prepareLocalDelete, type DeleteBatch, type DeletePreview } from '../api/localAgent'

type API = { prepare: (request: { runId: string; groupId: string }) => Promise<DeletePreview>; execute: (request: { batchId: string; selectionDigest: string }) => Promise<DeleteBatch> }
const defaultAPI: API = { prepare: prepareLocalDelete, execute: executeLocalDelete }
export function DeleteHistoryPage({ api = defaultAPI }: { api?: API }): ReactNode {
  const [runId, setRunID] = useState(''); const [groupId, setGroupID] = useState(''); const [preview, setPreview] = useState<DeletePreview>(); const [batch, setBatch] = useState<DeleteBatch>()
  const prepare = async (): Promise<void> => setPreview(await api.prepare({ runId, groupId }))
  const execute = async (): Promise<void> => { if (preview?.ok) setBatch(await api.execute({ batchId: preview.batchId, selectionDigest: preview.selectionDigest })) }
  return <section><h2>删除记录</h2><label>运行 ID<input aria-label="运行 ID" value={runId} onChange={(event) => setRunID(event.target.value)} /></label><label>分组 ID<input aria-label="分组 ID" value={groupId} onChange={(event) => setGroupID(event.target.value)} /></label><button type="button" onClick={() => void prepare()}>预览删除</button>{preview?.ok && <div><p>共 {preview.count} 个文件，合计 {preview.totalSize} 字节</p><button type="button" onClick={() => void execute()}>确认删除</button></div>}{batch?.ok && <p><span>已删除 {batch.succeeded}</span> / <span>失败 {batch.failed}</span> / <span>不确定 {batch.uncertain}</span></p>}</section>
}
