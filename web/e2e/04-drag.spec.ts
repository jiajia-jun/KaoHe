import { expect, test, type Page } from '@playwright/test'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

/**
 * 长按拖拽的端到端验收：一个手势、两种落点。
 *
 * 本文件接在 03-search.spec.ts 之后运行，库里已经有一批文件。
 * 需要哪份语料就现传哪份 —— 每个上传都落在列表最前，所以最近上传的几份
 * 一定占据最前面几行，断言就建立在这几行上，不受库里已有内容的影响。
 *
 * 拖拽只能用 page.mouse 一步一步驱动，不能用 Playwright 的 dragAndDrop：
 * 那是 HTML5 原生拖拽，而这里走的是 pointer 事件 + 380ms 长按闸门，两套机制无关。
 */
const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..')
const corpus = (name: string) => path.join(repoRoot, 'testdata', 'corpus', name)

/** 三份互不相同的语料，用来拼出一段可辨认的顺序 */
const A = '02_发布检查清单_v1.4.md'
const B = '09_本地部署故障排查_v1.3.txt'
const C = '10_文档分类与归档规范_v1.0.pdf'

/** 表格里按文件名定位一行。前面用例可能传过同名文件，一律取第一份。 */
function rowOf(page: Page, name: string) {
  return page.locator('.el-table__row').filter({ hasText: name })
}

/** 列表最前面几行的文件名。上传都落在最前，这几行是本次用例自己铺出来的。 */
async function topNames(page: Page, count: number): Promise<string[]> {
  return page.locator('.el-table__row .name-text').evaluateAll(
    (cells, limit) => cells.slice(0, limit).map((cell) => cell.textContent?.trim() ?? ''),
    count,
  )
}

/** 走一遍上传对话框，并等到它落进列表、遮罩也撤干净。 */
async function uploadVia(page: Page, name: string) {
  await page.getByRole('button', { name: '上传文件' }).click()
  const dialog = page.locator('.el-dialog')
  await expect(dialog).toBeVisible()
  await dialog.locator('input[type="file"]').setInputFiles(corpus(name))
  await dialog.getByRole('button', { name: '开始上传' }).click()
  await expect(page.getByText(`已上传「${name}」`)).toBeVisible()
  // 一次传完对话框会自己关上，但遮罩还要走完这段淡出。这一步必须等：
  // page.mouse.down() 不做可操作性检查，直接按在坐标上 —— 遮罩还在的话
  // 事件落到遮罩而不是表格行，长按永远不会开始，后面只能等到超时。
  await expect(dialog).toBeHidden()
}

/**
 * 按顺序上传若干文件，后传的落在更前面。
 *
 * 每次都真传，不与「库里已经有一份同名文件」抢时间：前面几个用例用的是同一批语料，
 * 跳过已存在的那份会让最前面几行变成别的文件，而本文件所有顺序断言都建立在这几行上。
 * 同名文件本来就允许共存，多传一份不会让谁的断言失真。
 */
async function seed(page: Page, names: string[]) {
  await page.goto('/documents')
  for (const name of names) await uploadVia(page, name)
  // 上传完列表要重取一次，而这一步是异步的：不等它落定就去读行坐标，
  // 量到的会是上一版表格里的位置，按下去就落在别的行上（甚至落在表格外面）。
  // 顺便把「新上传的文件排在最前」这个前提本身断言掉 —— 下面每个用例都建立在它上面。
  await expect.poll(() => topNames(page, names.length)).toEqual([...names].reverse())
}

/**
 * 按住某一行，等过 380ms 的长按闸门，返回时仍按着。
 *
 * 两个细节都不是可有可无的：
 * - 按下之后要等够长按时间，中途一动就作废（超过 5px 会被当成滚动列表）；
 * - 按点选在文件名那一格：行尾是操作列的按钮，长按会被「行内交互元素不抢手势」挡掉。
 */
async function startDrag(page: Page, name: string) {
  const box = (await rowOf(page, name).first().boundingBox())!
  const x = box.x + 100
  const y = box.y + box.height / 2
  await page.mouse.move(x, y)
  await page.mouse.down()
  await page.waitForTimeout(500)
  return { x, y }
}

/** 把按着的那一行拖到 (targetX, targetY) 并松手。松手前留一拍，让落点先算出来。 */
async function dropAt(page: Page, targetX: number, targetY: number) {
  await page.mouse.move(targetX, targetY, { steps: 10 })
  await page.waitForTimeout(120)
  await page.mouse.up()
}

