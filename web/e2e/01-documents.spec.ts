import { expect, test, type Page } from '@playwright/test'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

/**
 * 文件管理的端到端验收。
 *
 * 断言全部落在用户能看见的东西上（表格里的文字、提示、抽屉内容），
 * 不直接查数据库 —— 数据库里的数据正确但界面没渲染出来，对用户而言依然是坏的。
 *
 * 用例之间存在先后依赖（后一个接着前一个留下的数据），
 * 所以 playwright.config.ts 里把 workers 限制为 1；
 * 文件名带序号前缀，让「先跑哪个」由文件名决定，而不是靠字母序碰巧成立。
 * 本文件从空库开始，因此必须排在 02-categories.spec.ts 之前。
 */
const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..')
const corpus = (name: string) => path.join(repoRoot, 'testdata', 'corpus', name)

const MD = '06_星桥项目_故障复盘_2026-09-21.md'
const TXT = '09_本地部署故障排查_v1.3.txt'

/** 走一遍上传对话框：选文件、填标签、提交。 */
async function uploadVia(page: Page, filePath: string, tags?: string) {
  await page.getByRole('button', { name: '上传文件' }).click()
  const dialog = page.locator('.el-dialog')
  await expect(dialog).toBeVisible()
  await dialog.locator('input[type="file"]').setInputFiles(filePath)
  if (tags) {
    await dialog.getByPlaceholder('用逗号分隔，例如：发布,复盘').fill(tags)
  }
  await dialog.getByRole('button', { name: '开始上传' }).click()
}

/** 表格里按文件名定位一行。 */
function rowOf(page: Page, name: string) {
  return page.locator('.el-table__row').filter({ hasText: name })
}

test('根路径重定向到文件管理，且初始是空状态', async ({ page }) => {
  await page.goto('/')
  await expect(page).toHaveURL(/\/documents$/)
  await expect(page.getByText('文件管理与知识检索平台').first()).toBeVisible()
  await expect(page.getByText('还没有文件，点右上角的上传按钮添加')).toBeVisible()
})

test('上传 Markdown：进入列表、标签与索引状态一并展示', async ({ page }) => {
  await page.goto('/documents')
  await uploadVia(page, corpus(MD), '故障,复盘')

  await expect(page.getByText(`已上传「${MD}」`)).toBeVisible()

  const row = rowOf(page, MD)
  await expect(row).toBeVisible()
  await expect(row).toContainText('故障')
  await expect(row).toContainText('复盘')
  await expect(row).toContainText('未分类')
  // 先确认没有被误标成「仅存储」：那是 PDF 的正常状态，出现在 Markdown 上就是错的
  await expect(row).not.toContainText('仅存储')
  // 索引在后台异步做，列表会自己把状态推进到终态 ——
  // 这一步不需要手动刷新页面，正好验证了前端的轮询确实在工作。
  await expect(row).toContainText('已索引', { timeout: 30_000 })
})

test('上传 TXT 与 PDF：PDF 标记为仅存储，不参与正文索引', async ({ page }) => {
  await page.goto('/documents')
  await uploadVia(page, corpus(TXT))
  await expect(page.getByText(`已上传「${TXT}」`)).toBeVisible()
  await expect(rowOf(page, TXT)).toContainText('已索引', { timeout: 30_000 })

  const pdf = '10_文档分类与归档规范_v1.0.pdf'
  await uploadVia(page, corpus(pdf))
  await expect(page.getByText(`已上传「${pdf}」`)).toBeVisible()
  // PDF 不抽正文，是正常状态而非失败，界面必须把两者区分开
  await expect(rowOf(page, pdf)).toContainText('仅存储')
  // 它也不该被推进到「已索引」：等一会儿再确认一次，避免只是还没来得及变
  await page.waitForTimeout(2000)
  await expect(rowOf(page, pdf)).toContainText('仅存储')
})

test('拒绝不支持的格式，并说明支持哪些', async ({ page }) => {
  await page.goto('/documents')
  await page.getByRole('button', { name: '上传文件' }).click()
  const dialog = page.locator('.el-dialog')
  await dialog.locator('input[type="file"]').setInputFiles({
    name: '安装包.exe',
    mimeType: 'application/octet-stream',
    buffer: Buffer.from('MZ'),
  })

  await expect(dialog.getByText(/不支持 \.exe 格式/)).toBeVisible()
  // 预检不过时提交按钮应当不可用，避免发出一次注定失败的请求
  await expect(dialog.getByRole('button', { name: '开始上传' })).toBeDisabled()
  await dialog.getByRole('button', { name: '取消' }).click()
  await expect(rowOf(page, '安装包.exe')).toHaveCount(0)
})

test('按文件名搜索，无结果时给出区分于空库的提示', async ({ page }) => {
  await page.goto('/documents')
  const search = page.getByPlaceholder('按文件名搜索')

  await search.fill('故障复盘')
  await expect(rowOf(page, MD)).toBeVisible()
  await expect(rowOf(page, TXT)).toHaveCount(0)

  await search.fill('这个词一定搜不到')
  await expect(page.getByText('没有匹配的文件')).toBeVisible()

  await search.fill('')
  await expect(rowOf(page, MD)).toBeVisible()
})

test('点击行打开详情抽屉，信息与列表一致', async ({ page }) => {
  await page.goto('/documents')
  await rowOf(page, MD).click()

  const drawer = page.locator('.el-drawer')
  await expect(drawer).toBeVisible()
  await expect(drawer).toContainText(MD)
  await expect(drawer).toContainText('3 KiB')
  await expect(drawer).toContainText('故障')
  await expect(drawer).toContainText('未分类')
  await expect(drawer).toContainText('已索引', { timeout: 30_000 })
})

