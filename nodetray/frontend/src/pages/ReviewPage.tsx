import { useEffect, useState, type ReactNode } from 'react'
import { getLocalImagePreview, listLocalGroups, saveLocalReview, type ImagePreview, type LocalGroupPage } from '../api/localAgent'

type API = { list: () => Promise<LocalGroupPage>; save: (value: unknown) => Promise<unknown>; preview: (fileId: number) => Promise<ImagePreview> }
const defaultAPI: API = { list: () => listLocalGroups(), save: saveLocalReview, preview: getLocalImagePreview }
export function ReviewPage({ api = defaultAPI }: { api?: API }): ReactNode {
  const [page, setPage] = useState<LocalGroupPage>({ ok: true, groups: [] })
  const [images, setImages] = useState<Record<number, string>>({})
  const [deleteIDs, setDeleteIDs] = useState<Set<number>>(new Set())
  const [message, setMessage] = useState('')
  useEffect(() => { void api.list().then(setPage) }, [api])
  const show = async (fileID: number): Promise<void> => { const value = await api.preview(fileID); if (value.ok) setImages((current) => ({ ...current, [fileID]: `data:${value.mime};base64,${value.dataBase64}` })) }
  const changeDelete = (fileID: number, checked: boolean): void => setDeleteIDs((current) => {
    const next = new Set(current)
    if (checked) next.add(fileID); else next.delete(fileID)
    return next
  })
  const save = async (group: LocalGroupPage['groups'][number]): Promise<void> => {
    const result = await api.save({ runId: group.runId, groupId: group.groupId, reviewer: 'local-user', decisions: group.members.map((member) => ({ fileId: member.fileId, decision: deleteIDs.has(member.fileId) ? 'delete' : 'keep' })) }) as { ok?: boolean; errorSummary?: string }
    setMessage(result.ok ? '审核已保存' : result.errorSummary || '审核保存失败')
  }
  return <section><h2>结果审核</h2><p>分类：<span>精确重复</span> · <span>图片相似</span> · <span>视频相似</span> · <span>待确认</span></p><p>评分：<span>一筛</span> / <span>二筛</span> / <span>三筛</span></p>{page.groups.map((group) => <article key={group.groupId}><h3>{group.category} · {group.verdict}</h3><p>一筛 {group.stageOne || '—'} / 二筛 {group.stageTwo || '—'} / 三筛 {group.stageThree || '—'}</p>{group.members.map((member) => <div key={member.fileId}><span>{member.fileName}</span><label><input type="checkbox" aria-label={`删除 ${member.fileName}`} checked={deleteIDs.has(member.fileId)} onChange={(event) => changeDelete(member.fileId, event.target.checked)} />删除</label><button type="button" onClick={() => void show(member.fileId)}>预览 {member.fileName}</button>{images[member.fileId] && <img alt={`${member.fileName} 预览`} src={images[member.fileId]} />}</div>)}<button type="button" onClick={() => void save(group)}>保存审核</button></article>)}<p role="status">{message}</p></section>
}
