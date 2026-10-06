<script setup lang="ts">
// 应用外壳。M2 起左侧会挂上分类树、顶部挂上工具栏，
// 目前只保留标题与内容区，避免提前写死后续会被推翻的布局。
</script>

<template>
  <el-container class="app-shell">
    <el-header class="app-header">
      <span class="app-title">文件管理与知识检索平台</span>
      <span class="app-subtitle">上传 · 整理 · 检索 · 归档</span>
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
  align-items: baseline;
  gap: 12px;
  border-bottom: 1px solid var(--el-border-color-light);
  background: var(--el-bg-color);
}

.app-title {
  font-size: 18px;
  font-weight: 600;
}

.app-subtitle {
  font-size: 13px;
  color: var(--el-text-color-secondary);
}

.app-main {
  background: var(--el-fill-color-lighter);
}
</style>
