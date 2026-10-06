import { http } from '@/api/client'

/**
 * 索引状态。前端据此展示进度与失败原因，取值与后端 index_status 的 CHECK 约束一一对应。
 *
 * not_supported 与 failed 必须分开：前者是“这类文件本就不做正文索引”（PDF 预览稿），
 * 是正常状态；后者是“本该索引但出错了”，需要用户介入。
 */
export type IndexStatus = 'pending' | 'processing' | 'ready' | 'failed' | 'not_supported'

export interface DocumentItem {
  /** 对外标识，形如 doc_xxxxxxxxxxxxxxxx，同时是路由里的 :id */
  id: string
  name: string
  contentType: string
  sizeBytes: number
  categoryId: number | null
  categoryName: string | null
  tags: string[]
  archived: boolean
  /** 非空表示在回收站里。与 archived 是两个独立的位，可以同时成立 */
  deletedAt: string | null
  storageStatus: string
  indexStatus: IndexStatus
  indexError: string | null
  createdAt: string
  updatedAt: string
}

/**
 * 服务端的上传约束。前端据此做提交前的预检与文案提示，
 * 但真正的判定仍在后端 —— 预检只是为了少一次无谓的往返。
 */
export interface ServerConfig {
  maxUploadBytes: number
  allowedExtensions: string[]
}

export async function fetchConfig(): Promise<ServerConfig> {
  const { data } = await http.get<ServerConfig>('/config')
  return data
}

export interface DocumentListQuery {
  q?: string
  /** 指定分类时后端会把该分类的整棵子树一并纳入筛选 */
  categoryId?: number | null
  /** 只看没有归属分类的文件，对应分类树上的「未分类」 */
  uncategorized?: boolean
  archived?: boolean
  /** 只看回收站里的文件。为 true 时 archived 不起作用，两者不叠加 */
  trashed?: boolean
  page?: number
  pageSize?: number
}

export interface DocumentListResult {
  items: DocumentItem[]
  total: number
  page: number
  pageSize: number
}

export async function listDocuments(query: DocumentListQuery = {}): Promise<DocumentListResult> {
  const { data } = await http.get<DocumentListResult>('/documents', {
    params: {
      q: query.q || undefined,
      // 未分类对应 category_id IS NULL，没法用分类标识表达，后端约定为字面量 none
      categoryId: query.uncategorized ? 'none' : (query.categoryId ?? undefined),
      archived: query.archived ? 'true' : undefined,
      trashed: query.trashed ? 'true' : undefined,
      page: query.page,
      pageSize: query.pageSize,
    },
  })
  return data
}

export async function getDocument(id: string): Promise<DocumentItem> {
  const { data } = await http.get<DocumentItem>(`/documents/${encodeURIComponent(id)}`)
  return data
}

export interface UploadOptions {
  categoryId?: number | null
  tags?: string[]
  onProgress?: (percent: number) => void
}

export async function uploadDocument(file: File, options: UploadOptions = {}): Promise<DocumentItem> {
  const form = new FormData()
  form.append('file', file)
  if (options.categoryId != null) form.append('categoryId', String(options.categoryId))
  // 后端表单协议是逗号分隔；标签本身不允许含逗号，所以这里不会产生歧义
  if (options.tags?.length) form.append('tags', options.tags.join(','))

  const { data } = await http.post<DocumentItem>('/documents', form, {
    // 大文件上传耗时可能超过默认的 30s 超时
    timeout: 0,
    onUploadProgress: (event) => {
      if (!options.onProgress || !event.total) return
      options.onProgress(Math.round((event.loaded / event.total) * 100))
    },
  })
  return data
}

export interface DocumentPatch {
  name?: string
  /** 显式传 null 表示移出所有分类；不传该字段表示不改归属 */
  categoryId?: number | null
  tags?: string[]
  archived?: boolean
}

export async function updateDocument(id: string, patch: DocumentPatch): Promise<DocumentItem> {
  const { data } = await http.patch<DocumentItem>(`/documents/${encodeURIComponent(id)}`, patch)
  return data
}

export function archiveDocument(id: string): Promise<DocumentItem> {
  return updateDocument(id, { archived: true })
}

export function restoreDocument(id: string): Promise<DocumentItem> {
  return updateDocument(id, { archived: false })
}

/**
 * 移入回收站（软删除）。
 *
 * 原文件与索引都不动，因此恢复之后立刻能被搜到。
 * 这一步是可撤销的，界面不需要二次确认。
 */
export async function deleteDocument(id: string): Promise<DocumentItem> {
  const { data } = await http.delete<DocumentItem>(`/documents/${encodeURIComponent(id)}`)
  return data
}

/** 从回收站恢复。归档状态保持删除前的样子，不会一律回到「使用中」。 */
export async function restoreFromTrash(id: string): Promise<DocumentItem> {
  const { data } = await http.post<DocumentItem>(
    `/documents/${encodeURIComponent(id)}/restore`,
  )
  return data
}

/**
 * 彻底删除：记录与磁盘上的原文件一起消失，无法恢复。
 *
 * 只对回收站里的文件有效；对一份正在使用的文件调用会得到 409。
 */
export async function purgeDocument(id: string): Promise<void> {
  await http.delete(`/documents/${encodeURIComponent(id)}/purge`)
}

export interface EmptyTrashResult {
  purged: number
  message: string
}

/** 清空回收站。返回真正被删掉的条数，用于告诉用户刚才发生了什么。 */
export async function emptyTrash(): Promise<EmptyTrashResult> {
  const { data } = await http.delete<EmptyTrashResult>('/trash')
  return data
}

/**
 * 重新索引。
 *
 * 接口是幂等的：已经有任务在排队或执行时不会另建一个，
 * 因此界面上可以放心地在失败后反复重试。
 */
export async function reindexDocument(id: string): Promise<DocumentItem> {
  const { data } = await http.post<DocumentItem>(`/documents/${encodeURIComponent(id)}/reindex`)
  return data
}

/** 是否处于「后台正在处理」的中间态，界面据此决定要不要继续轮询。 */
export function isIndexing(status: IndexStatus): boolean {
  return status === 'pending' || status === 'processing'
}

/**
 * 取回原文件内容。
 *
 * 不用 <a href> 直接指到下载地址：那样一旦失败（例如原文件丢失返回 404），
 * 浏览器会跳到一片 JSON 上，用户看不到任何可读提示。
 * 这里先取成 Blob，成功再触发保存，失败就能按统一的错误结构展示。
 */
export async function fetchDocumentBlob(id: string, inline = false): Promise<Blob> {
  const { data } = await http.get<Blob>(`/documents/${encodeURIComponent(id)}/download`, {
    params: inline ? { inline: 'true' } : undefined,
    responseType: 'blob',
    timeout: 0,
  })
  return data
}

/** 触发浏览器另存为。文件名用库里的显示名，而不是磁盘上的存储键。 */
export function saveBlob(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = filename
  document.body.appendChild(anchor)
  anchor.click()
  anchor.remove()
  // 立即回收会让部分浏览器来不及开始下载，推迟一拍再释放
  setTimeout(() => URL.revokeObjectURL(url), 10_000)
}

/** 在浏览器新标签页里预览（PDF 走内联返回）。 */
export function openBlobInNewTab(blob: Blob): void {
  const url = URL.createObjectURL(blob)
  window.open(url, '_blank', 'noopener')
  setTimeout(() => URL.revokeObjectURL(url), 60_000)
}
