import axios from 'axios'

/**
 * 所有请求都走同源路径 /api，由 nginx（生产）或 Vite proxy（开发）转发到 api 容器。
 * 前端因此不持有任何后端地址配置，换部署环境不需要重新构建镜像。
 */
export const http = axios.create({
  baseURL: '/api/v1',
  timeout: 30_000,
})

/**
 * 后端约定的错误结构：{ code, message }。
 * code 供程序分支判断，message 直接展示给用户，前端不解析中文文案。
 */
export interface ApiError {
  code: string
  message: string
  /** HTTP 状态码；网络层就失败时为 0 */
  status: number
}

export function isApiError(value: unknown): value is ApiError {
  return (
    typeof value === 'object' &&
    value !== null &&
    typeof (value as ApiError).message === 'string' &&
    typeof (value as ApiError).status === 'number'
  )
}

/** 把任意异常收敛成可直接显示的文案，供 catch 块统一使用。 */
export function errorText(err: unknown): string {
  if (isApiError(err)) return err.message
  if (err instanceof Error) return err.message
  return String(err)
}

/**
 * 把 axios 的异常翻译成 ApiError。
 *
 * 单独处理 Blob 响应：下载接口带 responseType: 'blob'，
 * 出错时后端返回的 JSON 也会被包成 Blob，不读出来就看不懂失败原因。
 */
async function normalizeError(error: unknown): Promise<ApiError> {
  if (!axios.isAxiosError(error)) {
    return {
      code: 'error',
      message: error instanceof Error ? error.message : String(error),
      status: 0,
    }
  }

  const status = error.response?.status ?? 0
  let payload: unknown = error.response?.data

  if (payload instanceof Blob) {
    try {
      payload = JSON.parse(await payload.text())
    } catch {
      payload = undefined
    }
  }

  if (
    payload !== null &&
    typeof payload === 'object' &&
    typeof (payload as { message?: unknown }).message === 'string'
  ) {
    const body = payload as { code?: unknown; message: string }
    return {
      code: typeof body.code === 'string' ? body.code : 'error',
      message: body.message,
      status,
    }
  }

  if (error.code === 'ECONNABORTED') {
    return { code: 'timeout', message: '请求超时，请稍后重试', status }
  }
  if (status === 0) {
    return {
      code: 'network_error',
      message: '无法连接服务器，请确认服务已启动',
      status,
    }
  }
  return { code: 'error', message: `请求失败（HTTP ${status}）`, status }
}

// 让所有调用方拿到的失败原因都是 ApiError，不必各自判断 axios 的错误形状
http.interceptors.response.use(
  (response) => response,
  async (error: unknown) => {
    throw await normalizeError(error)
  },
)

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
