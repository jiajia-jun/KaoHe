<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { errorText } from '@/api/client'
import {
  archiveDocument,
  fetchConfig,
  fetchDocumentBlob,
  listDocuments,
  restoreDocument,
  saveBlob,
  type DocumentItem,
  type ServerConfig,
} from '@/api/documents'
import { fetchCategoryTree, parseCategoryKey, type CategoryNode } from '@/api/categories'
import { formatBytes, formatDateTime, fileExtension } from '@/utils/format'
import IndexStatusTag from '@/components/IndexStatusTag.vue'
import UploadDialog from '@/components/UploadDialog.vue'
import DocumentDrawer from '@/components/DocumentDrawer.vue'
import CategoryTree from '@/components/CategoryTree.vue'

/**
 * 文件管理页：左侧分类树，右侧文件列表。
 *
 * 三种加载状态（加载中 / 成功 / 失败）与两种空状态（从来没有文件 / 筛选后无结果）
 * 都显式渲染：空列表和请求失败长得一样，是最容易误导用户的一类界面缺陷。
 *
 * 分类树数据由本页统一持有，再传给上传对话框与详情抽屉 ——
 * 三处要的是同一份分类，各拉各的会在刚建完分类时出现一段时间的不一致。
 */

type Phase = 'loading' | 'ready' | 'error'

