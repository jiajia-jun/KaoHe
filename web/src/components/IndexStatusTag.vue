<script setup lang="ts">
import { computed } from 'vue'
import type { IndexStatus } from '@/api/documents'

/**
 * 索引状态的统一展示。列表、详情抽屉、后续的检索页都用它，
 * 保证同一个状态在各处颜色与文案完全一致。
 */
const props = defineProps<{ status: IndexStatus }>()

const MAP: Record<IndexStatus, { label: string; type: 'success' | 'info' | 'warning' | 'danger'; hint: string }> = {
  pending: { label: '待索引', type: 'info', hint: '已排队，等待后台处理' },
  processing: { label: '索引中', type: 'warning', hint: '正在抽取正文并生成向量' },
  ready: { label: '已索引', type: 'success', hint: '可被关键词与语义检索命中' },
  failed: { label: '索引失败', type: 'danger', hint: '抽取或向量化失败，可查看失败原因' },
  not_supported: { label: '仅存储', type: 'info', hint: '该格式不抽取正文，只提供保存、下载与预览' },
}

const view = computed(() => MAP[props.status] ?? { label: props.status, type: 'info' as const, hint: '' })
</script>

<template>
  <el-tooltip :content="view.hint" placement="top" :disabled="!view.hint">
    <el-tag :type="view.type" size="small" :effect="status === 'not_supported' ? 'plain' : 'light'">
      {{ view.label }}
    </el-tag>
  </el-tooltip>
</template>
