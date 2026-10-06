<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import type { UploadFile, UploadInstance } from 'element-plus'
import { errorText } from '@/api/client'
import {
  uploadDocument,
  type DocumentItem,
  type ServerConfig,
  type UploadBatchResult,
} from '@/api/documents'
import { flattenCategories, type CategoryNode } from '@/api/categories'
import { formatBytes, fileExtension } from '@/utils/format'

/**
 * 上传对话框：拖拽或点选若干文件，校验通过后逐个提交。
 *
 * 一次可选多个文件，提交时按顺序逐个调用单文件接口，而不是新开一个批量接口：
 * 服务端的超限判定、格式校验、落盘回滚一行都不用改，而且每个文件有各自的
 * 进度与成败 —— 同批里有一个格式不对，不会连累其余文件，用户也不用整批重来。
 *
 * 体积与格式的判定标准来自服务端的 /config，前端不另写一份常量，
 * 否则改了环境变量就会出现“界面说可以、服务端说不行”。
 * 这里的校验只是提前拦下明显不合格的文件，服务端仍会独立校验一次。
 */
const props = defineProps<{
  modelValue: boolean
  config: ServerConfig | null
  categories: CategoryNode[]
  /** 分类树上当前选中的分类，作为本批文件的默认归属 */
  categoryId: number | null
}>()

const emit = defineEmits<{
  'update:modelValue': [value: boolean]
  uploaded: [result: UploadBatchResult]
}>()

const visible = computed({
  get: () => props.modelValue,
  set: (value) => emit('update:modelValue', value),
})

/**
 * 列表里的一行 = 一个待上传文件。
 * invalid 与 failed 要分开：前者是校验就没过、不会发请求，
 * 后者是服务端拒了，再点一次上传就是重试。
 */
type EntryStatus = 'invalid' | 'ready' | 'uploading' | 'done' | 'failed'

interface UploadEntry {
  key: number
  file: File
  status: EntryStatus
  /** 0–100，仅上传中有意义 */
  percent: number
  /** 校验或上传失败的原因，直接展示在该行上 */
  reason: string
}

const STATUS_TEXT: Record<EntryStatus, string> = {
  invalid: '未通过校验',
  ready: '待上传',
  uploading: '上传中',
  done: '已上传',
  failed: '上传失败',
}

const uploadRef = ref<UploadInstance>()
const entries = ref<UploadEntry[]>([])
const tagsInput = ref('')
const categoryChoice = ref<number | null>(null)
const uploading = ref(false)
/** 本轮提交的总数与已完成数：进度文案要按轮次算，不能用在提交过程中会缩水的队列长度 */
const batchTotal = ref(0)
const batchDone = ref(0)
let nextKey = 0

// 用全角空格做缩进，在下拉里体现层级：「研发 / 前端」与顶层「前端」需要能区分开
const categoryOptions = computed(() =>
  flattenCategories(props.categories).map((item) => ({
    value: item.id,
    label: '　'.repeat(item.depth) + item.name,
  })),
)

const accept = computed(() => props.config?.allowedExtensions.join(',') ?? '')
const limitText = computed(() =>
  props.config ? formatBytes(props.config.maxUploadBytes) : '加载中',
)
const formatText = computed(
  () =>
    props.config?.allowedExtensions.map((ext) => ext.slice(1).toUpperCase()).join(' / ') ??
    'PDF / TXT / Markdown',
)

/** 待提交的行：没传过的，加上上一轮失败的（再点一次就是重试） */
const queue = computed(() =>
  entries.value.filter((entry) => entry.status === 'ready' || entry.status === 'failed'),
)
const totalBytes = computed(() => entries.value.reduce((sum, entry) => sum + entry.file.size, 0))
const submitLabel = computed(() => {
  if (!queue.value.length) return '开始上传'
  return queue.value.every((entry) => entry.status === 'failed')
    ? `重试 ${queue.value.length} 个失败项`
    : '开始上传'
})

/** 每次打开都清空上一次的状态，避免残留的进度条或报错让人误解。 */
watch(visible, (open) => {
  if (!open) return
  entries.value = []
  tagsInput.value = ''
  // 默认落在树上当前选中的分类：用户点开某个分类再上传，多半就是想传到那里
  categoryChoice.value = props.categoryId
  uploading.value = false
  batchTotal.value = 0
  batchDone.value = 0
  uploadRef.value?.clearFiles()
})

