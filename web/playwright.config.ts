/// <reference types="node" />
import { defineConfig } from '@playwright/test'

/**
 * 端到端测试跑在真实的 compose 环境上（nginx → api → PostgreSQL），
 * 不做任何 mock：mock 掉的正是最该被验证的那几层。
 *
 * 用 channel: 'msedge' 复用系统已安装的 Edge，
 * 因此不需要 `npx playwright install` 下载浏览器内核。
 * 换台机器若没有 Edge，把 channel 改成 'chrome' 或删掉该行并执行安装即可。
 */
export default defineConfig({
  testDir: './e2e',
  globalSetup: './e2e/global-setup.ts',
  // 各用例都会改动同一份文档数据，并行执行会互相干扰
  workers: 1,
  fullyParallel: false,
  timeout: 30_000,
  expect: { timeout: 10_000 },
  reporter: [['list']],
  use: {
    baseURL: process.env.E2E_BASE_URL ?? 'http://127.0.0.1:8080',
    channel: 'msedge',
    locale: 'zh-CN',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
})
