import axios from 'axios'

/**
 * 所有请求都走同源路径 /api，由 nginx（生产）或 Vite proxy（开发）转发到 api 容器。
 * 前端因此不持有任何后端地址配置，换部署环境不需要重新构建镜像。
 */
export const http = axios.create({
  baseURL: '/api/v1',
  timeout: 30_000,
})

export interface HealthResponse {
  status: 'ok' | 'degraded'
  service: string
  time: string
  db: 'ok' | 'down'
}

export async function fetchHealth(): Promise<HealthResponse> {
  const { data } = await http.get<HealthResponse>('/healthz')
  return data
}
