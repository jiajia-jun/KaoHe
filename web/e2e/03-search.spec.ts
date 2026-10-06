import { expect, test, type Page } from '@playwright/test'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

/**
 * 知识检索的端到端验收，对应验收条件里的「关键词搜索」与「语义检索」。
 *
 * 这里不去复现 acceptance_api.py 里的排序断言（那是接口层的活），
 * 只验界面这一层：结果有没有渲染出来、命中片段和来源文件是否可见、
 * 三种状态（加载 / 有结果 / 没结果）有没有被区分开、切换检索方式会不会张冠李戴。
 *
 * 本文件接在 02-categories.spec.ts 之后运行，库里有前面留下的文件。
 * 需要哪份语料就现传哪份，缺什么补什么，不假设前面用例一定传过。
 */
const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..')
const corpus = (name: string) => path.join(repoRoot, 'testdata', 'corpus', name)

/** 文件名命中「发布检查清单」的那份 */
const NAMED = '05_星桥项目_发布检查清单_2026-09-18.txt'
/** 正文里提到过「发布检查清单」、文件名却没有的那份 */
const IN_BODY = '07_星桥项目_用户访谈纪要_2026-09-12.txt'
/** 语义检索的提问对象 */
const CODE_RULE = '01_代码提交规范_v2.1.md'

/** 表格里按文件名定位一行。 */
function rowOf(page: Page, name: string) {
  return page.locator('.el-table__row').filter({ hasText: name })
}

/** 走一遍上传对话框。 */
async function uploadVia(page: Page, filePath: string) {
  await page.getByRole('button', { name: '上传文件' }).click()
  const dialog = page.locator('.el-dialog')
  await expect(dialog).toBeVisible()
  await dialog.locator('input[type="file"]').setInputFiles(filePath)
  await dialog.getByRole('button', { name: '开始上传' }).click()
  await expect(page.getByText(/已上传「/)).toBeVisible()
}

/** 确保这几份语料在库里并且索引完成；已经在的就不重复上传。 */
async function ensureIndexed(page: Page, names: string[]) {
  await page.goto('/documents')
  for (const name of names) {
    if ((await rowOf(page, name).count()) > 0) continue
    await uploadVia(page, corpus(name))
    await expect(rowOf(page, name)).toContainText('已索引', { timeout: 90_000 })
  }
  // 已存在的那些也要等到索引完成：语义检索只看得见有向量的文件
  for (const name of names) {
    await expect(rowOf(page, name).first()).toContainText('已索引', { timeout: 90_000 })
  }
}

/** 结果卡片。按文件名定位，同名文件（前面用例可能传过两次）取第一张。 */
function hitOf(page: Page, name: string) {
  return page.locator('.hit').filter({ hasText: name }).first()
}

/** 当前是哪个标签页的输入框。placeholder 随模式变化，用结构定位更稳。 */
function queryBox(page: Page) {
  return page.locator('.query-row input')
}

async function search(page: Page, query: string) {
  await queryBox(page).fill(query)
  await page.getByRole('button', { name: '检索' }).click()
}

test('检索页初始是「还没搜过」，不是「没有结果」', async ({ page }) => {
  await page.goto('/search')
  await expect(page.locator('.card-header')).toContainText('知识检索')
  // 一进页面就显示「没有找到相关内容」会让人以为库里是空的
  await expect(page.getByText('输入关键词或一句话描述，开始检索')).toBeVisible()
  await expect(page.getByText(/没有找到与/)).toBeHidden()
})

test('关键词检索：文件名命中与正文命中都能看到来源和片段', async ({ page }) => {
  test.setTimeout(180_000)
  await ensureIndexed(page, [NAMED, IN_BODY])

  await page.goto('/search')
  await search(page, '发布检查清单')

  await expect(page.getByText(/共 \d+ 份文件命中/)).toBeVisible()

  // 文件名命中的那份：标出了「文件名命中」，并且给出了正文里的片段
  const named = hitOf(page, NAMED)
  await expect(named).toBeVisible()
  await expect(named.getByText('文件名命中')).toBeVisible()
  await expect(named.locator('.match').first()).toContainText('发布检查清单')

  // 正文命中的那份：没有「文件名命中」标签，但片段里能看见这个词
  const body = hitOf(page, IN_BODY)
  await expect(body).toBeVisible()
  await expect(body.getByText('文件名命中')).toBeHidden()
  await expect(body.locator('.match').first()).toContainText('发布检查清单')

  // 结果卡片上要能看出文件的基本信息，而不是只有一个名字：
  // 大小按 KiB/MiB 这类可读单位显示（不是一串字节数），时间显示到分钟
  await expect(named).toContainText(/[\d.]+ (B|KiB|MiB)/)
  await expect(named).toContainText(/\d{4}-\d{2}-\d{2} \d{2}:\d{2}/)

  // 点文件名打开详情抽屉，说明结果确实指向库里的那份文件
  await named.locator('.hit-name').click()
  await expect(page.locator('.el-drawer')).toBeVisible()
  await expect(page.locator('.el-drawer')).toContainText(NAMED)
})

