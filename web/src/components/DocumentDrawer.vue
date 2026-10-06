<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { errorText } from '@/api/client'
import {
  archiveDocument,
  fetchDocumentBlob,
  getDocument,
  openBlobInNewTab,
  restoreDocument,
  saveBlob,
  updateDocument,
  type DocumentItem,
} from '@/api/documents'
import { flattenCategories, type CategoryNode } from '@/api/categories'
import { formatBytes, formatDateTime, fileExtension } from '@/utils/format'
import IndexStatusTag from '@/components/IndexStatusTag.vue'

/**
 * 文件详情抽屉。
 *
 * 打开时按 id 重新拉一次详情，而不是直接用列表里的那条记录：
 * 列表可能是几分钟前取的，索引状态在此期间大概率已经变了。
 */
const props = defineProps<{
  modelValue: boolean
  documentId: string | null
  categories: CategoryNode[]
}>()

const emit = defineEmits<{
  'update:modelValue': [value: boolean]
  /** 文档被改动（归档/恢复/改名），父组件据此刷新列表 */
  changed: [doc: DocumentItem]
}>()

const visible = computed({
  get: () => props.modelValue,
  set: (value) => emit('update:modelValue', value),
})

const doc = ref<DocumentItem | null>(null)
const loading = ref(false)
const loadError = ref('')
const busy = ref(false)

const editing = ref(false)
const nameDraft = ref('')
const tagsDraft = ref('')
const categoryDraft = ref<number | null>(null)

// 用全角空格做缩进体现层级，与上传对话框里的分类下拉一致
const categoryOptions = computed(() =>
  flattenCategories(props.categories).map((item) => ({
    value: item.id,
    label: '　'.repeat(item.depth) + item.name,
  })),
)

const isPdf = computed(() => doc.value?.contentType.startsWith('application/pdf') ?? false)
const dirty = computed(
  () =>
    doc.value !== null &&
    (nameDraft.value.trim() !== doc.value.name ||
      tagsDraft.value.trim() !== doc.value.tags.join(',') ||
      categoryDraft.value !== doc.value.categoryId),
)

async function load() {
  if (!props.documentId) return
  loading.value = true
  loadError.value = ''
  try {
    const fresh = await getDocument(props.documentId)
    doc.value = fresh
    resetDraft(fresh)
  } catch (err) {
    doc.value = null
    loadError.value = errorText(err)
  } finally {
    loading.value = false
  }
}

function resetDraft(target: DocumentItem) {
  nameDraft.value = target.name
  tagsDraft.value = target.tags.join(',')
  categoryDraft.value = target.categoryId
  editing.value = false
}

watch(
  () => [props.modelValue, props.documentId],
  ([open]) => {
    if (open) void load()
  },
  { immediate: true },
)

/** 统一的动作包装：置忙、报错、把最新文档回传给父组件。 */
async function run(action: () => Promise<DocumentItem>, successText: string) {
  busy.value = true
  try {
    const updated = await action()
    doc.value = updated
    resetDraft(updated)
    emit('changed', updated)
    ElMessage.success(successText)
  } catch (err) {
    ElMessage.error(errorText(err))
  } finally {
    busy.value = false
  }
}

function parseTags(raw: string): string[] {
  const seen = new Set<string>()
  const out: string[] = []
  for (const piece of raw.split(/[,，]/)) {
    const tag = piece.trim()
    if (!tag || seen.has(tag)) continue
    seen.add(tag)
    out.push(tag)
  }
  return out
}

async function save() {
  if (!doc.value) return
  const name = nameDraft.value.trim()
  if (!name) {
    ElMessage.error('文件名不能为空')
    return
  }
  // 分类显式传值（含 null）：这里表达的是「改成下拉里选的这个」，
  // 选为空就是要把它移出所有分类，与「不改动」是两回事
  await run(
    () =>
      updateDocument(doc.value!.id, {
        name,
        tags: parseTags(tagsDraft.value),
        categoryId: categoryDraft.value,
      }),
    '已保存',
  )
}

function toggleArchive() {
  if (!doc.value) return
  const target = doc.value
  if (target.archived) {
    void run(() => restoreDocument(target.id), '已恢复')
  } else {
    void run(() => archiveDocument(target.id), '已归档')
  }
}

