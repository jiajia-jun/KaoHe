import { ref, type Ref } from 'vue'

/**
 * 文件列表的长按拖拽：一个手势，两种落点。
 *
 * - 落在两行之间 → 调整次序（`onReorder`）；
 * - 落在左侧分类节点上 → 改变归属（`onDropCategory`）。
 *
 * 用 pointer 事件而不是 HTML5 原生拖拽，有三个理由：
 * 1. 原生 `draggable` 会与「点击行打开详情」抢同一个手势，得额外写一堆取消逻辑；
 * 2. 指针事件在触摸屏上同样触发，原生 `dragstart` 不会；
 * 3. 只有指针事件才拿得到实时坐标，画得出跟手的文件条和行间插入线。
 *
 * 代价是「点击」与「拖拽」共用同一个 pointerdown，必须自己划清界限：
 * 按住 380ms 且几乎没动才进入拖拽，否则松手仍然是普通点击。
 */

/** 按住多久才算要拖。短于这个时长的按下依旧是「点击打开详情」。 */
const LONG_PRESS_MS = 380

/** 长按期间允许的抖动。超出说明用户是在滚列表或选文字，这次按下就不再算数。 */
const MOVE_TOLERANCE = 5

/** 松手后要吃掉的那一次 click 的有效期。 */
const CLICK_SUPPRESS_MS = 300

/**
 * 落点锚点由 DOM 属性给出，不在内存里另存一份坐标。
 *
 * el-table 不给 `<tr>` 挂自定义属性（只有 row-key 与 row-class-name 两个口子），
 * 所以 `data-doc-id` 打在文件名单元格的内容上，需要行元素时再往上找它的 `<tr>`。
 */
const ROW_SELECTOR = '[data-doc-id]'

/** 分类节点上的落点锚点，由 CategoryTree 渲染。 */
const CAT_SELECTOR = '[data-cat-key]'

/** 行首的抓手：显式的拖动入口，按下即可拖，不必等长按。 */
const GRAB_SELECTOR = '.doc-grab'

/** 行内这些元素有自己的行为，长按不该把它们抢走。 */
const INTERACTIVE = 'button, a, input, textarea, select, [contenteditable]'

export interface DragOptions {
  /** 当前是否允许拖动：回收站、搜索结果、加载中都不该能拖 */
  available: () => boolean
  /**
   * 「拖到第一行之上的那一格」是否可用。
   *
   * 那一格的含义是全局置顶（afterId 为 null）。只有在列表本身就是全局序列的开头
   * （第一页）时它才成立；翻到第二页之后，屏幕上方还有看不见的行，
   * 而这个接口只认相对锚点，表达不出「插到本页第一行之前但又不是全局最前」。
   * 与其把文件凭空送到用户看不见的第一页，不如让那里根本不是落点。
   */
  canDropAtTop?: () => boolean
  /** 落在两行之间。afterId 为 null 表示落到最前，即全局置顶 */
  onReorder: (docId: string, afterId: string | null) => void
  /** 落在分类节点上。categoryId 为 null 表示「未分类」 */
  onDropCategory: (docId: string, categoryId: number | null) => void
  /** 拖动开始与结束，调用方据此暂停后台刷新 */
  onStart?: () => void
  onEnd?: () => void
}

export interface DocumentDrag {
  isDragging: Ref<boolean>
  /** 正在被拖动的文件标识；调用方据此取出文件名渲染跟手的文件条 */
  draggingId: Ref<string | null>
  /** 文件条当前的位置，给 fixed 定位用 */
  ghost: Ref<{ x: number; y: number }>
  /** 插入线画在第几行之前；null 表示不画（落点无变化，或指针在分类树上） */
  dropIndex: Ref<number | null>
  /** 高亮中的分类节点 key，透传给 CategoryTree */
  dropCatKey: Ref<string | null>
  /**
   * 绑定到表格根节点的 pointerdown。
   *
   * 用事件委托而不是给每行挂监听：行是 el-table 渲染的，拿不到行元素；
   * 而且委托天然覆盖行内任何一格 —— 用户不会只从文件名那一格开始拖。
   */
  onPointerDown: (event: PointerEvent) => void
}

