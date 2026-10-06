import { expect, test, type Page } from '@playwright/test'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

/**
 * 分类管理的端到端验收，对应验收条件里的「分类调整」：
 * 新建分类、移动文件后，列表和筛选结果正确，刷新后仍保留。
 *
 * 本文件接在 01-documents.spec.ts 之后运行，起始状态是「有 3 个文件、没有任何分类」。
 * 每个用例自己建需要的分类与文件，不依赖前面用例留下的分类结构。
 */
const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..')
const corpus = (name: string) => path.join(repoRoot, 'testdata', 'corpus', name)

const DOC = '05_星桥项目_发布检查清单_2026-09-18.txt'

/**
 * 树上某个节点那一行。
 * 按 data-name 精确定位：el-tree 把子节点渲染在父节点的 DOM 内部，
 * 用「包含某文本的 .el-tree-node」去找会同时命中外层祖先节点。
 */
function catRow(page: Page, name: string) {
  return page.locator(`.node[data-name="${name}"]`)
}

/**
 * 某一行所属的 el-tree 节点。
 *
 * xpath 里按「独立单词」比对 class，而不是用 contains(@class,"el-tree-node")：
 * 节点结构是 .el-tree-node > .el-tree-node__content > .node，
 * 子串匹配会先命中更靠内的 __content，而那个元素并不包含子节点。
 */
function nodeOf(page: Page, name: string) {
  return catRow(page, name).locator(
    `xpath=ancestor::div[contains(concat(' ', normalize-space(@class), ' '), ' el-tree-node ')][1]`,
  )
}

/** 当前选中的节点。is-current 只标在唯一一个节点上，取它自己的那一行。 */
function currentNode(page: Page) {
  return page.locator('.el-tree-node.is-current > .el-tree-node__content .node')
}

/** 带输入框的对话框（新建 / 重命名）：填内容后点指定按钮。 */
async function answerPrompt(page: Page, value: string, button: string) {
  const box = page.locator('.el-message-box')
  await expect(box).toBeVisible()
  await box.locator('input').fill(value)
  await box.getByRole('button', { name: button }).click()
}

/** 纯确认的对话框（删除）：没有输入框，直接点按钮。 */
async function answerConfirm(page: Page, button: string) {
  const box = page.locator('.el-message-box')
  await expect(box).toBeVisible()
  await box.getByRole('button', { name: button }).click()
}

/** 打开某个分类节点的「···」菜单。 */
async function openMenu(page: Page, name: string) {
  await catRow(page, name).hover()
  await catRow(page, name).locator('.node-more').click()
}

/**
 * 在下拉菜单里选一项。下拉 teleport 到 body，得从 page 上找。
 *
 * 必须限定 :visible：el-dropdown 会把菜单内容一直留在 DOM 里（关闭时只是隐藏），
 * 树上有几个分类就有几份菜单，取 .first() 会点到最靠前那个分类的隐藏菜单上。
 */
async function pickMenuItem(page: Page, label: string) {
  await page.locator('.el-dropdown-menu__item:visible').filter({ hasText: label }).first().click()
}

/** 新建一个分类：点面板标题上的「新建」，或某个节点菜单里的「新建子分类」。 */
async function createCategory(page: Page, name: string, parent?: string) {
  if (parent) {
    await openMenu(page, parent)
    await pickMenuItem(page, '新建子分类')
  } else {
    await page.locator('.category-panel').getByRole('button', { name: '新建' }).click()
  }
  await answerPrompt(page, name, '创建')
}

/** 上传时在对话框里选好归属分类。 */
async function uploadInto(page: Page, filePath: string, categoryName: string) {
  await page.getByRole('button', { name: '上传文件' }).click()
  const dialog = page.locator('.el-dialog')
  await expect(dialog).toBeVisible()
  await dialog.locator('input[type="file"]').setInputFiles(filePath)
  await dialog.locator('.el-select').click()
  await page.locator('.el-select-dropdown__item:visible').filter({ hasText: categoryName }).first().click()
  await dialog.getByRole('button', { name: '开始上传' }).click()
}

/** 表格里按文件名定位一行。 */
function rowOf(page: Page, name: string) {
  return page.locator('.el-table__row').filter({ hasText: name })
}

/** 打开文件详情，把它的分类改成指定分类并保存。行的可见性由调用方自己保证。 */
async function moveDocTo(page: Page, docName: string, categoryName: string) {
  await rowOf(page, docName).click()
  const drawer = page.locator('.el-drawer')
  await expect(drawer).toBeVisible()
  await drawer.getByRole('button', { name: '修改信息' }).click()

  await drawer.locator('.el-select').click()
  await page
    .locator('.el-select-dropdown__item:visible')
    .filter({ hasText: categoryName })
    .first()
    .click()
  await drawer.getByRole('button', { name: '保存' }).click()
  await expect(page.getByText('已保存')).toBeVisible()
  await page.keyboard.press('Escape')
}

test('新建顶层分类与子分类，树上呈现层级', async ({ page }) => {
  await page.goto('/documents')

  await createCategory(page, '研发')
  await expect(page.getByText('已创建分类「研发」')).toBeVisible()
  await expect(catRow(page, '研发')).toBeVisible()

  await createCategory(page, '前端', '研发')
  await expect(catRow(page, '前端')).toBeVisible()

  // 子分类应当嵌在「研发」节点的子树里，而不是与它平级
  await expect(nodeOf(page, '研发').locator('.node[data-name="前端"]')).toHaveCount(1)
})

