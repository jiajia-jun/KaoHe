<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { ElMessage, ElMessageBox, type ElTree } from 'element-plus'
import { errorText } from '@/api/client'
import {
  categoryKeyOf,
  createCategory,
  deleteCategory,
  updateCategory,
  type CategoryNode,
} from '@/api/categories'

/**
 * 分类树。
 *
 * 树上混了两类节点：真实的分类，以及「全部文件」「未分类」两个视图节点。
 * 它们对用户的含义都是「当前查看的范围」，所以在同一棵树里呈现、
 * 共用同一套选中状态；但视图节点不能改名或删除，因此操作菜单只对真实分类出现。
 *
 * 选中父分类时后端会把它的整棵子树一并纳入筛选 ——
 * 否则点上层的分类反而比点下层看到的文件更少，与树形结构的直觉相反。
 */
type ViewMode = 'all' | 'none' | 'category'

interface TreeNode {
  /** el-tree 的 node-key。视图节点用固定字符串，真实分类加 cat- 前缀 */
  key: string
  label: string
  mode: ViewMode
  categoryId: number | null
  /** 仅真实分类有值；视图节点的规模由列表自身的总数体现 */
  count: number | null
  children: TreeNode[]
}

const props = defineProps<{
  /** 当前选中的节点 key，取值由 parseCategoryKey 解析 */
  modelValue: string
  /** 分类树数据由父组件统一持有：上传对话框与详情抽屉要用同一份，避免各自拉取后不一致 */
  categories: CategoryNode[]
  loading: boolean
  error: string
}>()

const emit = defineEmits<{
  'update:modelValue': [key: string]
  /** 分类结构发生变化（增删改），父组件需要重新拉取分类树与文件列表 */
  changed: []
  retry: []
}>()

const treeRef = ref<InstanceType<typeof ElTree>>()

function toTreeNode(node: CategoryNode): TreeNode {
  return {
    key: categoryKeyOf(node.id),
    label: node.name,
    mode: 'category',
    categoryId: node.id,
    // 显示含子分类的总数，这样「点这个分类能筛出多少」一眼可见
    count: node.totalCount,
    children: node.children.map(toTreeNode),
  }
}

const treeData = computed<TreeNode[]>(() => [
  { key: 'all', label: '全部文件', mode: 'all', categoryId: null, count: null, children: [] },
  { key: 'none', label: '未分类', mode: 'none', categoryId: null, count: null, children: [] },
  ...props.categories.map(toTreeNode),
])

// current-node-key 只在初始化时生效，选中态被外部改变（例如删掉分类后退回「全部文件」）
// 时要显式同步，否则高亮会停在一个已经不存在的节点上。
//
// 同时也要盯住 categories：树数据一变，el-tree 会重建节点、把当前选中项一并丢掉。
// 删除分类恰好同时触发这两件事，只监听 modelValue 的话高亮会刚设好又被清掉。
// flush: 'post' 保证在 el-tree 处理完新数据之后再补设。
watch(
  [() => props.modelValue, () => props.categories],
  ([key]) => treeRef.value?.setCurrentKey(key),
  { flush: 'post' },
)

function onNodeClick(node: TreeNode) {
  emit('update:modelValue', node.key)
}

/** 同级重名由后端返回 409，这里先做本地校验，把明显的问题挡在输入框。 */
function nameValidator(value: string): boolean | string {
  const name = value.trim()
  if (!name) return '分类名不能为空'
  if ([...name].length > 40) return '分类名不能超过 40 个字符'
  return true
}

async function promptCreate(parentId: number | null, parentLabel?: string) {
  try {
    const { value } = await ElMessageBox.prompt(
      parentLabel ? `将在「${parentLabel}」下新建子分类` : '新建一个顶层分类',
      parentLabel ? '新建子分类' : '新建分类',
      {
        inputPlaceholder: '分类名',
        inputValidator: nameValidator,
        confirmButtonText: '创建',
        cancelButtonText: '取消',
      },
    )
    const node = await createCategory(value.trim(), parentId)
    emit('update:modelValue', categoryKeyOf(node.id))
    emit('changed')
    ElMessage.success(`已创建分类「${node.name}」`)
  } catch (err) {
    // 用户点取消时 ElMessageBox 也会 reject，用字符串区分，不当作错误上报
    if (err === 'cancel' || err === 'close') return
    ElMessage.error(errorText(err))
  }
}

async function promptRename(node: TreeNode) {
  try {
    const { value } = await ElMessageBox.prompt('', '重命名分类', {
      inputValue: node.label,
      inputValidator: nameValidator,
      confirmButtonText: '保存',
      cancelButtonText: '取消',
    })
    await updateCategory(node.categoryId!, { name: value.trim() })
    emit('changed')
    ElMessage.success('已重命名')
  } catch (err) {
    if (err === 'cancel' || err === 'close') return
    ElMessage.error(errorText(err))
  }
}