test('关键词检索搜不到时，空态与「还没搜过」区分开', async ({ page }) => {
  await page.goto('/search')
  await search(page, '这个词库里一定没有xyz')

  await expect(page.getByText(/没有找到与「这个词库里一定没有xyz」相关的内容/)).toBeVisible()
  // 顺便告诉用户下一步可以怎么办，而不是丢一句「无数据」
  await expect(page.getByText(/关键词按字面匹配/)).toBeVisible()
})

test('语义检索：整句话没有字面命中，仍然找得到对应文件', async ({ page }) => {
  test.setTimeout(180_000)
  await ensureIndexed(page, [CODE_RULE])

  const question = '合并代码之前需要做什么检查'

  // 先确认这句话在关键词检索里确实一无所获 —— 否则这个用例证明不了什么
  await page.goto('/search')
  await search(page, question)
  await expect(page.getByText(/没有找到与/)).toBeVisible()

  // 换成语义检索，同一句话应该找得到《代码提交规范》
  await page.getByRole('tab', { name: '语义检索' }).click()
  // 切换标签页必须把上一次的结果清掉，不然用户会以为这是新方式算出来的
  await expect(page.getByText('输入关键词或一句话描述，开始检索')).toBeVisible()
  await expect(page.getByText(/没有找到与/)).toBeHidden()

  await search(page, question)

  const hit = hitOf(page, CODE_RULE)
  await expect(hit).toBeVisible({ timeout: 30_000 })
  // 相似度以「相关度 0.xx」的形式展示，并说明低于阈值的片段被过滤掉了
  await expect(hit.getByText(/相关度 0\.\d\d/)).toBeVisible()
  await expect(page.getByText(/相似度低于 0\.\d\d 的片段未展示/)).toBeVisible()
  await expect(hit.locator('.match').first()).not.toBeEmpty()

  // 语义结果同样要能点开详情
  await hit.locator('.hit-name').click()
  await expect(page.locator('.el-drawer')).toContainText(CODE_RULE)
})

/** 在检索页的分类下拉里选一项。 */
async function pickCategory(page: Page, label: string) {
  await page.locator('.category-select').click()
  await page.locator('.el-select-dropdown__item').filter({ hasText: label }).first().click()
}

test('分类筛选在检索页生效：限定到别的分类就看不到它，限定到它所在的分类只剩它', async ({ page }) => {
  test.setTimeout(180_000)
  const api = `${process.env.E2E_BASE_URL ?? 'http://127.0.0.1:8080'}/api/v1`

  await ensureIndexed(page, [NAMED, IN_BODY])

  // 前置状态用接口铺：通过界面新建分类、再把文件挪过去，
  // 会把这个用例真正要验的东西（筛选有没有生效）淹没在一串无关操作里。
  const target = await (
    await page.request.post(`${api}/categories`, { data: { name: 'E2E检索筛选' } })
  ).json()
  const empty = await (
    await page.request.post(`${api}/categories`, { data: { name: 'E2E检索空分类' } })
  ).json()

  const list = await (await page.request.get(`${api}/documents?pageSize=100`)).json()
  const doc = list.items.find((it: { name: string }) => it.name === NAMED)
  if (!doc) throw new Error(`${NAMED} 不在库里，分类筛选的前置状态没法准备`)
  await page.request.patch(`${api}/documents/${doc.id}`, { data: { categoryId: target.id } })

  await page.goto('/search')

  // 不限分类：两份都能搜到
  await search(page, '发布检查清单')
  await expect(hitOf(page, NAMED)).toBeVisible()
  await expect(hitOf(page, IN_BODY)).toBeVisible()

  // 限定到 NAMED 所在的分类：只剩它
  await pickCategory(page, 'E2E检索筛选')
  await page.getByRole('button', { name: '检索' }).click()
  await expect(hitOf(page, NAMED)).toBeVisible()
  await expect(page.locator('.hit').filter({ hasText: IN_BODY })).toBeHidden({ timeout: 15_000 })

  // 限定到一个空分类：一份都不该有，且要给出空态而不是空白
  await pickCategory(page, 'E2E检索空分类')
  await page.getByRole('button', { name: '检索' }).click()
  await expect(page.getByText(/没有找到与「发布检查清单」相关的内容/)).toBeVisible()
})