test('同级重名被拒绝，并指出原因', async ({ page }) => {
  await page.goto('/documents')
  await createCategory(page, '研发')
  // 请求本身没问题，是与现有数据冲突，提示要说明是重名而不是泛泛的失败
  await expect(page.getByText('同一层级下已存在同名分类')).toBeVisible()
})

test('把文件上传到指定分类，列表展示其归属', async ({ page }) => {
  await page.goto('/documents')
  await uploadInto(page, corpus(DOC), '前端')
  await expect(page.getByText(`已上传「${DOC}」`)).toBeVisible()

  const row = rowOf(page, DOC)
  await expect(row).toBeVisible()
  await expect(row).toContainText('前端')
})

test('筛选分类：选父分类能筛到子分类下的文件', async ({ page }) => {
  await page.goto('/documents')
  // 文件实际归属「前端」，点上层「研发」也应命中 —— 否则点上层反而比点下层看到的更少
  await catRow(page, '研发').click()
  await expect(currentNode(page)).toHaveAttribute('data-name', '研发')
  await expect(rowOf(page, DOC)).toBeVisible()

  await catRow(page, '前端').click()
  await expect(currentNode(page)).toHaveAttribute('data-name', '前端')
  await expect(rowOf(page, DOC)).toBeVisible()
})

test('「未分类」节点筛出没有归属的文件', async ({ page }) => {
  await page.goto('/documents')
  await catRow(page, '未分类').click()

  // 01 里上传的其余文件没有归类，应当出现在这里；归类到「前端」的那个不该出现
  await expect(rowOf(page, DOC)).toHaveCount(0)
  await expect(page.locator('.el-table__row').first()).toBeVisible()
})

test('移动文件到另一个分类：原分类下消失，新分类下出现', async ({ page }) => {
  await page.goto('/documents')
  // 文件当前归在「前端」下（上一个用例就这么传的）
  await catRow(page, '前端').click()
  await expect(rowOf(page, DOC)).toBeVisible()

  await moveDocTo(page, DOC, '研发')

  // 还停在「前端」筛选下，移走了就该消失
  await expect(rowOf(page, DOC)).toHaveCount(0)

  await catRow(page, '研发').click()
  const moved = rowOf(page, DOC)
  await expect(moved).toBeVisible()
  await expect(moved).toContainText('研发')

  // 放回「前端」：后面的用例仍按「该文件归在『前端』下」来断言
  await moveDocTo(page, DOC, '前端')
  await catRow(page, '前端').click()
  await expect(rowOf(page, DOC)).toContainText('前端')
})

test('刷新后分类结构与文件归属都还在', async ({ page }) => {
  await page.goto('/documents')
  await page.reload()

  await expect(catRow(page, '研发')).toBeVisible()
  await expect(catRow(page, '前端')).toBeVisible()

  await catRow(page, '研发').click()
  await expect(rowOf(page, DOC)).toBeVisible()
})

test('重命名分类后，树上与列表同时更新', async ({ page }) => {
  await page.goto('/documents')
  await openMenu(page, '前端')
  await pickMenuItem(page, '重命名')
  await answerPrompt(page, '移动端', '保存')
  await expect(page.getByText('已重命名')).toBeVisible()

  await expect(catRow(page, '前端')).toHaveCount(0)
  await catRow(page, '研发').click()
  await expect(rowOf(page, DOC)).toContainText('移动端')
})

test('有子分类时不允许删除，并说明原因', async ({ page }) => {
  await page.goto('/documents')
  await openMenu(page, '研发')
  await pickMenuItem(page, '删除')
  await answerConfirm(page, '删除分类')

  await expect(page.getByText('该分类下还有子分类，请先删除或移走子分类')).toBeVisible()
  await expect(catRow(page, '研发')).toBeVisible()
})

test('删除分类：文件不被删除，转为未分类', async ({ page }) => {
  await page.goto('/documents')

  // 先删子分类。它下面有 1 个文件，确认框要把这件事说清楚
  await openMenu(page, '移动端')
  await pickMenuItem(page, '删除')
  const box = page.locator('.el-message-box')
  await expect(box).toContainText('1 个文件将变为「未分类」')
  await expect(box).toContainText('文件本身不会被删除')
  await box.getByRole('button', { name: '删除分类' }).click()
  await expect(page.getByText('分类已删除，1 个文件已转为未分类')).toBeVisible()

  await expect(catRow(page, '移动端')).toHaveCount(0)

  // 此时「研发」已没有子分类，可以删除
  await openMenu(page, '研发')
  await pickMenuItem(page, '删除')
  await answerConfirm(page, '删除分类')
  await expect(catRow(page, '研发')).toHaveCount(0)

  // 文件还在，只是回到未分类
  await catRow(page, '未分类').click()
  await expect(rowOf(page, DOC)).toBeVisible()
})

test('删除当前选中的分类后，筛选回到「全部文件」', async ({ page }) => {
  await page.goto('/documents')
  await createCategory(page, '临时分类')

  await openMenu(page, '临时分类')
  await pickMenuItem(page, '删除')
  await answerConfirm(page, '删除分类')

  // 停在一个已经不存在的筛选条件上，列表会一直空着且无从解释
  await expect(currentNode(page)).toHaveAttribute('data-name', '全部文件')
})