async function confirmDelete(node: TreeNode) {
  const count = node.count ?? 0
  const warning =
    count > 0
      ? `该分类下的 ${count} 个文件将变为「未分类」，文件本身不会被删除。`
      : '该分类下没有文件。'
  try {
    await ElMessageBox.confirm(warning, `删除分类「${node.label}」`, {
      type: 'warning',
      confirmButtonText: '删除分类',
      cancelButtonText: '取消',
      confirmButtonClass: 'el-button--danger',
    })
    const result = await deleteCategory(node.categoryId!)
    // 删掉的正是当前选中的分类时退回「全部文件」，
    // 否则列表会停在一个已经不存在的筛选条件上
    if (props.modelValue === node.key) emit('update:modelValue', 'all')
    emit('changed')
    ElMessage.success(result.message)
  } catch (err) {
    if (err === 'cancel' || err === 'close') return
    ElMessage.error(errorText(err))
  }
}

</script>


<template>
  <el-card shadow="never" class="category-panel">
    <template #header>
      <div class="panel-header">
        <span>分类</span>
        <el-button text type="primary" size="small" @click="promptCreate(null)">新建</el-button>
      </div>
    </template>

    <el-skeleton v-if="loading" :rows="5" animated />

    <el-alert v-else-if="error" type="error" :closable="false" show-icon title="分类加载失败">
      <p class="error-detail">{{ error }}</p>
      <el-button text type="primary" @click="emit('retry')">重试</el-button>
    </el-alert>

    <el-tree v-else ref="treeRef" :data="treeData" node-key="key" highlight-current
      :expand-on-click-node="false" default-expand-all :indent="14" @node-click="onNodeClick">
      <template #default="{ data }">
        <!-- data-name 供端到端测试定位节点：el-tree 把子节点渲染在父节点的 DOM 内部，
             按「包含某文本的节点」去找会同时命中外层祖先，需要一个精确的锚点 -->
        <div class="node" :data-name="data.label">
          <span class="node-label" :title="data.label">{{ data.label }}</span>
          <span v-if="data.count !== null" class="node-count">{{ data.count }}</span>
          <!-- 三个动作一律常显，不做成悬停才出现的「···」菜单。
               删除入口藏在悬停后面时，用户找不到它，只会得出「这个功能没有」的结论 ——
               一个没人能发现的删除按钮，与不提供删除按钮是等价的。
               常显的代价是树上多几个符号，比功能看不见便宜得多。
               图标按钮没有可读文字，aria-label 既是屏幕阅读器的名字，
               也是端到端测试定位它们的锚点。 -->
          <span v-if="data.mode === 'category'" class="node-actions">
            <el-tooltip content="新建子分类" placement="top" :show-after="400">
              <button class="node-action" type="button" aria-label="新建子分类"
                @click.stop="promptCreate(data.categoryId, data.label)">
                <svg viewBox="0 0 16 16" aria-hidden="true">
                  <path d="M8 3.2v9.6M3.2 8h9.6" />
                </svg>
              </button>
            </el-tooltip>
            <el-tooltip content="重命名" placement="top" :show-after="400">
              <button class="node-action" type="button" aria-label="重命名" @click.stop="promptRename(data)">
                <svg viewBox="0 0 16 16" aria-hidden="true">
                  <path d="M11.4 2.3l2.3 2.3-8.6 8.6-3 .7.7-3z" />
                </svg>
              </button>
            </el-tooltip>
            <el-tooltip content="删除分类" placement="top" :show-after="400">
              <button class="node-action node-action-danger" type="button" aria-label="删除分类"
                @click.stop="confirmDelete(data)">
                <svg viewBox="0 0 16 16" aria-hidden="true">
                  <path d="M2.6 4.3h10.8M6.3 4.3V2.7h3.4v1.6M4.2 4.3l.6 8.8a.9.9 0 0 0 .9.8h4.6a.9.9 0 0 0 .9-.8l.6-8.8" />
                </svg>
              </button>
            </el-tooltip>
          </span>
        </div>
      </template>
    </el-tree>
  </el-card>
</template>

<style scoped>
.category-panel {
  height: 100%;
}

.panel-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
}

.node {
  display: flex;
  align-items: center;
  gap: 6px;
  width: 100%;
  padding-right: 4px;
}

.node-label {
  flex: 1;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.node-count {
  flex: none;
  font-size: 12px;
  color: var(--el-text-color-secondary);
}

.node-actions {
  flex: none;
  display: flex;
  align-items: center;
  /* 按钮本身有 18px 的点击区，再留空隙会把这一行撑得比树还宽 */
  gap: 0;
}

.node-action {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 18px;
  height: 18px;
  padding: 0;
  border: 0;
  border-radius: 4px;
  background: transparent;
  /* 常显但不抢眼：树的主体是分类名，操作是次要的 */
  color: var(--el-text-color-placeholder);
  cursor: pointer;
  transition: background-color 0.15s, color 0.15s;
}

.node-action svg {
  width: 13px;
  height: 13px;
  fill: none;
  stroke: currentColor;
  stroke-width: 1.5;
  stroke-linecap: round;
  stroke-linejoin: round;
}

.node-action:hover {
  background: var(--el-fill-color);
  color: var(--el-color-primary);
}

.node-action-danger:hover {
  background: var(--el-color-danger-light-9);
  color: var(--el-color-danger);
}

/* 键盘用户同样要能看到当前落在哪个按钮上，不能只有 :hover */
.node-action:focus-visible {
  outline: 2px solid var(--el-color-primary);
  outline-offset: 1px;
}

.error-detail {
  margin: 4px 0 8px;
  font-size: 13px;
  font-family: var(--el-font-family-mono, monospace);
}
</style>
