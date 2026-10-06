import { http } from '@/api/client'

export interface CategoryNode {
  id: number
  parentId: number | null
  name: string
  sortOrder: number
  depth: number
  /** 本分类直属的文件数 */
  documentCount: number
  /** 含所有子分类的文件数 */
  totalCount: number
  children: CategoryNode[]
}

/**
 * 树上的选中项。分类树与文件列表要把它翻译成同一套查询参数，
 * 所以解析规则放在这里共用，避免两边各写一份正则而慢慢走偏。
 */
export type CategoryFilter =
  | { mode: 'all' }
  | { mode: 'none' }
  | { mode: 'category'; id: number }

export function parseCategoryKey(key: string): CategoryFilter {
  if (key === 'none') return { mode: 'none' }
  const matched = /^cat-(\d+)$/.exec(key)
  if (matched) return { mode: 'category', id: Number(matched[1]) }
  return { mode: 'all' }
}

export function categoryKeyOf(id: number): string {
  return `cat-${id}`
}

export interface FlatCategory {
  id: number
  name: string
  depth: number
}

/**
 * 把树压平，供下拉选择使用（上传时选归属、详情里改归属）。
 * 保留 depth 是为了在下拉里用缩进体现层级，否则「研发 / 前端」和「前端」看起来没区别。
 */
export function flattenCategories(nodes: CategoryNode[], depth = 0): FlatCategory[] {
  const out: FlatCategory[] = []
  for (const node of nodes) {
    out.push({ id: node.id, name: node.name, depth })
    out.push(...flattenCategories(node.children, depth + 1))
  }
  return out
}

export async function fetchCategoryTree(): Promise<CategoryNode[]> {
  const { data } = await http.get<{ items: CategoryNode[] }>('/categories')
  return data.items
}

export async function createCategory(name: string, parentId: number | null): Promise<CategoryNode> {
  const { data } = await http.post<CategoryNode>('/categories', { name, parentId })
  return data
}

export interface CategoryPatch {
  name?: string
  /** 显式传 null 表示移到顶层；不传该字段表示不改层级 */
  parentId?: number | null
}

export async function updateCategory(id: number, patch: CategoryPatch): Promise<CategoryNode> {
  const { data } = await http.patch<CategoryNode>(`/categories/${id}`, patch)
  return data
}

export interface DeleteCategoryResult {
  movedDocuments: number
  message: string
}

export async function deleteCategory(id: number): Promise<DeleteCategoryResult> {
  const { data } = await http.delete<DeleteCategoryResult>(`/categories/${id}`)
  return data
}
