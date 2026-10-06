/** 把字节数格式化成可读写法。列表里满屏的 3092 远不如 3.0 KiB 直观。 */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '—'
  const units = ['B', 'KiB', 'MiB', 'GiB']
  let value = bytes
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit += 1
  }
  // 字节数不带小数；其余保留一位，但整数就不显示 .0
  const text = unit === 0 ? String(value) : value.toFixed(1).replace(/\.0$/, '')
  return `${text} ${units[unit]}`
}

/**
 * 把后端的 RFC3339 时间转成本地时区的 YYYY-MM-DD HH:mm。
 * 后端一律以 UTC 存储，由浏览器负责换算，服务端不假设用户所在时区。
 */
export function formatDateTime(iso: string): string {
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) return '—'
  const pad = (n: number) => String(n).padStart(2, '0')
  return (
    `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ` +
    `${pad(date.getHours())}:${pad(date.getMinutes())}`
  )
}

/** 从文件名里取扩展名（小写，含点）。用于列表上的格式标签。 */
export function fileExtension(name: string): string {
  const dot = name.lastIndexOf('.')
  return dot > 0 ? name.slice(dot).toLowerCase() : ''
}
