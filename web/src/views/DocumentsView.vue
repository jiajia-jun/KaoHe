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
  moveDocument,
  purgeDocument,
  restoreDocument,
  restoreFromTrash,
  saveBlob,
  updateDocument,
  type DocumentItem,
  type ServerConfig,
  type UploadBatchResult,
} from '@/api/documents'
import {
  fetchCategoryTree,
  flattenCategories,
  parseCategoryKey,
  type CategoryNode,
} from '@/api/categories'
import { formatBytes, formatDateTime, fileExtension } from '@/utils/format'
import { useDocumentDrag } from '@/composables/useDocumentDrag'
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

function onUploaded(result: UploadBatchResult) {
  const { succeeded, failed } = result
  if (succeeded.length) {
    // 单文件仍是「已上传「文件名」」；多于一个才换成计数，逐个念一遍反而看不清
    const ok =
      succeeded.length === 1 ? `已上传「${succeeded[0].name}」` : `已上传 ${succeeded.length} 个文件`
    // 有失败的仍报成功数，但把失败条数一并说清 —— 失败明细在上传对话框里逐行留着
    if (failed > 0) ElMessage.warning(`${ok}，${failed} 个失败`)
    else ElMessage.success(ok)

    page.value = 1
    // 新文件一定不在回收站或归档区里，上传完还停在那两个页面会让人以为没传上去
    tab.value = 'active'
    void load()
    void loadCategories()
  }
  // 整批都失败时不刷新，也不需要提示：对话框没关，失败原因就在用户眼前的每一行上
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

/**
 * 什么时候能拖。
 *
 * 回收站是暂存区，位置对它没有意义；搜索结果由相关性主导（打分排序在服务端，
 * 拖动一个按分数排出来的列表，用户说不清自己想要什么）。这两种情况下一律不给拖，
 * 而不是「拖了但顺序待会儿会被冲掉」—— 后者比干脆不能拖更让人困惑。
 */
const dragEnabled = computed(
  () =>
    (tab.value === 'active' || tab.value === 'archived') &&
    search.value.trim() === '' &&
    phase.value === 'ready',
)

/**
 * 长按拖拽。落点有两种：两行之间改顺序，分类节点上改归属。
 *
 * 拖动期间必须停掉索引轮询 —— 一次静默刷新会在拖动中途换掉整个列表，
 * 手里攥着的那一行就和屏幕上的对不上了。
 */
const {
  isDragging,
  draggingId,
  ghost: dragGhost,
  dropIndex,
  dropCatKey,
  onPointerDown: onTablePointerDown,
} = useDocumentDrag({
  available: () => dragEnabled.value,
  // 拖到第一行之上的含义是全局置顶，只有第一页的「上面」才真的是最前
  canDropAtTop: () => page.value === 1,
  onStart: stopIndexWatch,
  onEnd: watchIndexProgress,
  onReorder: (docId, afterId) => void reorder(docId, afterId),
  onDropCategory: (docId, categoryId) => void moveToCategory(docId, categoryId),
})

/** 跟手的文件条上显示的名字。从列表里现取，不让拖拽逻辑再抄一份行数据。 */
const draggingName = computed(
  () => items.value.find((item) => item.id === draggingId.value)?.name ?? '',
)

/**
 * 插入线的落点样式。
 *
 * 写成普通函数而不是 computed：el-table 只在自身重渲染时调用它，而这里读的
 * 是拖动过程中每次 pointermove 都在变的 ref —— 必须现读现算。用 computed
 * 会把第一次算出来的值缓存住，线就画不动了。
 */
function rowClassName({ rowIndex }: { rowIndex: number }): string {
  if (!isDragging.value) return ''
  const target = dropIndex.value
  if (target === null) return ''
  // 落到最后一行之后：标记最后一行即可，不必为了画一条线在表尾塞个占位元素
  if (target >= items.value.length) {
    return rowIndex === items.value.length - 1 ? 'is-drop-after' : ''
  }
  return rowIndex === target ? 'is-drop-before' : ''
}

/**
 * 把文件挪到另一份之后（afterId 为 null 即置顶）。
 *
 * 乐观更新：先把本地列表挪好，让手势的落点和眼前的结果立刻对上，再把请求发出去；
 * 失败就重新取一次，回到服务端的真实顺序。
 *
 * 不弹成功提示，也不给撤销 —— 结果就在眼前，不满意拖回去即可。
 * 这与「移入回收站」「移入分类」正好相反：那两步的数据离开了视野，
 * 才需要一条带撤销入口的提示把它找回来（DESIGN.md §3.8：反馈强度按可逆性来定）。
 */
async function reorder(rowId: string, afterId: string | null) {
  const from = items.value.findIndex((item) => item.id === rowId)
  if (from < 0) return

  const rest = items.value.filter((_, index) => index !== from)
  let at = 0
  if (afterId !== null) {
    // 锚点不在本页时本地排不出来。服务端能处理，但屏幕上会先闪一个错的顺序，
    // 不如什么都不做 —— 跨页拖动本来也不在支持范围内（见 DESIGN.md §5）。
    const anchor = rest.findIndex((item) => item.id === afterId)
    if (anchor < 0) return
    at = anchor + 1
  }

  const next = [...rest.slice(0, at), items.value[from], ...rest.slice(at)]
  // 顺序没变（拖回原位）就不发请求：服务端也会判定为「没有变化」，白等一趟
  if (next.every((item, index) => item.id === items.value[index].id)) return

  items.value = next
  try {
    await moveDocument(rowId, afterId)
  } catch (err) {
    ElMessage.error(`调整顺序失败：${errorText(err)}`)
    // 服务端才是准的：拉回真实顺序，别把一个错的顺序留在屏幕上
    void load(true)
  }
}

/**
 * 把文件拖到分类节点上。
 *
 * 直接移动、不弹确认：这一步与「移入回收站」同级，随时可以反悔，
 * 所以按同一套做法给一条带撤销入口的提示就够了（DESIGN.md §3.8）。
 */
async function moveToCategory(docId: string, categoryId: number | null) {
  const row = items.value.find((item) => item.id === docId)
  if (!row) return
  // 已经在这个分类里（含「本来就是未分类」）就什么都不做，也不发请求
  if ((row.categoryId ?? null) === categoryId) return

  const previous = row.categoryId ?? null
  try {
    await updateDocument(docId, { categoryId })
    notifyMoved(row, categoryId, previous)
    void load()
    void loadCategories()
  } catch (err) {
    ElMessage.error(`移动分类失败：${errorText(err)}`)
  }
}

/** 分类名：撤销时要能说清「移回哪里」，拿标识糊弄等于没提示。 */
function categoryLabel(id: number | null): string {
  if (id === null) return '未分类'
  const name = flattenCategories(categories.value).find((item) => item.id === id)?.name
  return name ? `「${name}」` : '原分类'
}

/**
 * 移动分类后的提示，带一个就地撤销的入口。
 *
 * 时长与写法都跟「移入回收站」保持一致：同样是「数据离开了原来的位置」，
 * 用户需要一条能立刻反悔的通路。而拖动排序不给撤销 —— 结果就在眼前，拖回去即可。
 */
function notifyMoved(row: DocumentItem, categoryId: number | null, previous: number | null) {
  const instance = ElMessage({
    type: 'success',
    duration: 6000,
    message: h('div', { style: 'display:flex;align-items:center;gap:12px' }, [
      h('span', null, `已把「${row.name}」移入${categoryLabel(categoryId)}`),
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
            void undoMove(row.id, row.name, previous)
          },
        },
        () => '撤销',
      ),
    ]),
  })
}

