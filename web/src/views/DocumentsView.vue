<script setup lang="ts">
import { computed, h, onMounted, onUnmounted, ref, watch } from 'vue'
import { ElButton, ElMessage, ElMessageBox } from 'element-plus'
import { errorText } from '@/api/client'
import {
  archiveDocument,
  deleteDocument,
  emptyTrash,
  fetchConfig,
  fetchDocumentBlob,
  isIndexing,
  listDocuments,
  purgeDocument,
  restoreDocument,
  restoreFromTrash,
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

/**
 * 三个标签页对应三种互斥的取数范围，而不是三个可以叠加的筛选条件：
 * 「使用中」是不归档且没删的，「已归档」是归档且没删的，「回收站」是删掉的。
 * 一份文件同时只能出现在其中一个里 —— 叠加条件会让它同时属于多个页面，
 * 用户删掉之后还能在「使用中」看见它，就只能理解成删除失败了。
 */
type DocTab = 'active' | 'archived' | 'trash'
const tab = ref<DocTab>('active')

const categoryKey = ref('all')
const categories = ref<CategoryNode[]>([])
const categoriesLoading = ref(false)
const categoriesError = ref('')

const config = ref<ServerConfig | null>(null)
const configError = ref('')
const uploadOpen = ref(false)
const drawerOpen = ref(false)
const activeId = ref<string | null>(null)

const selection = computed(() => parseCategoryKey(categoryKey.value))
const hasFilter = computed(
  () => search.value.trim() !== '' || categoryKey.value !== 'all' || tab.value !== 'active',
)
const emptyText = computed(() => {
  if (search.value.trim()) return '没有匹配的文件'
  if (categoryKey.value === 'none') return '还没有未分类的文件'
  if (selection.value.mode === 'category') return '这个分类下还没有文件'
  if (tab.value === 'archived') return '还没有归档的文件'
  if (tab.value === 'trash') return '回收站是空的'
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

/**
 * silent 用于后台轮询：不切回加载态。
 * 否则每 1.5 秒列表就会闪一次骨架屏，比状态更新本身更惹眼。
 */
async function load(silent = false) {
  if (!silent) phase.value = 'loading'
  loadError.value = ''
  try {
    const current = parseCategoryKey(categoryKey.value)
    const result = await listDocuments({
      q: search.value.trim(),
      categoryId: current.mode === 'category' ? current.id : undefined,
      uncategorized: current.mode === 'none',
      archived: tab.value === 'archived',
      trashed: tab.value === 'trash',
      page: page.value,
      pageSize: pageSize.value,
    })
    items.value = result.items
    total.value = result.total
    // 删除或归档掉当前页最后一条后页码可能已越界，回退到最后一页重取
    const lastPage = Math.max(1, Math.ceil(result.total / pageSize.value))
    if (page.value > lastPage) {
      page.value = lastPage
      return load(silent)
    }
    phase.value = 'ready'
  } catch (err) {
    // 轮询失败保留上一次的内容，只把错误留给下一次成功覆盖
    if (silent) return
    phase.value = 'error'
    loadError.value = errorText(err)
    return
  }
  // 每次取到新数据都重新判断一次是否要继续跟：上传、切换筛选后同样要跟上
  watchIndexProgress()
}

/**
 * 索引在后台进行，列表上的状态会自己往前走。
 * 只要还有文件停在待索引/索引中，就隔一会儿静默刷新一次；
 * 全部落定后停下来，不留一个常驻的定时器。
 */
let indexTimer: number | undefined

function stopIndexWatch() {
  window.clearTimeout(indexTimer)
  indexTimer = undefined
}

function watchIndexProgress() {
  stopIndexWatch()
  if (phase.value !== 'ready' || !items.value.some((item) => isIndexing(item.indexStatus))) return
  indexTimer = window.setTimeout(() => void load(true), 1500)
}

/**
 * 取服务端的上传限制。
 *
 * 取不到不影响浏览、检索、下载，所以不让它连累整页；
 * 但上传按钮会一直不可用，因此必须把「正在取」和「取失败了」分开记 ——
 * 一直显示「正在获取…」会把一次已经失败的请求伪装成还在进行。
 */
async function loadConfig() {
  configError.value = ''
  try {
    config.value = await fetchConfig()
  } catch (err) {
    config.value = null
    configError.value = errorText(err)
  }
}

onMounted(() => {
  void loadConfig()
  void Promise.all([loadCategories(), load()])
})

onUnmounted(stopIndexWatch)

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

watch(tab, () => {
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
  // 新文件一定不在回收站或归档区里，上传完还停在那两个页面会让人以为没传上去
  tab.value = 'active'
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

/**
 * 删除成功后的提示，带一个就地撤销的入口。
 *
 * 用 Element Plus 的 message 配渲染函数，而不是引入通知组件：
 * 撤销就发生在这条提示上，不该再让用户去别处找入口。
 */
function notifyTrashed(row: DocumentItem) {
  const instance = ElMessage({
    type: 'success',
    // 比默认的 3 秒长：撤销是要用户做个决定的，一闪而过等于没给这个机会
    duration: 6000,
    message: h('div', { style: 'display:flex;align-items:center;gap:12px' }, [
      h('span', null, `「${row.name}」已移入回收站`),
      h(
        ElButton,
        {
          text: true,
          type: 'primary',
          size: 'small',
          // 先关掉提示再发请求：撤销失败会另起一条错误提示，
          // 两条消息叠在一起时，用户分不清哪条说的是当前状态
          onClick: () => {
            instance.close()
            void putBack(row)
          },
        },
        () => '撤销',
      ),
    ]),
  })
}

/** 移入回收站。不弹确认框：这一步随时可以反悔，拦一道只会让人对着能撤销的操作犹豫。 */
async function moveToTrash(row: DocumentItem) {
  try {
    await deleteDocument(row.id)
    notifyTrashed(row)
    void load()
    void loadCategories()
  } catch (err) {
    ElMessage.error(errorText(err))
  }
}

/** 从回收站恢复。撤销按钮走的也是这里 —— 两者要做的事完全一样，没有第二套逻辑。 */
async function putBack(row: DocumentItem) {
  try {
    await restoreFromTrash(row.id)
    ElMessage.success(`已恢复「${row.name}」`)
    void load()
    void loadCategories()
  } catch (err) {
    ElMessage.error(errorText(err))
  }
}

/** 彻底删除单条。这一步不可逆，所以要弹确认，且文案必须说清「无法恢复」。 */
async function purge(row: DocumentItem) {
  try {
    await ElMessageBox.confirm(
      `「${row.name}」将被永久删除，原文件与已经建立的索引一并消失，无法恢复。`,
      '彻底删除',
      {
        type: 'warning',
        confirmButtonText: '永久删除',
        cancelButtonText: '取消',
        confirmButtonClass: 'el-button--danger',
      },
    )
  } catch {
    return // 用户取消
  }
  try {
    await purgeDocument(row.id)
    ElMessage.success(`已彻底删除「${row.name}」`)
    void load()
    void loadCategories()
  } catch (err) {
    ElMessage.error(errorText(err))
  }
}

/** 清空回收站。删掉的条数由服务端返回，不拿列表当前页的数字凑 —— 那一页可能被搜索过滤过。 */
async function clearTrash() {
  try {
    await ElMessageBox.confirm(
      '回收站里的所有文件都会被永久删除，原文件与已经建立的索引一并消失，无法恢复。',
      '清空回收站',
      {
        type: 'warning',
        confirmButtonText: '清空回收站',
        cancelButtonText: '取消',
        confirmButtonClass: 'el-button--danger',
      },
    )
  } catch {
    return // 用户取消
  }
  try {
    const result = await emptyTrash()
    ElMessage.success(result.message)
    void load()
    void loadCategories()
  } catch (err) {
    ElMessage.error(errorText(err))
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
              <!-- 只在回收站里出现：清空是一个把整个页面删空的动作，
                   让它常驻在文件列表旁边，离误点太近 -->
              <el-button v-if="tab === 'trash'" type="danger" plain @click="clearTrash">
                清空回收站
              </el-button>
              <!-- 必须写成 load()：直接写 load 会把点击事件当成第一个参数传进 silent，
                   于是loading 态不出现，失败时还会被 `if (silent) return` 吞掉 -->
              <el-button :disabled="phase === 'loading'" @click="load()">刷新</el-button>
              <!-- 上传限制拿不到就先不给点：对话框里的体积与格式校验都以它为准，
                   放进去也只会得到一个「正在获取…，请稍候重试」的空壳 -->
              <el-tooltip :content="configError ? `拿不到上传限制：${configError}` : '正在获取服务端上传限制'"
                placement="bottom" :disabled="!!config">
                <span>
                  <el-button type="primary" :disabled="!config" @click="uploadOpen = true">上传文件</el-button>
                </span>
              </el-tooltip>
              <el-button v-if="configError" text type="primary" @click="loadConfig">重试</el-button>
            </div>
          </div>
        </template>

        <el-tabs v-model="tab" class="tabs">
          <el-tab-pane label="使用中" name="active" />
          <el-tab-pane label="已归档" name="archived" />
          <el-tab-pane label="回收站" name="trash" />
        </el-tabs>

        <!-- 加载中 -->
        <el-skeleton v-if="phase === 'loading'" :rows="5" animated />

        <!-- 请求失败 -->
        <el-alert v-else-if="phase === 'error'" type="error" :closable="false" show-icon title="无法加载文件列表">
          <p class="error-detail">{{ loadError }}</p>
          <el-button text type="primary" @click="load()">重试</el-button>
        </el-alert>

        <!-- 空结果 -->
        <el-empty v-else-if="!items.length" :description="emptyText">
          <el-button v-if="hasFilter" @click="search = ''; tab = 'active'; categoryKey = 'all'">
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

            <el-table-column label="操作" width="220" align="right">
              <template #default="{ row }">
                <!-- 阻止冒泡，否则点按钮会同时打开详情抽屉 -->
                <el-button text type="primary" size="small" @click.stop="download(row)">下载</el-button>
                <!-- 回收站里的行只给「恢复」和「彻底删除」两个动作：
                     归档/取消归档对一份已经删掉的文件没有意义，
                     而把它放回去之后再改归属，路径也更短 -->
                <template v-if="tab === 'trash'">
                  <el-button text size="small" @click.stop="putBack(row)">恢复</el-button>
                  <el-button text type="danger" size="small" @click.stop="purge(row)">彻底删除</el-button>
                </template>
                <template v-else>
                  <el-button text size="small" @click.stop="toggleArchive(row)">
                    {{ row.archived ? '恢复' : '归档' }}
                  </el-button>
                  <el-button text type="danger" size="small" @click.stop="moveToTrash(row)">删除</el-button>
                </template>
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