const phase = ref<Phase>('loading')
const loadError = ref('')
const items = ref<DocumentItem[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const search = ref('')
const archivedTab = ref<'active' | 'archived'>('active')

const categoryKey = ref('all')
const categories = ref<CategoryNode[]>([])
const categoriesLoading = ref(false)
const categoriesError = ref('')

const config = ref<ServerConfig | null>(null)
const uploadOpen = ref(false)
const drawerOpen = ref(false)
const activeId = ref<string | null>(null)

const selection = computed(() => parseCategoryKey(categoryKey.value))
const hasFilter = computed(
  () => search.value.trim() !== '' || categoryKey.value !== 'all' || archivedTab.value === 'archived',
)
const emptyText = computed(() => {
  if (search.value.trim()) return '没有匹配的文件'
  if (categoryKey.value === 'none') return '还没有未分类的文件'
  if (selection.value.mode === 'category') return '这个分类下还没有文件'
  if (archivedTab.value === 'archived') return '还没有归档的文件'
  return '还没有文件，点右上角的上传按钮添加'
})

async function loadCategories() {
  categoriesLoading.value = true
  categoriesError.value = ''
  try {
    categories.value = await fetchCategoryTree()
  } catch (err) {
    categoriesError.value = errorText(err)
  } finally {
    categoriesLoading.value = false
  }
}

async function load() {
  phase.value = 'loading'
  loadError.value = ''
  try {
    const current = parseCategoryKey(categoryKey.value)
    const result = await listDocuments({
      q: search.value.trim(),
      categoryId: current.mode === 'category' ? current.id : undefined,
      uncategorized: current.mode === 'none',
      archived: archivedTab.value === 'archived',
      page: page.value,
      pageSize: pageSize.value,
    })
    items.value = result.items
    total.value = result.total
    // 删除或归档掉当前页最后一条后页码可能已越界，回退到最后一页重取
    const lastPage = Math.max(1, Math.ceil(result.total / pageSize.value))
    if (page.value > lastPage) {
      page.value = lastPage
      return load()
    }
    phase.value = 'ready'
  } catch (err) {
    phase.value = 'error'
    loadError.value = errorText(err)
  }
}

onMounted(async () => {
  // 上传限制取不到不影响浏览文件，失败时只是上传按钮暂不可用
  try {
    config.value = await fetchConfig()
  } catch {
    config.value = null
  }
  await Promise.all([loadCategories(), load()])
})

// 搜索输入防抖：每敲一个字就发一次请求既浪费也会让结果闪烁
let searchTimer: number | undefined
watch(search, () => {
  window.clearTimeout(searchTimer)
  searchTimer = window.setTimeout(() => {
    page.value = 1
    void load()
  }, 300)
})

// 切换分类或归档标签页都要回到第一页，否则会停在一个超出范围的页码上
watch(categoryKey, () => {
  page.value = 1
  void load()
})

watch(archivedTab, () => {
  page.value = 1
  void load()
})

function onPageChange(next: number) {
  page.value = next
  void load()
}

function onPageSizeChange(next: number) {
  pageSize.value = next
  page.value = 1
  void load()
}

/** 分类被增删改后：分类树要重取，文件列表也要重取（归属和筛选范围都可能变了）。 */
async function onCategoriesChanged() {
  await loadCategories()
  await load()
}

function openDetail(row: DocumentItem) {
  activeId.value = row.id
  drawerOpen.value = true
}

function onUploaded(doc: DocumentItem) {
  ElMessage.success(`已上传「${doc.name}」`)
  page.value = 1
  archivedTab.value = 'active'
  void load()
  void loadCategories()
}

async function toggleArchive(row: DocumentItem) {
  try {
    if (row.archived) {
      await restoreDocument(row.id)
      ElMessage.success(`已恢复「${row.name}」`)
    } else {
      await archiveDocument(row.id)
      ElMessage.success(`已归档「${row.name}」`)
    }
    void load()
    void loadCategories()
  } catch (err) {
    ElMessage.error(errorText(err))
  }
}

async function download(row: DocumentItem) {
  try {
    // 先取成 Blob 再保存：直接跳转到下载地址的话，失败时会显示一片 JSON
    const blob = await fetchDocumentBlob(row.id)
    saveBlob(blob, row.name)
  } catch (err) {
    ElMessage.error(`下载失败：${errorText(err)}`)
  }
}

/** 当前选中的分类标识，作为上传时的默认归属。 */
const selectedCategoryId = computed(() =>
  selection.value.mode === 'category' ? selection.value.id : null,
)
</script>

<template>
  <div class="documents-page">
    <aside class="category-aside">
      <CategoryTree v-model="categoryKey" :categories="categories" :loading="categoriesLoading"
        :error="categoriesError" @changed="onCategoriesChanged" @retry="loadCategories" />
    </aside>

    <div class="documents-content">
      <el-card shadow="never">
        <template #header>
          <div class="toolbar">
            <el-input v-model="search" class="toolbar-search" placeholder="按文件名搜索" clearable>
              <template #prefix>
                <svg class="search-icon" viewBox="0 0 16 16" aria-hidden="true">
                  <circle cx="7" cy="7" r="5" fill="none" stroke="currentColor" stroke-width="1.6" />
                  <path d="M11 11l4 4" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" />
                </svg>
              </template>
            </el-input>
            <div class="toolbar-right">
              <el-button :disabled="phase === 'loading'" @click="load">刷新</el-button>
              <el-tooltip content="正在获取服务端上传限制" placement="bottom" :disabled="!!config">
                <span>
                  <el-button type="primary" :disabled="!config" @click="uploadOpen = true">上传文件</el-button>
                </span>
              </el-tooltip>
            </div>
          </div>
        </template>

        <el-tabs v-model="archivedTab" class="tabs">
          <el-tab-pane label="使用中" name="active" />
          <el-tab-pane label="已归档" name="archived" />
        </el-tabs>

        <!-- 加载中 -->
        <el-skeleton v-if="phase === 'loading'" :rows="5" animated />

        <!-- 请求失败 -->
        <el-alert v-else-if="phase === 'error'" type="error" :closable="false" show-icon title="无法加载文件列表">
          <p class="error-detail">{{ loadError }}</p>
          <el-button text type="primary" @click="load">重试</el-button>
        </el-alert>

        <!-- 空结果 -->
        <el-empty v-else-if="!items.length" :description="emptyText">
          <el-button v-if="hasFilter" @click="search = ''; archivedTab = 'active'; categoryKey = 'all'">
            清除筛选
          </el-button>
        </el-empty>

        <!-- 数据 -->
        <template v-else>
          <el-table :data="items" row-key="id" class="table" @row-click="openDetail">
            <el-table-column label="文件名" min-width="240">
              <template #default="{ row }">
                <div class="cell-name">
                  <span class="name-text">{{ row.name }}</span>
                  <el-tag size="small" type="info" effect="plain" class="ext-tag">
                    {{ fileExtension(row.name).replace('.', '').toUpperCase() || '?' }}
                  </el-tag>
                </div>
              </template>
            </el-table-column>

            <el-table-column label="标签" min-width="140">
              <template #default="{ row }">
                <el-tag v-for="tag in row.tags" :key="tag" size="small" class="tag">{{ tag }}</el-tag>
                <span v-if="!row.tags.length" class="muted">—</span>
              </template>
            </el-table-column>

            <el-table-column label="分类" min-width="110">
              <template #default="{ row }">
                <span v-if="row.categoryName">{{ row.categoryName }}</span>
                <span v-else class="muted">未分类</span>
              </template>
            </el-table-column>

            <el-table-column label="大小" width="90">
              <template #default="{ row }">{{ formatBytes(row.sizeBytes) }}</template>
            </el-table-column>

            <el-table-column label="索引状态" width="110">
              <template #default="{ row }">
                <IndexStatusTag :status="row.indexStatus" />
              </template>
            </el-table-column>

            <el-table-column label="上传时间" width="160">
              <template #default="{ row }">{{ formatDateTime(row.createdAt) }}</template>
            </el-table-column>

            <el-table-column label="操作" width="170" align="right">
              <template #default="{ row }">
                <!-- 阻止冒泡，否则点按钮会同时打开详情抽屉 -->
                <el-button text type="primary" size="small" @click.stop="download(row)">下载</el-button>
                <el-button text size="small" @click.stop="toggleArchive(row)">
                  {{ row.archived ? '恢复' : '归档' }}
                </el-button>
              </template>
            </el-table-column>
          </el-table>

          <el-pagination class="pager" background layout="total, sizes, prev, pager, next" :total="total"
            :current-page="page" :page-size="pageSize" :page-sizes="[10, 20, 50]"
            @current-change="onPageChange" @size-change="onPageSizeChange" />
        </template>
      </el-card>
    </div>

    <UploadDialog v-model="uploadOpen" :config="config" :categories="categories"
      :category-id="selectedCategoryId" @uploaded="onUploaded" />
    <DocumentDrawer v-model="drawerOpen" :document-id="activeId" :categories="categories"
      @changed="onCategoriesChanged" />
  </div>
</template>

<style scoped>
.documents-page {
  display: flex;
  align-items: flex-start;
  gap: 16px;
}

.category-aside {
  flex: none;
  width: 248px;
  position: sticky;
  /* 让树在长列表滚动时保持可见；56px 是顶部 header 的高度 */
  top: 0;
}

.documents-content {
  flex: 1;
  min-width: 0;
}

/* 窄屏下改为上下排列，避免表格被压到无法阅读 */
@media (max-width: 900px) {
  .documents-page {
    flex-direction: column;
  }

  .category-aside {
    width: 100%;
    position: static;
  }
}

.toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
}

.toolbar-search {
  max-width: 320px;
}

.toolbar-right {
  display: flex;
  gap: 8px;
}

.search-icon {
  width: 14px;
  height: 14px;
  color: var(--el-text-color-placeholder);
}

.tabs {
  margin-bottom: 4px;
}

.table {
  width: 100%;
}

/* 表格行可点击进入详情，给出指针反馈 */
.table :deep(.el-table__row) {
  cursor: pointer;
}

.cell-name {
  display: flex;
  align-items: center;
  gap: 8px;
}

.name-text {
  word-break: break-all;
}

.ext-tag {
  flex: none;
}

.tag {
  margin-right: 4px;
}

.muted {
  color: var(--el-text-color-placeholder);
}

.pager {
  margin-top: 16px;
  justify-content: flex-end;
}

.error-detail {
  margin: 4px 0 8px;
  font-family: var(--el-font-family-mono, monospace);
  font-size: 13px;
}
</style>
