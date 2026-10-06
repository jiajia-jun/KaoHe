import { createRouter, createWebHistory } from 'vue-router'

/**
 * 首页直接进文件管理，系统状态挪到独立入口。
 * 检索页在 M5 接入，届时挂在「知识检索」下。
 */
const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', redirect: '/documents' },
    {
      path: '/documents',
      name: 'documents',
      component: () => import('@/views/DocumentsView.vue'),
      meta: { title: '文件管理' },
    },
    {
      path: '/status',
      name: 'status',
      component: () => import('@/views/StatusView.vue'),
      meta: { title: '系统状态' },
    },
    // 未知路径统一回到文件管理，避免出现一片空白
    { path: '/:pathMatch(.*)*', redirect: '/documents' },
  ],
})

export default router
