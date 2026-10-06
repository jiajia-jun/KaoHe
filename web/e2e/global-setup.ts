import { execFileSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import path from 'node:path'

/**
 * 每个测试轮次开始前把数据清空，让断言建立在确定的状态上。
 *
 * 这些测试跑在本机的 compose 环境里，直接借 compose 进容器操作数据库，
 * 比在测试里另起一套连接配置更简单，也不会与 .env 里的取值脱节。
 */
const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..')

function compose(args: string[]): string {
  return execFileSync('docker', ['compose', ...args], {
    cwd: repoRoot,
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
  }).trim()
}

export default function globalSetup() {
  compose([
    'exec',
    '-T',
    'db',
    'psql',
    '-U',
    'kaohe',
    '-d',
    'kaohe',
    '-c',
    'TRUNCATE documents, index_jobs, document_chunks RESTART IDENTITY CASCADE;',
  ])
  // 数据库清空了，磁盘上的原文件也必须一起清掉，
  // 否则残留文件不会影响断言，却会掩盖“删除是否真的落盘”这类问题
  compose(['exec', '-T', 'api', 'sh', '-c', 'rm -rf /data/uploads/*'])

  // 顺带确认服务是活的：与其让第一个用例报一堆难懂的定位失败，不如在这里直接说清楚
  try {
    const health = execFileSync(
      'curl',
      ['-fsS', `${process.env.E2E_BASE_URL ?? 'http://127.0.0.1:8080'}/api/v1/healthz`],
      { encoding: 'utf8' },
    )
    console.log(`[e2e] 服务健康检查通过：${health}`)
  } catch {
    throw new Error(
      '无法访问服务，请先执行 docker compose up -d 并确认 web 暴露的端口与 E2E_BASE_URL 一致',
    )
  }
}
