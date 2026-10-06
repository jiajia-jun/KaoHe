<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { errorText } from '@/api/client'
import { fetchDocumentBlob, saveBlob, type DocumentItem } from '@/api/documents'
import { searchKeyword, searchSemantic, type SearchResult } from '@/api/search'
import { fetchCategoryTree, flattenCategories, type CategoryNode } from '@/api/categories'
import { formatBytes, formatDateTime } from '@/utils/format'
import IndexStatusTag from '@/components/IndexStatusTag.vue'
import DocumentDrawer from '@/components/DocumentDrawer.vue'

/**
 * 知识检索页：关键词检索与语义检索并排成两个标签页。
 *
 * 两条路径共用结果列表：用户看到的都是「哪份文件 + 命中的片段」，
 * 区别只在命中怎么算出来的。分成两套渲染会让两边的信息展示慢慢长歪。
 *
 * 三种状态（加载中 / 成功 / 失败）与两种空状态（还没搜过 / 搜了但没结果）
 * 分开渲染 —— 空结果和请求失败看起来一模一样，是最容易误导用户的一类界面缺陷。
 */

type Mode = 'keyword' | 'semantic'
type Phase = 'idle' | 'loading' | 'ready' | 'error'

const mode = ref<Mode>('keyword')
const input = ref('')
const phase = ref<Phase>('idle')
const result = ref<SearchResult | null>(null)
const errorMessage = ref('')
/** 已经搜过的词，用于区分「还没搜」与「搜了没结果」 */
const searched = ref('')
/** 上一次用的检索方式，切换标签页时结果要跟着清掉，否则会张冠李戴 */
const searchedMode = ref<Mode>('keyword')

const categoryKey = ref('all')
const categories = ref<CategoryNode[]>([])
const includeArchived = ref(false)

const drawerOpen = ref(false)
const activeId = ref<string | null>(null)

const categoryOptions = computed(() =>
  flattenCategories(categories.value).map((item) => ({
    value: item.id,
    label: '　'.repeat(item.depth) + item.name,
  })),
)

const selectedCategoryId = computed(() => (categoryKey.value === 'all' ? null : Number(categoryKey.value)))

const placeholder = computed(() =>
  mode.value === 'keyword'
    ? '按文件名或正文关键词搜索，例如：发布检查清单'
    : '用一句话描述你要找的内容，例如：容器起来了但是服务还是用不了',
)

const hint = computed(() =>
  mode.value === 'keyword'
    ? '关键词检索按字面匹配：文件名或正文里出现这个词才会命中。'
    : '语义检索按意思匹配：不必和原文用词一致，只要描述得接近就能找到；相似度低于阈值的片段不会展示。',
)

async function loadCategories() {
  try {
    categories.value = await fetchCategoryTree()
  } catch {
    // 分类筛选取不到不影响检索本身，退化成「全部分类」
    categories.value = []
  }
}

async function run() {
  const query = input.value.trim()
  if (!query) {
    ElMessage.warning(mode.value === 'keyword' ? '请输入要检索的关键词' : '请输入要检索的内容描述')
    return
  }

  phase.value = 'loading'
  errorMessage.value = ''
  searched.value = query
  searchedMode.value = mode.value
  try {
    const options = {
      categoryId: selectedCategoryId.value,
      archived: includeArchived.value,
    }
    result.value =
      mode.value === 'keyword'
        ? await searchKeyword(query, options)
        : await searchSemantic(query, options)
    phase.value = 'ready'
  } catch (err) {
    result.value = null
    phase.value = 'error'
    errorMessage.value = errorText(err)
  }
}

/**
 * 切换检索方式时清空结果。
 * 不清的话，语义检索的结果会留在关键词检索的标签页下，
 * 而用户看不出这些结果是另一种方式算出来的。
 */
function onModeChange() {
  result.value = null
  phase.value = 'idle'
  errorMessage.value = ''
}

function openDetail(doc: DocumentItem) {
  activeId.value = doc.id
  drawerOpen.value = true
}

async function download(doc: DocumentItem) {
  try {
    const blob = await fetchDocumentBlob(doc.id)
    saveBlob(blob, doc.name)
  } catch (err) {
    ElMessage.error(`下载失败：${errorText(err)}`)
  }
}

/** 命中序号从 0 开始，给用户看时从 1 开始。 */
function partLabel(ordinal: number): string {
  return `第 ${ordinal + 1} 段`
}

function scoreText(score: number): string {
  return score.toFixed(2)
}

/** 相似度的颜色分档。只为了让人一眼看出哪些是强命中，不追求精确刻度。 */
function scoreType(score: number): 'success' | 'warning' | 'info' {
  if (score >= 0.6) return 'success'
  if (score >= 0.5) return 'warning'
  return 'info'
}

onMounted(loadCategories)
</script>