export function useDocumentDrag(options: DragOptions): DocumentDrag {
  const isDragging = ref(false)
  const draggingId = ref<string | null>(null)
  const ghost = ref({ x: 0, y: 0 })
  const dropIndex = ref<number | null>(null)
  const dropCatKey = ref<string | null>(null)

  let pressTimer: ReturnType<typeof setTimeout> | null = null
  /** 已按下、还没确定是点击还是拖拽的那一行 */
  let armed: string | null = null
  let originX = 0
  let originY = 0
  let pointerX = 0
  let pointerY = 0
  /**
   * 拖动开始那一刻的可见行锚点，按 DOM 顺序。
   * 拖动期间不再重新读：一来看不到的行拖不到，二来中途刷新换掉列表会让手里的行对不上。
   */
  let anchors: HTMLElement[] = []
  let fromIndex = -1
  let dragging = ''
  /** 与 dropCatKey 同步更新的分类 id：key 用来高亮，id 才是接口要的东西 */
  let dropCatId: number | null = null

  /** 行锚点按 DOM 顺序，也就是用户看到的那份顺序 —— DOM 不会背着我们过期。 */
  function rowAnchors(): HTMLElement[] {
    return Array.from(document.querySelectorAll<HTMLElement>(`tbody ${ROW_SELECTOR}`))
  }

  /** 锚点所在的整行；拿不到就退回锚点自己，至少有个大致位置。 */
  function rowBox(anchor: HTMLElement): DOMRect {
    return (anchor.closest('tr') ?? anchor).getBoundingClientRect()
  }

  function clearPress() {
    if (pressTimer !== null) {
      clearTimeout(pressTimer)
      pressTimer = null
    }
  }

  function detach() {
    window.removeEventListener('pointermove', onMove)
    window.removeEventListener('pointerup', onUp)
    window.removeEventListener('pointercancel', onCancel)
    window.removeEventListener('keydown', onKey, true)
  }

  function reset() {
    isDragging.value = false
    draggingId.value = null
    dragging = ''
    dropIndex.value = null
    dropCatKey.value = null
    dropCatId = null
    anchors = []
    fromIndex = -1
    armed = null
    document.body.classList.remove('is-doc-dragging')
  }

  /**
   * 吃掉拖拽结束时紧跟而来的那一次 click。
   *
   * 松手会先后触发 pointerup 与 click，而行点击绑在 click 上 ——
   * 不拦的话，每次拖完都会顺手把详情抽屉打开（拖到分类上时还会顺手把那个分类选中）。
   * 用捕获阶段拦，才能在 el-table 的处理函数之前生效。
   */
  function swallowNextClick() {
    const handler = (event: MouseEvent) => {
      event.stopPropagation()
      event.preventDefault()
    }
    window.addEventListener('click', handler, true)
    setTimeout(() => window.removeEventListener('click', handler, true), CLICK_SUPPRESS_MS)
  }

  function begin() {
    clearPress()
    if (!armed || isDragging.value) return

    const list = rowAnchors()
    const from = list.findIndex((anchor) => anchor.dataset.docId === armed)
    // 行已不在 DOM 里（比如按下之后列表被刷新过），干脆不开始，别留个半吊子状态
    if (from < 0) {
      armed = null
      return
    }

    anchors = list
    fromIndex = from
    dragging = armed
    draggingId.value = armed
    ghost.value = { x: pointerX, y: pointerY }
    isDragging.value = true
    document.body.classList.add('is-doc-dragging')
    options.onStart?.()
  }

  function onMove(event: PointerEvent) {
    pointerX = event.clientX
    pointerY = event.clientY

    if (!isDragging.value) {
      if (!armed) return
      if (Math.hypot(pointerX - originX, pointerY - originY) > MOVE_TOLERANCE) {
        // 长按期间动了这么多：用户是在滚动列表或选文字，这次按下不作数
        clearPress()
        armed = null
        detach()
      }
      return
    }

    ghost.value = { x: pointerX, y: pointerY }

    // 命中判定一律从坐标反查 DOM，不在内存里再维护一份「哪个元素在什么位置」——
    // 那份数据会随着滚动、展开、刷新悄悄过期，而 DOM 永远就是用户看到的那个样子。
    const under = document.elementFromPoint(pointerX, pointerY)
    const catEl = under?.closest<HTMLElement>(CAT_SELECTOR)
    const mode = catEl?.dataset.catMode
    // 「全部文件」是视图不是分类，不接受落下
    if (catEl && (mode === 'category' || mode === 'none')) {
      dropCatKey.value = catEl.dataset.catKey ?? null
      dropCatId = mode === 'none' ? null : Number(catEl.dataset.catId)
      dropIndex.value = null
      return
    }

    dropCatKey.value = null
    dropCatId = null

    // 插入点按每行的垂直中线算：指针越过中线，就说明它已经属于下一行
    let target: number | null = anchors.length
    for (let i = 0; i < anchors.length; i += 1) {
      const box = rowBox(anchors[i])
      if (pointerY < box.top + box.height / 2) {
        target = i
        break
      }
    }
    // 落点就是原位（拖到自己前面或后面）时不画插入线：那里没有变化可做
    if (target === fromIndex || target === fromIndex + 1) target = null
    if (target === 0 && options.canDropAtTop && !options.canDropAtTop()) target = null
    // ref 只在值真的变了才触发更新，所以这里不必自己去重；
    // 每次 pointermove 都改的话整个表体都会跟着重渲染
    dropIndex.value = target
  }

  /** commit 为 false 表示取消（Esc 或 pointercancel）：什么都不落 */
  function endDrag(commit: boolean) {
    detach()
    clearPress()

    if (!isDragging.value) {
      armed = null
      return
    }

    const id = dragging
    const index = dropIndex.value
    const catKey = dropCatKey.value
    const catId = dropCatId
    const anchorId = index !== null && index > 0 ? (anchors[index - 1]?.dataset.docId ?? null) : null
    reset()
    swallowNextClick()
    options.onEnd?.()

    if (!commit) return

    if (catKey !== null) {
      options.onDropCategory(id, catId)
      return
    }
    if (index === null) return
    // 「插到第 index 行之前」翻译成接口要的相对锚点：锚点就是它前面那一行。
    // 落到最前时没有锚点，正是后端的置顶语义 —— 接口收相对锚点而不是下标，
    // 就是因为界面上的列表随时可能只是全局次序的一个子序列。
    options.onReorder(id, anchorId)
  }

  function onUp() {
    endDrag(true)
  }

  function onCancel() {
    endDrag(false)
  }

  function onKey(event: KeyboardEvent) {
    if (event.key === 'Escape') endDrag(false)
  }

  function attach(event: PointerEvent, captureOn: Element | null) {
    originX = pointerX = event.clientX
    originY = pointerY = event.clientY
    window.addEventListener('pointermove', onMove)
    window.addEventListener('pointerup', onUp)
    window.addEventListener('pointercancel', onCancel)
    window.addEventListener('keydown', onKey, true)
    // 指针捕获：手滑出表格甚至滑到窗口外也还收得到事件。
    // 个别输入设备不支持，失败了也不影响主流程（window 上的监听照常工作）。
    try {
      captureOn?.setPointerCapture(event.pointerId)
    } catch {
      /* 不支持捕获时退化成普通的 window 监听 */
    }
  }

  function onPointerDown(event: PointerEvent) {
    if (event.button !== 0 || !options.available() || isDragging.value) return
    const target = event.target as HTMLElement | null
    if (!target) return

    const row = target.closest('tr')
    const anchor = row?.querySelector<HTMLElement>(ROW_SELECTOR)
    const docId = anchor?.dataset.docId
    if (!row || !docId) return

    const grabbing = !!target.closest(GRAB_SELECTOR)
    // 行内的按钮、链接、输入框有自己的行为，抓手除外
    if (!grabbing && target.closest(INTERACTIVE)) return

    attach(event, row)
    armed = docId
    // 抓手是显式的拖动入口，按下就拖；行的其他地方要按住不动，短按仍然是打开详情
    if (grabbing) begin()
    else pressTimer = setTimeout(begin, LONG_PRESS_MS)
  }

  return {
    isDragging,
    draggingId,
    ghost,
    dropIndex,
    dropCatKey,
    onPointerDown,
  }
}