function validate(candidate: File): string {
  if (!props.config) return '正在获取服务端上传限制，请稍候重试'
  if (candidate.size === 0) return '文件内容为空，请换一个文件'
  if (candidate.size > props.config.maxUploadBytes) {
    return `文件大小 ${formatBytes(candidate.size)}，超过上传上限 ${limitText.value}`
  }
  const ext = fileExtension(candidate.name)
  if (!props.config.allowedExtensions.includes(ext)) {
    return `不支持 ${ext || '该'} 格式，目前支持 ${formatText.value}`
  }
  return ''
}

/** 选文件是逐个回调的，拖一批进来会连着调用多次。 */
function onChange(uploadFile: UploadFile) {
  const raw = uploadFile.raw
  if (!raw) return
  const reason = validate(raw)
  entries.value.push({
    key: (nextKey += 1),
    file: raw,
    status: reason ? 'invalid' : 'ready',
    percent: 0,
    reason,
  })
}

function removeEntry(target: UploadEntry) {
  entries.value = entries.value.filter((entry) => entry !== target)
}

function clearEntries() {
  entries.value = []
  uploadRef.value?.clearFiles()
}

function parseTags(): string[] {
  const seen = new Set<string>()
  const out: string[] = []
  for (const raw of tagsInput.value.split(/[,，]/)) {
    const tag = raw.trim()
    if (!tag || seen.has(tag)) continue
    seen.add(tag)
    out.push(tag)
  }
  return out
}

async function submit() {
  const batch = queue.value
  if (!batch.length || uploading.value) return
  uploading.value = true
  batchTotal.value = batch.length
  batchDone.value = 0

  const tags = parseTags()
  const succeeded: DocumentItem[] = []

  // 串行提交：每行都要有自己的进度，并发发出去的话进度条会互相打架；
  // 而且后端 worker 是单消费者，同时扔进去也不会让索引更快。
  for (const entry of batch) {
    entry.status = 'uploading'
    entry.percent = 0
    entry.reason = ''
    try {
      const doc = await uploadDocument(entry.file, {
        categoryId: categoryChoice.value,
        tags,
        onProgress: (value) => {
          entry.percent = value
        },
      })
      entry.status = 'done'
      entry.percent = 100
      succeeded.push(doc)
    } catch (err) {
      // 服务端的拒绝理由（超限、格式、分类不存在）直接展示，不再包一层自己的话术
      entry.status = 'failed'
      entry.reason = errorText(err)
    }
    batchDone.value += 1
  }

  uploading.value = false
  emit('uploaded', { succeeded, failed: batchTotal.value - succeeded.length })
  // 全成功才自动关闭；有失败的就留在原地，用户能直接看到是哪几个、为什么
  if (succeeded.length === batchTotal.value) visible.value = false
}
</script>

