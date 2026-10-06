<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'

/**
 * 应用外壳：顶部标题 + 导航，下方是路由内容区。
 *
 * 分类树没有放在这里，而是由文件管理页自己持有 ——
 * 它是那一页的筛选条件，独立于其他页面，放在外壳里反而要额外处理它的可见性。
 */
const route = useRoute()
const activeMenu = computed(() => String(route.name ?? 'documents'))
</script>

<template>
  <el-container class="app-shell">
    <el-header class="app-header">
      <div class="brand">
        <span class="app-title">文件管理与知识检索平台</span>
        <span class="app-subtitle">上传 · 整理 · 检索 · 归档</span>
      </div>
      <el-menu :default-active="activeMenu" mode="horizontal" class="app-nav" router :ellipsis="false">
        <el-menu-item index="documents" :route="{ name: 'documents' }">文件管理</el-menu-item>
        <el-menu-item index="status" :route="{ name: 'status' }">系统状态</el-menu-item>
      </el-menu>
    </el-header>

    <el-main class="app-main">
      <router-view />
    </el-main>
  </el-container>
</template>

<style scoped>
.app-shell {
  min-height: 100vh;
}

.app-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 24px;
  border-bottom: 1px solid var(--el-border-color-light);
  background: var(--el-bg-color);
}

.brand {
  display: flex;
  align-items: baseline;
  gap: 12px;
}

.app-title {
  font-size: 18px;
  font-weight: 600;
  white-space: nowrap;
}

.app-subtitle {
  font-size: 13px;
  color: var(--el-text-color-secondary);
}

/* 顶部导航去掉默认的下边框，避免与 header 的分隔线叠成双线 */
.app-nav {
  border-bottom: none;
  flex: 1;
}

.app-main {
  background: var(--el-fill-color-lighter);
}
</style>
