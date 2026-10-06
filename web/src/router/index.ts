import { createRouter, createWebHistory } from 'vue-router'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    {
      path: '/',
      name: 'status',
      component: () => import('@/views/StatusView.vue'),
      meta: { title: '系统状态' },
    },
  ],
})

export default router