/** 下载与预览都先把内容取成 Blob，失败时才能给出可读原因而不是跳到一片 JSON。 */
async function withBlob(use: (blob: Blob) => void, failText: string) {
  if (!doc.value) return
  busy.value = true
  try {
    const blob = await fetchDocumentBlob(doc.value.id, doc.value.contentType.startsWith('application/pdf'))
    use(blob)
  } catch (err) {
    // 记录在库但磁盘上文件缺失时会走到这里，原因要与索引失败区分开
    ElMessage.error(`${failText}：${errorText(err)}`)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <el-drawer v-model="visible" title="文件详情" size="460px">
    <el-skeleton v-if="loading" :rows="6" animated />

    <el-alert v-else-if="loadError" type="error" :closable="false" show-icon :title="loadError">
      <el-button text type="primary" @click="load">重试</el-button>
    </el-alert>

    <template v-else-if="doc">
      <el-descriptions :column="1" border>
        <el-descriptions-item label="文件名">
          <template v-if="editing">
            <el-input v-model="nameDraft" maxlength="200" show-word-limit />
          </template>
          <span v-else class="doc-name">{{ doc.name }}</span>
        </el-descriptions-item>

        <el-descriptions-item label="标签">
          <template v-if="editing">
            <el-input v-model="tagsDraft" placeholder="用逗号分隔" />
          </template>
          <template v-else-if="doc.tags.length">
            <el-tag v-for="tag in doc.tags" :key="tag" size="small" class="doc-tag">{{ tag }}</el-tag>
          </template>
          <span v-else class="doc-empty">未设置</span>
        </el-descriptions-item>

        <el-descriptions-item label="分类">
          <template v-if="editing">
            <el-select v-model="categoryDraft" placeholder="不选择则归为未分类" clearable filterable
              class="category-select">
              <el-option v-for="option in categoryOptions" :key="option.value" :label="option.label"
                :value="option.value" />
            </el-select>
          </template>
          <span v-else-if="doc.categoryName">{{ doc.categoryName }}</span>
          <span v-else class="doc-empty">未分类</span>
        </el-descriptions-item>

        <el-descriptions-item label="格式">
          {{ fileExtension(doc.name).replace('.', '').toUpperCase() || '未知' }}
        </el-descriptions-item>

        <el-descriptions-item label="大小">{{ formatBytes(doc.sizeBytes) }}</el-descriptions-item>

        <el-descriptions-item label="索引状态">
          <IndexStatusTag :status="doc.indexStatus" />
          <div v-if="doc.indexError" class="doc-index-error">{{ doc.indexError }}</div>
        </el-descriptions-item>

        <el-descriptions-item label="状态">
          <el-tag v-if="doc.archived" type="info" size="small">已归档</el-tag>
          <el-tag v-else type="success" size="small">使用中</el-tag>
        </el-descriptions-item>

        <el-descriptions-item label="上传时间">{{ formatDateTime(doc.createdAt) }}</el-descriptions-item>
        <el-descriptions-item label="更新时间">{{ formatDateTime(doc.updatedAt) }}</el-descriptions-item>
      </el-descriptions>

      <div class="drawer-actions">
        <el-button :disabled="busy" @click="withBlob((b) => saveBlob(b, doc!.name), '下载失败')">
          下载
        </el-button>
        <el-button v-if="isPdf" :disabled="busy" @click="withBlob(openBlobInNewTab, '预览失败')">
          预览
        </el-button>
        <el-button v-if="!editing" :disabled="busy" @click="editing = true">修改信息</el-button>
        <template v-else>
          <el-button type="primary" :disabled="!dirty || busy" @click="save">保存</el-button>
          <el-button :disabled="busy" @click="resetDraft(doc)">取消</el-button>
        </template>
        <el-button :type="doc.archived ? 'success' : 'warning'" :loading="busy" @click="toggleArchive">
          {{ doc.archived ? '恢复' : '归档' }}
        </el-button>
      </div>
    </template>
  </el-drawer>
</template>

<style scoped>
.doc-name {
  word-break: break-all;
}

.doc-tag {
  margin-right: 6px;
}

.doc-empty {
  color: var(--el-text-color-secondary);
}

.category-select {
  width: 100%;
}

.doc-index-error {
  margin-top: 6px;
  font-size: 12px;
  color: var(--el-color-danger);
  word-break: break-all;
}

.drawer-actions {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin-top: 16px;
}
</style>
