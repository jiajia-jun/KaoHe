import { expect, test, type Page } from '@playwright/test'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

/**
 * 常见屏幕尺寸下的布局。
 *
 * 这条用例的存在理由，是文件列表曾经在 1280 宽下横向溢出 170px：
 * 各列宽度合计 1104，而表格容器只有 934，右对齐的操作列整排落在屏幕外，
 * 「下载 / 归档 / 删除」只能横向滚动表格才点得到。1366 的笔记本同样溢出。
 *
 * 浏览器用例原先抓不到它 —— Playwright 点击前会先把元素滚进视口，
 * 所以「点得到按钮」这个断言在任何宽度下都成立。要发现溢出，只能直接量
 * 滚动容器的 clientWidth 与 scrollWidth，这正是本文件在做的事。
 *
 * 这也是 docs/TESTING.md §6 第 1 条「多种屏幕尺寸没有自动化用例」的补课。
 *
 * 本文件排在最后运行，但每个用例都自己造需要的那一行，单独跑也不依赖前面的文件。
 */
const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..')
const corpus = (name: string) => path.join(repoRoot, 'testdata', 'corpus', name)

/**
 * 用 PDF：它 indexStatus 是 not_supported，不建索引任务，
 * 因此列表不会每 1.5 秒静默刷新 —— 量列宽时表格是静止的。
 * 文件名够长，能把它压到折行；再带两个标签，把标签列也压到折行。
 */
const FILE = '08_检索技术说明_关键词与语义_v1.0.pdf'
const TAGS = '故障,复盘'

/** Playwright 默认视口就是 1280×720 —— 恰好是当初出问题的那个尺寸 */
const VIEWPORTS = [
  { width: 1280, height: 720 },
  { width: 1366, height: 768 },
  { width: 1440, height: 900 },
  { width: 1920, height: 1080 },
]

/** 表里没有行就现传一份，让断言建立在确定的非空状态上 */
async function ensureRow(page: Page) {
  await page.goto('/documents')
  if ((await page.locator('.el-table__row').count()) > 0) return

  await page.getByRole('button', { name: '上传文件' }).click()
  const dialog = page.locator('.el-dialog')
  await expect(dialog).toBeVisible()
  await dialog.locator('input[type="file"]').setInputFiles(corpus(FILE))
  await dialog.getByPlaceholder('用逗号分隔，例如：发布,复盘').fill(TAGS)
  await dialog.getByRole('button', { name: '开始上传' }).click()
  await expect(dialog).toBeHidden()
  await expect(page.locator('.el-table__row').first()).toBeVisible()
}

/**
 * 量表格有没有横向溢出。
 *
 * 判据是滚动容器的 clientWidth/scrollWidth，不是 .el-table__body 的宽度 ——
 * 后者比容器宽是「结果」，本身说明不了什么。
 *
 * el-table 渲染出首行之后还会再算一次列宽，首行出现时读到的可能是过渡态
 * （列宽合计偏小），所以连续采样，两次一致才作数。
 */
async function tableOverflow(page: Page) {
  const read = () =>
    page.evaluate(() => {
      const wrap = document.querySelector('.table-wrap .el-scrollbar__wrap')
      if (!wrap) return null
      return { client: wrap.clientWidth, scroll: wrap.scrollWidth }
    })

  await page.waitForTimeout(1500)

  let last = await read()
  for (let i = 0; i < 4; i++) {
    await page.waitForTimeout(500)
    const now = await read()
    if (now && last && now.client === last.client && now.scroll === last.scroll) break
    last = now
  }
  return last
}

test('常见屏幕尺寸下文件列表不出现横向滚动', async ({ page }) => {
  await ensureRow(page)

  for (const vp of VIEWPORTS) {
    await page.setViewportSize(vp)
    const m = await tableOverflow(page)
    expect(m, `视口 ${vp.width}×${vp.height} 下没找到表格滚动容器`).not.toBeNull()
    expect(
      m!.scroll,
      `视口 ${vp.width}×${vp.height} 下表格横向溢出 ${m!.scroll - m!.client}px` +
        `（容器 ${m!.client}px，内容 ${m!.scroll}px）`,
    ).toBeLessThanOrEqual(m!.client)
  }
})

