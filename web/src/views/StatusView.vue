<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { errorText, fetchHealth, type HealthResponse } from '@/api/client'

/**
 * M1 的验收页：它同时穿过三层 —— 浏览器 → nginx → api → PostgreSQL。
 * 页面能显示“数据库 ok”，就说明 compose 编排、反代、迁移都通了。
 *
 * 三种状态（加载中 / 成功 / 失败）都在这里显式渲染，后面各页面沿用同一套约定。
 */

type Phase = 'loading' | 'ok' | 'error'

const phase = ref<Phase>('loading')
const health = ref<HealthResponse | null>(null)
const errorMessage = ref('')

async function load() {
  phase.value = 'loading'
  errorMessage.value = ''
  try {
    health.value = await fetchHealth()
    phase.value = 'ok'
  } catch (err) {
    phase.value = 'error'
    // 必须走 errorText：请求失败被收敛成的是 ApiError 这个普通对象，
    // 它不是 Error 的实例，用 instanceof 判断会一路落到 String(err)，
    // 界面上就只剩一句 [object Object]
    errorMessage.value = errorText(err)
  }
}

onMounted(load)
</script>

<template>
  <div class="status-page">
    <el-card shadow="never" class="status-card">
      <template #header>
        <div class="card-header">
          <span>系统状态</span>
          <el-button text :loading="phase === 'loading'" @click="load">重新检测</el-button>
        </div>
      </template>

      <!-- 加载中 -->
      <el-skeleton v-if="phase === 'loading'" :rows="3" animated />

      <!-- 成功 -->
      <el-descriptions v-else-if="phase === 'ok' && health" :column="1" border>
        <el-descriptions-item label="接口服务">
          <el-tag type="success" size="small">ok</el-tag>
        </el-descriptions-item>
        <el-descriptions-item label="数据库">
          <el-tag :type="health.db === 'ok' ? 'success' : 'danger'" size="small">
            {{ health.db }}
          </el-tag>
        </el-descriptions-item>
        <el-descriptions-item label="服务名">{{ health.service }}</el-descriptions-item>
        <el-descriptions-item label="服务端时间">{{ health.time }}</el-descriptions-item>
      </el-descriptions>

      <!-- 失败 -->
      <el-alert v-else type="error" :closable="false" show-icon title="无法连接后端接口">
        <p class="error-detail">{{ errorMessage }}</p>
        <p class="error-hint">
          若 API 容器尚未就绪，请稍候重试；若持续失败，执行
          <code>docker compose logs api</code> 查看原因。
        </p>
      </el-alert>
    </el-card>
  </div>
</template>

<style scoped>
.status-page {
  max-width: 720px;
}

.card-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
}

.error-detail {
  margin: 0 0 8px;
  font-family: var(--el-font-family-mono, monospace);
  font-size: 13px;
}

.error-hint {
  margin: 0;
  font-size: 13px;
}
</style>