/** 长按拖动一行到某个纵坐标（横向仍停在按下的位置）。 */
async function dragRowTo(page: Page, name: string, targetY: number) {
  const from = await startDrag(page, name)
  await dropAt(page, from.x, targetY)
}

/** 分类树上某个节点那一行（与 02-categories.spec.ts 同一套锚点）。 */
function catRow(page: Page, name: string) {
  return page.locator(`.node[data-name="${name}"]`)
}

/** 分类节点的计数徽标。 */
async function catCount(page: Page, name: string): Promise<string> {
  return (await catRow(page, name).locator('.node-count').innerText()).trim()
}

test('拖动一行到列表最前：顺序改变，且刷新之后仍然保留', async ({ page }) => {
  // seed 结束时已经确认过顺序是 [C, B, A]（每次上传都落在最前，所以是倒着的）
  await seed(page, [A, B, C])

  const firstRow = (await page.locator('.el-table__row').first().boundingBox())!
  const from = await startDrag(page, A)
  // 跟手的文件条是「长按真的触发了拖拽」而不是普通点击的证据
  await expect(page.locator('.drag-ghost')).toContainText(A)
  await dropAt(page, from.x, firstRow.y + 6)

  await expect.poll(() => topNames(page, 3)).toEqual([A, C, B])

  // 刷新后顺序还在，才说明它真的落到了库里，而不是只改了眼前这一份
  await page.reload()
  await expect(rowOf(page, A).first()).toBeVisible()
  expect(await topNames(page, 3)).toEqual([A, C, B])

  // 再拖回中间。落点不是最前时走的是另一条分支（锚点不再是 null），要单独验一次
  const second = (await page.locator('.el-table__row').nth(1).boundingBox())!
  await dragRowTo(page, A, second.y + second.height - 4)
  await expect.poll(() => topNames(page, 3)).toEqual([C, A, B])
})

test('把文件拖到分类节点上：移入该分类，并可以撤销', async ({ page }) => {
  const api = `${process.env.E2E_BASE_URL ?? 'http://127.0.0.1:8080'}/api/v1`
  // 前置状态用接口铺：这个用例要验的是「拖过去有没有生效」，
  // 从界面新建分类会把它淹没在一串无关操作里
  const target = await (
    await page.request.post(`${api}/categories`, { data: { name: 'E2E拖拽目标' } })
  ).json()

  await seed(page, [A, B, C])
  await page.goto('/documents')

  expect(await catCount(page, 'E2E拖拽目标')).toBe('0')

  const node = (await catRow(page, 'E2E拖拽目标').boundingBox())!
  await startDrag(page, B)
  await page.mouse.move(node.x + node.width / 2, node.y + node.height / 2, { steps: 10 })
  await page.waitForTimeout(120)
  // 悬停在分类上时那一行要亮起来，否则用户不知道松手会落到哪里
  await expect(catRow(page, 'E2E拖拽目标')).toHaveClass(/is-drop-target/)
  await page.mouse.up()

  await expect(page.getByText(`已把「${B}」移入「E2E拖拽目标」`)).toBeVisible()
  await expect.poll(() => catCount(page, 'E2E拖拽目标')).toBe('1')
  await expect(rowOf(page, B).first()).toContainText('E2E拖拽目标')

  // 撤销入口就在提示上，点一下回到原来的归属
  await page.getByRole('button', { name: '撤销' }).click()
  await expect(page.getByText(`已把「${B}」移回未分类`)).toBeVisible()
  await expect.poll(() => catCount(page, 'E2E拖拽目标')).toBe('0')
  await expect(rowOf(page, B).first()).toContainText('未分类')

  await page.request.delete(`${api}/categories/${target.id}`)
})

test('短按一行仍然是打开详情：长按闸门没把点击吃掉', async ({ page }) => {
  await seed(page, [A, B, C])

  await rowOf(page, A).first().click()
  await expect(page.locator('.el-drawer')).toBeVisible()
  await expect(page.locator('.el-drawer')).toContainText(A)
})

test('回收站里不给拖：连抓手都不出现', async ({ page }) => {
  await seed(page, [A, B, C])

  await expect(page.locator('.doc-grab').first()).toBeVisible()

  await page.getByRole('tab', { name: '回收站' }).click()
  // 回收站是暂存区，位置对它没有意义，所以连入口都不给
  await expect(page.locator('.doc-grab')).toHaveCount(0)
})