test('抽屉里改名与改标签后，列表同步更新', async ({ page }) => {
  await page.goto('/documents')
  await rowOf(page, TXT).click()

  const drawer = page.locator('.el-drawer')
  await expect(drawer).toBeVisible()
  await drawer.getByRole('button', { name: '修改信息' }).click()
  await drawer.locator('input[maxlength="200"]').fill('本地部署故障排查-已改名.txt')
  await drawer.getByPlaceholder('用逗号分隔').fill('部署,排障')
  await drawer.getByRole('button', { name: '保存' }).click()
  await expect(page.getByText('已保存')).toBeVisible()

  await page.keyboard.press('Escape')
  await expect(rowOf(page, '本地部署故障排查-已改名.txt')).toContainText('部署')
})

test('下载：文件名与原文件后缀一致，内容非空', async ({ page }) => {
  await page.goto('/documents')
  const downloadPromise = page.waitForEvent('download')
  await rowOf(page, MD).getByRole('button', { name: '下载' }).click()
  const download = await downloadPromise

  expect(download.suggestedFilename()).toBe(MD)
  const stream = await download.createReadStream()
  const chunks: Buffer[] = []
  for await (const chunk of stream) chunks.push(chunk as Buffer)
  expect(Buffer.concat(chunks).length).toBeGreaterThan(0)
})

test('归档：从“使用中”消失，进入“已归档”，再恢复', async ({ page }) => {
  await page.goto('/documents')
  await rowOf(page, MD).getByRole('button', { name: '归档' }).click()
  await expect(page.getByText(`已归档「${MD}」`)).toBeVisible()
  // 点行内按钮不应顺带把详情抽屉打开
  await expect(page.locator('.el-drawer')).toBeHidden()
  await expect(rowOf(page, MD)).toHaveCount(0)

  await page.getByRole('tab', { name: '已归档' }).click()
  const archived = rowOf(page, MD)
  await expect(archived).toBeVisible()
  // 归档状态由所在的标签页表达，行内只需体现可执行的动作已换成“恢复”
  await expect(archived.getByRole('button', { name: '恢复' })).toBeVisible()
  await expect(archived.getByRole('button', { name: '归档' })).toHaveCount(0)

  await archived.getByRole('button', { name: '恢复' }).click()
  await expect(page.getByText(`已恢复「${MD}」`)).toBeVisible()
  await expect(page.getByText('还没有归档的文件')).toBeVisible()

  await page.getByRole('tab', { name: '使用中' }).click()
  await expect(rowOf(page, MD)).toBeVisible()
})

test('刷新页面后列表仍在（数据来自数据库而非内存）', async ({ page }) => {
  await page.goto('/documents')
  await expect(rowOf(page, MD)).toBeVisible()
  await page.reload()
  await expect(rowOf(page, MD)).toBeVisible()
  await expect(rowOf(page, '本地部署故障排查-已改名.txt')).toBeVisible()
})

/**
 * 放在最后：它要等索引走到失败的终态，是这里最慢的一个用例。
 *
 * 默认 3 次尝试之间隔 15s / 30s 的退避，所以从上传到「索引失败」要 45 秒上下。
 * 这个等待本身就是被测对象 —— 验收要求失败可重试、且失败不影响原文件，
 * 那就得真的等到那个状态出现，而不是靠改库把它提前按下去。
 */
test('索引失败：状态与原因可见、可重试，且不影响下载', async ({ page }) => {
  test.setTimeout(150_000)
  await page.goto('/documents')

  // 只有空白字符的文本抽不出任何正文，是构造「索引失败」最省事也最真实的输入
  await page.getByRole('button', { name: '上传文件' }).click()
  const dialog = page.locator('.el-dialog')
  await dialog.locator('input[type="file"]').setInputFiles({
    name: '空白.txt',
    mimeType: 'text/plain',
    buffer: Buffer.from('   \n\t\n'),
  })
  await dialog.getByRole('button', { name: '开始上传' }).click()
  await expect(page.getByText('已上传「空白.txt」')).toBeVisible()

  const row = rowOf(page, '空白.txt')
  await expect(row).toContainText('索引失败', { timeout: 90_000 })

  await row.click()
  const drawer = page.locator('.el-drawer')
  await expect(drawer).toBeVisible()
  await expect(drawer).toContainText('索引失败')
  // 失败原因必须看得见，否则用户不知道该重试还是该换个文件
  await expect(drawer.locator('.el-alert')).toContainText('正文')

  // 失败只影响索引，原文件在上传时就已落盘，下载必须照常
  const downloadPromise = page.waitForEvent('download')
  await drawer.getByRole('button', { name: '下载' }).click()
  const download = await downloadPromise
  expect(download.suggestedFilename()).toBe('空白.txt')
  const stream = await download.createReadStream()
  const chunks: Buffer[] = []
  for await (const chunk of stream) chunks.push(chunk as Buffer)
  expect(Buffer.concat(chunks).toString('utf8')).toBe('   \n\t\n')

  // 重试入口可用：点下去要真的重新排队，而不是只弹一句提示
  await drawer.getByRole('button', { name: '重新索引' }).click()
  await expect(page.getByText('已提交重新索引')).toBeVisible()
  await expect(drawer.getByText(/待索引|索引中/)).toBeVisible()
})