<template>
  <div class="search-page">
    <el-card shadow="never">
      <template #header>
        <div class="card-header">
          <span>知识检索</span>
          <span class="header-hint">在已入库的文件里找内容</span>
        </div>
      </template>

      <el-tabs v-model="mode" class="mode-tabs" @tab-change="onModeChange">
        <el-tab-pane label="关键词检索" name="keyword" />
        <el-tab-pane label="语义检索" name="semantic" />
      </el-tabs>
      <p class="hint">{{ hint }}</p>

      <div class="query-row">
        <el-input v-model="input" :placeholder="placeholder" clearable size="large"
          @keyup.enter="run" />
        <el-button type="primary" size="large" :loading="phase === 'loading'" @click="run">
          检索
        </el-button>
      </div>

      <div class="filters">
        <el-select v-model="categoryKey" class="category-select" placeholder="全部分类">
          <el-option label="全部分类" value="all" />
          <el-option v-for="option in categoryOptions" :key="option.value" :label="option.label"
            :value="option.value" />
        </el-select>
        <el-checkbox v-model="includeArchived" label="包含已归档的文件" />
      </div>

      <!-- 加载中 -->
      <el-skeleton v-if="phase === 'loading'" :rows="4" animated />

      <!-- 请求失败 -->
      <el-alert v-else-if="phase === 'error'" type="error" :closable="false" show-icon
        :title="searchedMode === 'semantic' ? '语义检索失败' : '关键词检索失败'">
        <p class="error-detail">{{ errorMessage }}</p>
        <el-button text type="primary" @click="run">重试</el-button>
      </el-alert>

      <!-- 还没搜过 -->
      <el-empty v-else-if="phase === 'idle'" description="输入关键词或一句话描述，开始检索" />

      <!-- 搜了但没结果 -->
      <el-empty v-else-if="!result?.items.length">
        <template #description>
          <p>没有找到与「{{ searched }}」相关的内容</p>
          <p class="empty-hint">
            {{ searchedMode === 'semantic'
              ? '语义检索只展示相似度达标的结果。可以换个说法再试，或改用关键词检索。'
              : '关键词按字面匹配，可以换一个更短的词再试。' }}
          </p>
        </template>
      </el-empty>

      <!-- 结果 -->
      <template v-else>
        <div class="result-meta">
          <span>
            共 {{ result.total }} 份文件命中
            <template v-if="searchedMode === 'semantic' && result.minScore">
              ，相似度低于 {{ result.minScore.toFixed(2) }} 的片段未展示
            </template>
          </span>
        </div>

        <div v-for="item in result.items" :key="item.document.id" class="hit">
          <div class="hit-head">
            <el-button text type="primary" class="hit-name" @click="openDetail(item.document)">
              {{ item.document.name }}
            </el-button>
            <el-tag v-if="item.score !== undefined" size="small" :type="scoreType(item.score)">
              相关度 {{ scoreText(item.score) }}
            </el-tag>
            <IndexStatusTag :status="item.document.indexStatus" />
          </div>

          <div class="hit-meta">
            <span>{{ item.document.categoryName ?? '未分类' }}</span>
            <span>{{ formatBytes(item.document.sizeBytes) }}</span>
            <span>{{ formatDateTime(item.document.createdAt) }}</span>
            <el-tag v-if="item.nameHit" size="small" type="warning" effect="plain">文件名命中</el-tag>
            <span v-if="item.bodyHits" class="muted">正文命中 {{ item.bodyHits }} 段</span>
          </div>

          <ul v-if="item.matches.length" class="matches">
            <li v-for="match in item.matches" :key="match.ordinal" class="match">
              <span class="match-label">
                {{ partLabel(match.ordinal) }}
                <template v-if="match.score !== undefined">· {{ scoreText(match.score) }}</template>
              </span>
              <span class="match-text">{{ match.text }}</span>
            </li>
          </ul>
          <p v-else class="muted no-match">
            仅在文件名中命中；该文件没有可检索的正文（PDF 只做存储与预览）。
          </p>

          <div class="hit-actions">
            <el-button text size="small" @click="openDetail(item.document)">查看详情</el-button>
            <el-button text size="small" @click="download(item.document)">下载</el-button>
          </div>
        </div>
      </template>
    </el-card>

    <DocumentDrawer v-model="drawerOpen" :document-id="activeId" :categories="categories" />
  </div>
</template>

<style scoped>
.search-page {
  max-width: 900px;
}

.card-header {
  display: flex;
  align-items: baseline;
  gap: 12px;
}

.header-hint {
  font-size: 13px;
  color: var(--el-text-color-secondary);
}

.mode-tabs {
  margin-bottom: 0;
}

.hint {
  margin: -8px 0 16px;
  font-size: 13px;
  color: var(--el-text-color-secondary);
}

.query-row {
  display: flex;
  gap: 8px;
}

.filters {
  display: flex;
  align-items: center;
  gap: 16px;
  margin: 12px 0 20px;
}

.category-select {
  width: 220px;
}

.result-meta {
  margin-bottom: 12px;
  font-size: 13px;
  color: var(--el-text-color-secondary);
}

/* 一份文件一张卡片：命中片段可能有多段，用分隔线划开比嵌套卡片清爽 */
.hit {
  padding: 14px 0;
  border-top: 1px solid var(--el-border-color-lighter);
}

.hit:first-of-type {
  border-top: none;
}

.hit-head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}

.hit-name {
  font-size: 15px;
  font-weight: 600;
  height: auto;
  padding: 0;
  word-break: break-all;
  white-space: normal;
  text-align: left;
}

.hit-meta {
  display: flex;
  flex-wrap: wrap;
  gap: 12px;
  margin: 4px 0 8px;
  font-size: 12px;
  color: var(--el-text-color-secondary);
}

.matches {
  margin: 0;
  padding: 0;
  list-style: none;
}

.match {
  display: flex;
  gap: 8px;
  padding: 6px 10px;
  margin-bottom: 6px;
  background: var(--el-fill-color-light);
  border-radius: 4px;
  font-size: 13px;
  line-height: 1.7;
}

.match-label {
  flex: none;
  color: var(--el-text-color-placeholder);
  font-variant-numeric: tabular-nums;
}

.match-text {
  word-break: break-word;
}

.no-match {
  margin: 0 0 4px;
  font-size: 13px;
}

.hit-actions {
  display: flex;
  gap: 4px;
}

.muted {
  color: var(--el-text-color-placeholder);
}

.empty-hint {
  margin-top: 4px;
  font-size: 13px;
  color: var(--el-text-color-secondary);
}

.error-detail {
  margin: 4px 0 8px;
  font-family: var(--el-font-family-mono, monospace);
  font-size: 13px;
}
</style>