/**
 * 量操作列那一格里每个按钮的位置。
 *
 * 量的是按钮在**内容坐标系**里的位置，不是视口坐标：
 * 视口坐标 = 内容坐标 − scrollLeft，于是「按钮贴到视口右边」既可能是列太宽，
 * 也可能是表格恰好被滚到了最右 —— 两者量出来一样，断言就分不清。
 * 减去容器原点再加回 scrollLeft 把滚动量抵消掉，剩下的才纯粹是布局。
 *
 * 整段放在一次 evaluate 里而不是用 locator：Playwright 取包围盒前会先把
 * 元素滚进视口，那样量到的位置又成了「滚了才看得见」，回到上面那个混淆。
 *
 * 空 tab 不会渲染 el-table，所以量不到容器是正常情况，返回 null 让调用方跳过；
 * 列表每 1.5 秒会因索引状态刷新重渲染，正好落在两次之间也可能量不到，故重试几次。
 */
async function measureOpsCell(page: Page) {
  for (let i = 0; i < 10; i++) {
    const m = await page.evaluate(() => {
      const wrap = document.querySelector('.table-wrap .el-scrollbar__wrap') as HTMLElement | null
      const cell = document.querySelector('.el-table__body tr')?.lastElementChild
      if (!wrap || !cell) return null
      // 内容坐标原点：容器左边缘往左退 scrollLeft
      const origin = wrap.getBoundingClientRect().left - wrap.scrollLeft
      return {
        clientWidth: wrap.clientWidth,
        buttons: [...cell.querySelectorAll('button')].map((b) => {
          const r = b.getBoundingClientRect()
          return {
            label: b.textContent?.trim() ?? '',
            left: Math.round(r.left - origin),
            right: Math.round(r.right - origin),
          }
        }),
      }
    })
    if (m) return m
    await page.waitForTimeout(300)
  }
  return null
}

test('操作列的按钮在常见屏幕尺寸下都落在视口内', async ({ page }) => {
  await ensureRow(page)

  // 三个 tab 的按钮不一样：回收站是「下载 / 恢复 / 彻底删除」，
  // 比「使用中」的「下载 / 归档 / 删除」宽 20px（「彻底删除」四个字），
  // 操作列的 204px 正是按回收站那一格定的 —— 只量默认那个 tab 就漏掉了正主。
  //
  // 让同一份文件顺着「归档 → 删除」走一遍，每轮量完再把它推进下一个 tab。
  // 比另造三份数据省事，也保证每一格量到的都是真实存在的那一行；
  // 顺序不能换，文件一离开「使用中」就回不来，除非先量过它。
  const ADVANCE: Record<string, { button: string; next: string }> = {
    使用中: { button: '归档', next: '已归档' },
    已归档: { button: '删除', next: '回收站' },
  }

  for (const tab of ['使用中', '已归档', '回收站']) {
    await page.getByRole('tab', { name: tab }).click()
    await page.waitForTimeout(1000)

    for (const vp of VIEWPORTS) {
      await page.setViewportSize(vp)
      await page.waitForTimeout(800)

      const m = await measureOpsCell(page)
      if (!m) continue // 这个 tab 没有行

      for (const b of m.buttons) {
        expect(
          b.right,
          `${tab}：视口 ${vp.width}×${vp.height} 下「${b.label}」落在可视区外（内容坐标 x=${b.right}，` +
            `可视区只有 ${m.clientWidth}px，要多横向滚动 ${b.right - m.clientWidth}px 才点得到）`,
        ).toBeLessThanOrEqual(m.clientWidth)
        expect(
          b.left,
          `${tab}：视口 ${vp.width}×${vp.height} 下「${b.label}」的左边缘在可视区外（内容坐标 x=${b.left}）`,
        ).toBeGreaterThanOrEqual(0)
      }
    }

    // 量完把这一行推给下一个 tab，让下一轮有数据。「归档」和「删除」都不弹确认框
    //（只有「彻底删除」弹），所以这里不需要处理对话框。
    const step = ADVANCE[tab]
    if (!step) break
    await page.locator('.el-table__row').first().getByRole('button', { name: step.button, exact: true }).click()
    await page.getByRole('tab', { name: step.next }).click()
    await page.waitForTimeout(1000)
  }
})