/** 撤销移分类。产物就是一次普通的改归属，失败按普通错误提示，不做二次撤销。 */
async function undoMove(docId: string, name: string, categoryId: number | null) {
  try {
    await updateDocument(docId, { categoryId })
    ElMessage.success(`已把「${name}」移回${categoryLabel(categoryId)}`)
    void load()
    void loadCategories()
  } catch (err) {
    ElMessage.error(errorText(err))
  }
}
</script>

<template>
  <div class="documents-page">
    <aside class="category-aside">
      <CategoryTree v-model="categoryKey" :categories="categories" :loading="categoriesLoading"
        :error="categoriesError" :drop-key="dropCatKey" @changed="onCategoriesChanged"
        @retry="loadCategories" />
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
          <!-- pointerdown 挂在表格外面这一层，而不是 el-table 上：
               el-table 会不会把原生事件透传到根节点，取决于它的 inheritAttrs 设置，
               包一层是确定的，也让「委托到整个表格」这件事在模板里看得见 -->
          <div class="table-wrap" @pointerdown="onTablePointerDown">
          <!-- 列宽合计 918 是算出来的，不是试出来的。
               1280×720 下表格容器只有 934px（视口 − el-main 40 − 分类栏 248 − 间距 16
               − el-card 40），这 934 就是全部预算，而操作列右对齐 —— 一旦列宽合计超了，
               「下载/归档/删除」会整排落到屏幕外，只能横向滚动表格才点得到。
               Playwright 点击前会把元素滚进视口，所以浏览器用例抓不到这种溢出，
               要靠 e2e/05-layout.spec.ts 里直接量 clientWidth/scrollWidth 的那条来钉。
               各列取「实测的单行内容宽度 + 16 内边距」，多出来的余量全给文件名。 -->
          <el-table :data="items" row-key="id" class="table" @row-click="openDetail"
            :row-class-name="rowClassName">
            <!-- 抓手列：纯长按没有任何可见线索，而一个没人能发现的交互与不提供它
                 是等价的（CategoryTree 里对删除按钮写过同一句话）。它是显式入口，
                 按下即可拖；行内别处按住 380ms 同样能拖，两条路都留。 -->
            <el-table-column v-if="tab !== 'trash'" width="38" class-name="col-grab">
              <template #default>
                <span v-if="dragEnabled" class="doc-grab" aria-label="拖动调整位置"
                  title="按住拖动可调整位置，或拖到左侧分类上">
                  <svg viewBox="0 0 16 16" aria-hidden="true">
                    <circle cx="6" cy="4.5" r="1.2" />
                    <circle cx="6" cy="8" r="1.2" />
                    <circle cx="6" cy="11.5" r="1.2" />
                    <circle cx="10" cy="4.5" r="1.2" />
                    <circle cx="10" cy="8" r="1.2" />
                    <circle cx="10" cy="11.5" r="1.2" />
                  </svg>
                </span>
              </template>
            </el-table-column>

            <el-table-column label="文件名" min-width="210">
              <template #default="{ row }">
                <!-- data-doc-id 是拖拽的行锚点：el-table 不给 tr 挂自定义属性，
                     所以打在这一格上，需要行元素时再往上找它的 tr -->
                <div class="cell-name" :data-doc-id="row.id">
                  <span class="name-text">{{ row.name }}</span>
                  <el-tag size="small" type="info" effect="plain" class="ext-tag">
                    {{ fileExtension(row.name).replace('.', '').toUpperCase() || '?' }}
                  </el-tag>
                </div>
              </template>
            </el-table-column>

            <el-table-column label="标签" min-width="64">
              <template #default="{ row }">
                <el-tag v-for="tag in row.tags" :key="tag" size="small" class="tag">{{ tag }}</el-tag>
                <span v-if="!row.tags.length" class="muted">—</span>
              </template>
            </el-table-column>

            <el-table-column label="分类" min-width="104">
              <template #default="{ row }">
                <span v-if="row.categoryName">{{ row.categoryName }}</span>
                <span v-else class="muted">未分类</span>
              </template>
            </el-table-column>

            <el-table-column label="大小" width="82">
              <template #default="{ row }">{{ formatBytes(row.sizeBytes) }}</template>
            </el-table-column>

            <el-table-column label="索引状态" width="84">
              <template #default="{ row }">
                <IndexStatusTag :status="row.indexStatus" />
              </template>
            </el-table-column>

            <el-table-column label="上传时间" min-width="132">
              <template #default="{ row }">{{ formatDateTime(row.createdAt) }}</template>
            </el-table-column>

            <!-- 204 是按回收站那一格定的：下载/恢复/彻底删除 三个按钮单行要 202px，
                 比「使用中」的 下载/归档/删除 还宽 20px（「彻底删除」四个字）。
                 操作列拿的是「绝不能退化」的预算 —— 按钮折行看起来像 bug，
                 而文件名折行是正常排版，所以这 20px 从文件名身上挪。 -->
            <el-table-column label="操作" width="204" align="right">
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
          </div>

          <!-- 跟手的文件条。模板渲染而不是手工造 DOM：样式、字体、主题变量
               跟着组件走，不用在 JS 里再写一份。
               pointer-events: none 是必须的 —— 否则 elementFromPoint 只会命中它自己，
               落点就永远算不出来。 -->
          <div v-if="isDragging && draggingName" class="drag-ghost"
            :style="{ left: `${dragGhost.x}px`, top: `${dragGhost.y}px` }">
            {{ draggingName }}
          </div>

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

