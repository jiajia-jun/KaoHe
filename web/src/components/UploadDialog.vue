<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import type { UploadFile, UploadInstance, UploadRawFile } from 'element-plus'
import { errorText } from '@/api/client'
import { uploadDocument, type DocumentItem, type ServerConfig } from '@/api/documents'
import { formatBytes, fileExtension } from '@/utils/format'

/**
 * 上传对话框：拖拽或点选一个文件，校验通过后再提交。
 *
 * 体积与格式的判定标准来自服务端的 /config，前端不另写一份常量，
 * 否则改了环境变量就会出现“界面说可以、服务端说不行”。
 * 这里的校验只是提前拦下明显不合格的文件，服务端仍会独立校验一次。
 */
const props = defineProps<{
  modelValue: boolean
  config: ServerConfig | null
  /** M3 接入分类树后由父组件传入当前选中的分类，作为新建文件的默认归属 */
  categoryId?: number | null
}>()

const emit = defineEmits<{
  'update:modelValue': [value: boolean]
  uploaded: [doc: DocumentItem]
}>()

const visible = computed({
  get: () => props.modelValue,
  set: (value) => emit('update:modelValue', value),
})

const uploadRef = ref<UploadInstance>()
const file = ref<File | null>(null)
const tagsInput = ref('')
const percent = ref(0)
const uploading = ref(false)
const problem = ref('')

const accept = computed(() => props.config?.allowedExtensions.join(',') ?? '')
const limitText = computed(() =>
  props.config ? formatBytes(props.config.maxUploadBytes) : '加载中',
)
const formatText = computed(
  () =>
    props.config?.allowedExtensions.map((ext) => ext.slice(1).toUpperCase()).join(' / ') ??
    'PDF / TXT / Markdown',
)

/** 每次打开都清空上一次的状态，避免残留的进度条或报错让人误解。 */
watch(visible, (open) => {
  if (!open) return
  file.value = null
  tagsInput.value = ''
  percent.value = 0
  problem.value = ''
  uploading.value = false
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

function onChange(uploadFile: UploadFile) {
  const raw = uploadFile.raw
  if (!raw) return
  problem.value = validate(raw)
  file.value = problem.value ? null : raw
}

/** limit=1 时再次拖入会触发这里，直接换成新文件而不是报错。 */
function onExceed(files: File[]) {
  const next = files[0] as UploadRawFile | undefined
  if (!next) return
  uploadRef.value?.clearFiles()
  uploadRef.value?.handleStart(next)
  problem.value = validate(next)
  file.value = problem.value ? null : next
}

function onRemove() {
  file.value = null
  problem.value = ''
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
  if (!file.value || problem.value) return
  uploading.value = true
  percent.value = 0
  try {
    const doc = await uploadDocument(file.value, {
      categoryId: props.categoryId ?? null,
      tags: parseTags(),
      onProgress: (value) => {
        percent.value = value
      },
    })
    emit('uploaded', doc)
    visible.value = false
  } catch (err) {
    // 服务端的拒绝理由（超限、格式、分类不存在）直接展示，不再包一层自己的话术
    problem.value = errorText(err)
  } finally {
    uploading.value = false
  }
}
</script>

<template>
  <el-dialog v-model="visible" title="上传文件" width="520px" :close-on-click-modal="!uploading"
    :close-on-press-escape="!uploading" :show-close="!uploading">
    <el-upload ref="uploadRef" drag :accept="accept" :auto-upload="false" :limit="1" :show-file-list="true"
      :on-change="onChange" :on-exceed="onExceed" :on-remove="onRemove" :disabled="uploading">
      <!-- 内联 SVG 而不引入图标库：整个前端只有这一处需要图标 -->
      <svg class="upload-icon" viewBox="0 0 48 48" aria-hidden="true">
        <path d="M24 32V12m0 0l-8 8m8-8l8 8" fill="none" stroke="currentColor" stroke-width="2.5"
          stroke-linecap="round" stroke-linejoin="round" />
        <path d="M8 30v6a4 4 0 004 4h24a4 4 0 004-4v-6" fill="none" stroke="currentColor" stroke-width="2.5"
          stroke-linecap="round" />
      </svg>
      <div class="upload-text">把文件拖到这里，或<em>点击选择</em></div>
      <template #tip>
        <div class="upload-tip">支持 {{ formatText }}，单个文件不超过 {{ limitText }}</div>
      </template>
    </el-upload>

    <el-form label-width="72px" class="upload-form">
      <el-form-item label="标签">
        <el-input v-model="tagsInput" placeholder="用逗号分隔，例如：发布,复盘" :disabled="uploading" clearable />
      </el-form-item>
    </el-form>

    <el-progress v-if="uploading" :percentage="percent" :stroke-width="10" class="upload-progress" />

    <el-alert v-if="problem" type="error" :closable="false" show-icon class="upload-alert" :title="problem" />

    <template #footer>
      <el-button :disabled="uploading" @click="visible = false">取消</el-button>
      <el-button type="primary" :loading="uploading" :disabled="!file || !!problem" @click="submit">
        {{ uploading ? '上传中' : '开始上传' }}
      </el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.upload-icon {
  font-size: 48px;
  color: var(--el-text-color-placeholder);
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

.upload-form {
  margin-top: 16px;
}

.upload-progress {
  margin-top: 4px;
}

.upload-alert {
  margin-top: 12px;
}
</style>