<template>
  <el-dialog v-model="visible" title="上传文件" width="347px" :close-on-click-modal="!uploading"
    :close-on-press-escape="!uploading" :show-close="!uploading">
    <el-upload ref="uploadRef" class="upload-zone" drag multiple :accept="accept" :auto-upload="false"
      :show-file-list="false" :on-change="onChange" :disabled="uploading">
      <!-- 内联 SVG 而不引入图标库：整个前端只有这一处需要图标 -->
      <svg class="upload-icon" viewBox="0 0 48 48" aria-hidden="true">
        <path d="M24 32V12m0 0l-8 8m8-8l8 8" fill="none" stroke="currentColor" stroke-width="2.5"
          stroke-linecap="round" stroke-linejoin="round" />
        <path d="M8 30v6a4 4 0 004 4h24a4 4 0 004-4v-6" fill="none" stroke="currentColor" stroke-width="2.5"
          stroke-linecap="round" />
      </svg>
      <div class="upload-text">把文件拖到这里，或<em>点击选择</em>（可多选）</div>
      <template #tip>
        <div class="upload-tip">支持 {{ formatText }}，单个文件不超过 {{ limitText }}</div>
      </template>
    </el-upload>

    <div v-if="entries.length" class="batch">
      <div class="batch-head">
        <span>{{ entries.length }} 个文件 · 共 {{ formatBytes(totalBytes) }}</span>
        <el-button v-if="!uploading" link size="small" @click="clearEntries">清空</el-button>
      </div>
      <ul class="batch-list">
        <li v-for="entry in entries" :key="entry.key" class="batch-item" :class="`is-${entry.status}`">
          <div class="batch-line">
            <span class="batch-name" :title="entry.file.name">{{ entry.file.name }}</span>
            <span class="batch-size">{{ formatBytes(entry.file.size) }}</span>
            <el-button v-if="!uploading" link size="small" class="batch-remove" @click="removeEntry(entry)">
              移除
            </el-button>
          </div>
          <el-progress v-if="entry.status === 'uploading'" :percentage="entry.percent" :stroke-width="6"
            :show-text="false" class="batch-bar" />
          <p v-else class="batch-note">{{ entry.reason || STATUS_TEXT[entry.status] }}</p>
        </li>
      </ul>
    </div>

    <el-form label-width="72px" class="upload-form">
      <el-form-item label="分类">
        <el-select v-model="categoryChoice" placeholder="不选择则归为未分类" clearable filterable
          :disabled="uploading" class="category-select">
          <el-option v-for="option in categoryOptions" :key="option.value" :label="option.label"
            :value="option.value" />
        </el-select>
      </el-form-item>
      <el-form-item label="标签">
        <el-input v-model="tagsInput" placeholder="用逗号分隔，例如：发布,复盘" :disabled="uploading" clearable />
      </el-form-item>
      <!-- 混批时最容易被忽略的一点：这两项不是逐个文件填的 -->
      <p v-if="entries.length > 1" class="batch-hint">分类与标签对本批全部文件生效</p>
    </el-form>

    <template #footer>
      <el-button :disabled="uploading" @click="visible = false">取消</el-button>
      <!-- 提交过程中队列会一路清空，这里要排除 uploading，否则按钮会在最后一个文件上传到一半时变灰 -->
      <el-button type="primary" :loading="uploading" :disabled="!uploading && !queue.length" @click="submit">
        {{ uploading ? `上传中 ${batchDone}/${batchTotal}` : submitLabel }}
      </el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
/* 内联 SVG 没有固有尺寸：只给 font-size 的话浏览器会退回默认尺寸（约等于撑满容器宽度），
   图标因此从 48px 变成几百 px 见方，把整个拖拽区顶得很高。必须显式给出宽高。 */
.upload-icon {
  width: 32px;
  height: 32px;
  color: var(--el-text-color-placeholder);
}

/* 拖拽区上下留白等比收窄，否则对话框会显得又窄又高 */
.upload-zone {
  --el-upload-dragger-padding-vertical: 27px;
}

.upload-text {
  color: var(--el-text-color-regular);
}

.upload-text em {
  color: var(--el-color-primary);
  font-style: normal;
}

.upload-tip {
  margin-top: 8px;
  font-size: 12px;
  color: var(--el-text-color-secondary);
}

.batch {
  margin-top: 12px;
}

.batch-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  font-size: 12px;
  color: var(--el-text-color-secondary);
}

/* 拖进来的文件可能很多，列表自己滚，不要顶着对话框一直长高 */
.batch-list {
  margin: 4px 0 0;
  padding: 0;
  list-style: none;
  max-height: 168px;
  overflow-y: auto;
}

.batch-item {
  padding: 6px 0;
  border-bottom: 1px solid var(--el-border-color-lighter);
}

.batch-item:last-child {
  border-bottom: none;
}

.batch-line {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 12px;
}

/* 文件名占满剩余宽度并省略，后面的体积与按钮才不会被长名字挤走 */
.batch-name {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--el-text-color-regular);
}

.batch-size {
  flex: none;
  color: var(--el-text-color-placeholder);
}

.batch-remove {
  flex: none;
}

.batch-note {
  margin: 2px 0 0;
  font-size: 12px;
  color: var(--el-text-color-secondary);
}

.batch-bar {
  margin: 4px 0 0;
}

.is-invalid .batch-note,
.is-failed .batch-note {
  color: var(--el-color-danger);
}

.is-done .batch-note {
  color: var(--el-color-success);
}

.upload-form {
  margin-top: 16px;
}

.category-select {
  width: 100%;
}

.batch-hint {
  margin: 0;
  font-size: 12px;
  line-height: 1.4;
  color: var(--el-text-color-secondary);
}
</style>
