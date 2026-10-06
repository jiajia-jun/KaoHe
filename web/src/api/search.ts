import { http } from '@/api/client'
import type { DocumentItem } from '@/api/documents'

/**
 * 两种检索共用一条结果的结构：一份文档 + 它命中的若干片段。
 *
 * keyword 与 semantic 的差别只在片段上：
 * 关键词命中的是「包含这个词的那一段」，带相似度的是「语义上最接近的那一段」。
 * 界面据此决定标什么 —— 但结果列表本身可以完全复用。
 */
export interface SearchMatch {
  /** 片段在所属文档中的序号，从 0 开始 */
  ordinal: number
  /** 展示用文本；关键词检索已在命中词前后截好窗口 */
  text: string
  /** 语义检索才有：该片段与查询的余弦相似度 */
  score?: number
}

export interface SearchItem {
  document: DocumentItem
  matches: SearchMatch[]
  /** 关键词检索：文件名本身命中了关键词 */
  nameHit?: boolean
  /** 关键词检索：正文命中的片段总数，可能大于 matches 的长度 */
  bodyHits?: number
  /** 语义检索：该文档最相关片段的相似度 */
  score?: number
}

export interface SearchResult {
  query: string
  items: SearchItem[]
  total: number
  /** 语义检索才有：本次生效的相似度下限 */
  minScore?: number
  page?: number
  pageSize?: number
}

export interface SearchQuery {
  categoryId?: number | null
  archived?: boolean
}

/** 关键词检索：文件名与正文一起找。 */
export async function searchKeyword(q: string, options: SearchQuery = {}): Promise<SearchResult> {
  const { data } = await http.get<SearchResult>('/search', {
    params: {
      q,
      categoryId: options.categoryId ?? undefined,
      archived: options.archived ? 'true' : undefined,
    },
  })
  return data
}

/**
 * 语义检索：把自然语言描述交给后端转成向量后检索。
 *
 * 用 POST 而不是 GET：查询是完整的一句话，塞进查询串既有长度上限，
 * 也会被原样写进访问日志。
 */
export async function searchSemantic(
  query: string,
  options: SearchQuery = {},
): Promise<SearchResult> {
  const { data } = await http.post<SearchResult>('/search/semantic', {
    query,
    categoryId: options.categoryId ?? undefined,
    archived: options.archived ?? false,
    topK: 10,
  })
  return data
}