/* 单元格内边距 12 → 8。这不只是让表格更紧凑：8 列各让出 8px 就是 64px，
   正好是 1280 宽下把横向滚动条挤出去所需的余量。Element Plus 自己的
   .el-table .cell 是 0 12px，这里按优先级盖掉它。 */
.table :deep(.cell) {
  padding: 0 8px;
}

/* 表格行可点击进入详情，给出指针反馈 */
.table :deep(.el-table__row) {
  cursor: pointer;
}

/* 抓手：常显，且明确告诉用户「这里可以按住」。没有它的话，
   长按拖拽是一个没有任何线索的手势。 */
.doc-grab {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 20px;
  height: 20px;
  /* 原来这里有个 -4px（抵消 12px 内边距），内边距降到 8 之后
     它会把图标顶出内容区，而 .cell 是 overflow:hidden，会被裁掉左边一条 */
  border-radius: 4px;
  color: var(--el-text-color-placeholder);
  cursor: grab;
  /* 触摸屏上按住拖动时，别让浏览器把它当成滚动 */
  touch-action: none;
}

.doc-grab svg {
  width: 15px;
  height: 15px;
  fill: currentColor;
}

.doc-grab:hover {
  background: var(--el-fill-color);
  color: var(--el-text-color-regular);
}

/* 拖动期间整页统一指针：指针滑到行外时，抓手的形状会让人以为拖不动了 */
:global(body.is-doc-dragging) {
  cursor: grabbing;
  /* 拖动过程中不该选出文字来 —— 表体里到处是文本节点 */
  user-select: none;
}

/* 插入线。画在 td 上而不是 tr 上：表格行的 box-shadow 在各浏览器上渲染不一致。
   target >= 行数 时标记最后一行，线就落在表格底部，不需要尾部占位元素。 */
.table :deep(.el-table__row.is-drop-before > td) {
  box-shadow: inset 0 2px 0 0 var(--el-color-primary) !important;
}

.table :deep(.el-table__row.is-drop-after > td) {
  box-shadow: inset 0 -2px 0 0 var(--el-color-primary) !important;
}

.drag-ghost {
  position: fixed;
  z-index: 3000;
  max-width: 260px;
  padding: 4px 10px;
  border: 1px solid var(--el-color-primary);
  border-radius: 4px;
  background: var(--el-bg-color);
  color: var(--el-text-color-primary);
  font-size: 12px;
  line-height: 20px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  box-shadow: var(--el-box-shadow-light);
  /* 见模板里的说明：不穿透的话落点判定只会命中它自己 */
  pointer-events: none;
  /* 稍微偏右下，别让文件条压住指针底下的那一行 */
  transform: translate(12px, -50%);
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
