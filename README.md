# 文件管理与知识检索平台

解决文件分散、分类混乱、查找困难和归档后难以复用的问题。
支持文件的**上传、整理、检索、下载、归档与删除**，并通过**真实的文本向量**提供语义检索。

> 交付物索引：架构设计与关键取舍见 [`docs/DESIGN.md`](docs/DESIGN.md)，
> 测试与验收说明见 [`SELF-CHECK.md`](SELF-CHECK.md)，
> 5 组自然语言检索示例见 [`docs/RETRIEVAL-EXAMPLES.md`](docs/RETRIEVAL-EXAMPLES.md)。
>
> **已知边界**：PDF 当前只做存储与在线预览，正文索引是**待补的功能**而非设计取舍 ——
> 验收语料的 PDF 正文实际取得到（见 [`docs/DESIGN.md`](docs/DESIGN.md) §3.5 的更正）；
> 没有身份认证，任何能访问该端口的人都能读写全部文件。
> 其余没有验到的部分集中在 [`SELF-CHECK.md`](SELF-CHECK.md) §6。

---

## 一、快速开始

### 前置条件

- Docker Desktop（含 Docker Compose v2），建议分配 4 GB 以上内存
- 首次构建需要能访问镜像仓库与 npm registry

### 启动

```bash
git clone <仓库地址> && cd KaoHe

cp .env.example .env
# 打开 .env，至少把 POSTGRES_PASSWORD 改成自己的口令

docker compose up --build
```

首次构建需要编译 Go、构建前端并拉取镜像，约需数分钟。待日志出现
`api 已启动` 后，浏览器打开：

<http://localhost:8080>

若 8080 被占用，改 `.env` 里的 `WEB_PORT` 即可（容器内部不受影响）。

### 停止

```bash
docker compose down
```

> ⚠️ **不要使用 `docker compose down -v`。**
> `-v` 会连同具名卷一起删除，文件、分类与归档状态将全部丢失，且无法通过重建镜像恢复。
> 正常情况下 `-v` 也不必要：`./logs` 是绑定挂载、不在卷里，但 `uploads` 与 `pgdata` 会一起没。

---

## 二、五分钟走一遍

| 步骤 | 在哪里 | 做什么 |
| --- | --- | --- |
| 1 | 浏览器 →「系统状态」 | 应看到**接口服务 ok / 数据库 ok**，说明四层链路（浏览器 → nginx → api → PostgreSQL）已通 |
| 2 | 文件管理（首页） | 点右上角**上传文件**，把 `testdata/corpus/` 里的文档拖进去。上传时有进度条 |
| 3 | 文件管理 | 看**索引状态**列：Markdown/TXT 会从「待索引」走到「已索引」，PDF 显示「仅存储」 |
| 4 | 左侧分类树 | 点「新建」建几个分类，或在详情抽屉里改一份文件的归属 |
| 5 | 文件管理 | **长按列表里的任意一行**（或按住行首的抓手）拖动：落到两行之间是调顺序，落到左侧分类节点上是改归属 |
| 6 | 知识检索 →「关键词检索」 | 搜 `发布检查清单`：文件名命中的排在前面，正文提到的也带出片段 |
| 7 | 知识检索 →「语义检索」 | 输入 `容器起来了但是服务还是用不了`（整句在关键词检索里 0 命中） |
| 8 | 文件管理 | 把某个文件**归档**，它会从默认列表消失，切到「已归档」标签页还能找到 |
| 9 | 文件管理 | 点某行的**删除**：文件移入**回收站**，提示里带一个「撤销」；切到「回收站」标签页可以**恢复**或**彻底删除** |
| 10 | 文件管理 | 在回收站里点**清空回收站**，会先弹一个红色确认框 —— 这一步不可逆 |

第 5 步的两种落点是**同一个手势**，但顺序是整张表的**一条序列**，落库，新上传的文件排在最前。
归档区、回收站、某个分类的列表都是这条序列的子序列 —— 归档再恢复会回到原位，
因为「排在第几」和「在不在归档区」是两件互不干扰的事。
回收站标签页和搜索结果里不给拖：前者是暂存区，位置对它没有意义；
后者的次序由相关性决定，拖它说不清会拖到哪儿去。

第 7 步是这套系统最值得看的一处：**用与原文完全不同的措辞，找到对应的文档和片段。**
完整的 5 组示例（含预期命中的文件）见 [`docs/RETRIEVAL-EXAMPLES.md`](docs/RETRIEVAL-EXAMPLES.md)。

第 9、10 步值得单独说一句：**删除是两步的，而且两步的强度刻意不同。**
「删除」只是把文件移进回收站，原文件、分类归属和已经建好的索引一个都不动，
所以它不弹确认框、反而给一个撤销入口 —— 拦一道只会让人对着能撤销的操作犹豫。
真正毁掉数据的动作（彻底删除、清空回收站）才弹红色确认框，并且文案里写明「无法恢复」。
分类的删除则是硬删除，但入口改成每行常显的图标按钮：删除入口藏在悬停菜单后面时，
用户找不到它只会得出「这个功能没有」的结论，那和不提供删除按钮是等价的。

---

## 三、配置项

所有配置来自环境变量，仓库内只提供 `.env.example`，**密钥一律不写进仓库**。

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `POSTGRES_USER` | — | 必填，数据库账号 |
| `POSTGRES_PASSWORD` | — | 必填，数据库口令 |
| `POSTGRES_DB` | — | 必填，数据库名 |
| `WEB_PORT` | `8080` | 宿主机访问端口，唯一对外暴露的端口 |
| `MAX_UPLOAD_BYTES` | `20971520` | 单文件上传上限（20 MiB），超限返回 413 |
| `SEMANTIC_MIN_SCORE` | `0.5` | 语义检索的余弦相似度下限，低于它的片段不展示 |
| `LOG_LEVEL` | `info` | 日志等级：`debug` / `info` / `warn` / `error` |
| `LOG_CONSOLE_FORMAT` | `console` | **只影响 stdout** 的格式，文件恒为 JSON |
| `LOG_HOST_DIR` | `./logs` | 日志落在宿主机哪个目录（容器内固定为 `/data/logs`） |
| `LOG_MAX_SIZE_MB` | `32` | 单文件超过它就切一份，必须为正 |
| `LOG_MAX_BACKUPS` | `5` | 最多保留几份轮转文件，`0` 表示不限 |
| `LOG_MAX_AGE_DAYS` | `30` | 轮转文件最多留几天，`0` 表示不限 |
| `LOG_COMPRESS` | `false` | 轮转文件是否压成 `.gz`（开了就得用 `zcat` 看） |
| `SLOW_REQUEST_MS` | `500` | 超过它的请求标 `slow=true`，并升到 `warn` |
| `SLOW_TASK_MS` | `5000` | 超过它的索引任务标 `slow=true` |

`db`、`api`、`worker`、`embed` 均**不映射宿主机端口**，只在 compose 内网互通，
因此不存在与本机已有服务抢端口的问题。

`SEMANTIC_MIN_SCORE` 的值是按当前向量模型在验收语料上标定出来的，
换模型后必须重新标定 —— 标定方法与实测数据见 `docs/DESIGN.md` §4.3。

### 日志

`api` / `worker` / `migrate` 三个角色各写一个文件，绑定挂载到仓库下的 `./logs`：

```text
logs/api.log       logs/worker.log       logs/migrate.log
```

按角色分文件是**必须的**，不是偏好：`api` 与 `worker` 是两个独立进程，
而轮转是「重命名 + 新建」、不做多进程同步，两个进程写同一个文件会交替丢日志、轮转互相踩。
同理，**同时跑两套栈时必须给其中一套换 `LOG_HOST_DIR`**（见 §八 的空库自测）。

文件是 JSON，每行一条，`service` 字段标明来自哪个角色：

```jsonc
{"level":"info","ts":"…","caller":"indexer/worker.go:200","msg":"索引完成",
 "service":"worker","jobId":9,"documentId":10,"attempt":1,
 "chunks":12,"truncated":false,"costMs":255.52}
```

跨文件关联靠这几个字段，排查时优先按它们 grep：

| 字段 | 含义 |
| --- | --- |
| `requestId` | 一次 HTTP 请求，回写到响应头 `X-Request-Id`，仅 api 侧有 |
| `documentId` | 文档的数字主键 |
| `docUID` | 文档对外的 `doc_xxxx` 标识 |
| `jobId` | 一次索引任务 |
| `service` | 进程角色（`api` / `worker` / `migrate`） |

索引是异步的，worker 处理任务时那次 HTTP 请求早已结束、而且在另一个容器里，
所以 **worker 日志里没有 `requestId`** —— 硬塞进去只是撒谎。
一份文档跨两个文件的连接键是 `documentId`：上传成功后 api 记一条
`文档已入库`（带 `documentId` 与 `docUID`），worker 处理它时记同一个 `documentId`：

```jsonc
// api.log   —— 上传时
{"msg":"文档已入库","documentId":19,"docUID":"doc_40f0db59ee132146","enqueued":true}
// worker.log —— 索引时
{"msg":"索引完成","jobId":17,"documentId":19,"attempt":1,"chunks":2,"costMs":76.08}
```

两个开关值得单独说：

- `SLOW_REQUEST_MS` / `SLOW_TASK_MS` 超出的条目会带 `slow=true`，请求还会升到 `warn`。
  这样 `LOG_LEVEL=warn` 时留下的正好全是异常，慢请求不会被淹没在正常行里。
  默认值是实测标定的：本机接口请求绝大多数在 45ms 以内，索引任务在 16ms~585ms。
- `LOG_DIR` 不可写时进程**拒绝启动**。宁可起不来，也不要一个看起来正常、
  却永远不产生日志文件的进程 —— 「日志悄悄失效」正是这次要消灭的那类问题。

---

## 四、服务构成

| 服务 | 说明 |
| --- | --- |
| `web` | nginx 托管前端静态资源，并把 `/api/` 反向代理到 `api` |
| `api` | Go + Gin，对外 HTTP 接口 |
| `worker` | 索引任务消费者（与 `api` 同一个镜像，`--mode=worker`） |
| `embed` | 文本向量边车（ONNX Runtime + BGE 中文模型），只提供 `POST /embed` |
| `db` | PostgreSQL 16 + pgvector + pg_trgm |
| `migrate` | 一次性任务，建扩展、表与索引；成功后 `api` 才启动 |

`api`、`worker`、`migrate` 由**同一个镜像**以不同 `--mode` 启动，避免三份依赖版本漂移。

### 数据持久化

| 具名卷 | 内容 |
| --- | --- |
| `pgdata` | PostgreSQL 数据目录 |
| `uploads` | 上传的原始文件 |

容器重启或重新创建（`docker compose down && docker compose up`）后，数据仍然存在。

### 只启动其中一部分

`api` **刻意不依赖** `embed` 的健康状态：语义检索是可选能力，
用它把整个平台拦在启动之外不划算。边车没起来时文件管理与关键词检索照常工作，
只有语义检索返回 `503 service_unavailable`，界面会把原因说清楚。

同理，`web` 的 nginx 用 Docker 内嵌 DNS 动态解析 `api` 地址，
因此**单独重建 `api` 容器**（地址会变）之后不需要连 `web` 一起重启 ——
否则请求会一直打到已经失效的地址上，对外表现为 502。

---

## 五、接口一览

对外接口统一挂在 `/api/v1` 下，只有容器健康检查 `/healthz` 例外
（它由 compose 直接打 `api`，不经过 nginx）。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/healthz` | 健康检查；数据库不可达时返回 503，前端首页据此显示链路状态 |
| GET | `/config` | 上传上限、支持的格式，前端据此做前置校验 |
| POST | `/documents` | 上传（multipart） |
| GET | `/documents` | 列表；`q`、`categoryId`、`uncategorized`、`archived`、`trashed` 可组合 |
| GET | `/documents/:id` | 详情 |
| PATCH | `/documents/:id` | 改名 / 改标签 / 改归属分类 / 归档 / 恢复 |
| GET | `/documents/:id/download` | 下载；`?inline=true` 供 PDF 在线预览（支持 Range） |
| POST | `/documents/:id/reindex` | 重新索引 |
| POST | `/documents/:id/position` | 调整顺序：`{"afterId": "doc_xxx"}` 表示挪到该文件之后，`null` 表示置顶 |
| DELETE | `/documents/:id` | **移入回收站**，可撤销 |
| POST | `/documents/:id/restore` | 从回收站恢复 |
| DELETE | `/documents/:id/purge` | **彻底删除**，仅对回收站中的文件生效 |
| DELETE | `/trash` | 清空回收站，返回清掉的条数 |
| GET | `/search` | 关键词检索（文件名 + 正文） |
| POST | `/search/semantic` | 语义检索 |
| GET / POST | `/categories` | 分类树 / 新建分类 |
| PATCH / DELETE | `/categories/:id` | 改名 / 移动 / 删除分类 |

删除为什么是两步：`DELETE /documents/:id` 只是打上 `deleted_at`，
原文件与已经建好的索引都还在，随时可以恢复；`/purge` 才是真的抹掉。
**彻底删除只对回收站里的文件生效** —— 对一份使用中的文件直接调 `/purge` 会拿到 409，
必须先移入回收站。多出来的这一步不是仪式，它保证误删永远有一步可以停下来。

回收站里的文件在**列表、关键词检索、语义检索**三处都不可见。
这三个入口共用同一个过滤条件，因此不存在「删掉了但还能被搜出来」这种漏法 ——
一处漏掉比没有删除功能更糟。

`/position` 收的是**相对锚点**（挪到哪份文件之后）而不是绝对下标：
分类筛选后的列表只是全局序列的一段子序列，只有「排在 X 之后」在每一段子序列里都指向同一个位置。

---

## 六、排障

**页面打不开** —— 先看容器状态与日志，不要一上来就重建数据库：

```bash
docker compose ps
docker compose logs --tail=100 api
docker compose logs --tail=100 web
```

**接口失败但页面能开** —— 多半是 `api` 未通过健康检查。`docker compose logs api` 会给出原因。

**语义检索返回「向量服务暂不可用」** —— `embed` 边车还没就绪或已退出。
它启动时要装载 ONNX 模型，需要十几秒。先看 `docker compose logs embed`。
这个提示只影响语义检索，文件管理与关键词检索不受影响。

**文件上传成功但搜不到内容** —— 看列表的「索引状态」列。若是「索引失败」，
打开详情抽屉能看到失败原因，并点「重新索引」。原文件始终在，下载不受影响。
「仅存储」是 PDF 的当前状态，不是失败 —— PDF 正文索引尚未接上（见 §3.5）。

**删掉的文件想找回来** —— 文件列表上切到「回收站」标签页，点该行的「恢复」。
注意「彻底删除」与「清空回收站」是不可逆的，历史文件不会保留副本；
误删之后应尽快恢复，不要先清空回收站。

**容器重建后资料不见了** —— 核对是否误用了 `docker compose down -v`，
以及 compose 项目名是否被改动（改名会让应用连到另一组新卷上）。
先执行 `docker volume ls` 记录旧卷位置，不要急着删卷。

**首次构建卡住** —— 多为镜像或 npm 拉取慢，可配置国内镜像源后重试。

**想看某个请求到底发生了什么** —— 响应头里有 `X-Request-Id`，拿它去日志里对：

```bash
docker compose exec api grep '<requestId>' /data/logs/api.log
# 或者直接看文件：日志就在仓库下的 ./logs/api.log
```

浏览器里拿不到这个头时，服务端自己生成的 id 也在每一条访问日志里，
按 `slow=true` 或 `status=500` 筛一遍就能找到可疑的那次请求。

**想知道某个文件索引得怎么样** —— 用 `documentId` 跨两个文件 grep：

```bash
docker compose exec api sh -c 'grep "\"documentId\":10" /data/logs/*.log'
```

**日志文件里出现大片 NUL 或者看不见新内容** —— 多半是在容器还跑着的时候
从宿主机 `> logs/api.log` 截断了文件：容器仍按旧偏移追加，中间就留下一段空洞。
清日志要先把容器停掉（`docker compose stop api worker && rm logs/*.log`），
或者干脆用轮转（`LOG_MAX_SIZE_MB`），别手工截断。

---

## 七、目录结构

```text
.
├── cmd/server/            可执行入口，按 --mode 分角色
├── internal/
│   ├── config/            环境变量读取
│   ├── db/                数据库连接
│   ├── filekind/          格式清单与能力边界（收不收、抽不抽正文，两处共用一份）
│   ├── storage/           上传文件落盘（临时文件 + rename 原子替换）
│   ├── store/             数据访问：文档、分类、索引任务、检索
│   ├── indexer/           正文抽取、切片、向量边车客户端、任务消费者
│   ├── snippet/           命中片段的取窗（按字符，不按字节）
│   ├── logging/           zap 装配、字段词表、按角色落盘与轮转
│   ├── httpapi/           HTTP 路由、处理器与请求链路中间件
│   └── migrate/sql/       迁移文件，按文件名顺序执行
├── services/embed/        向量边车（Python + ONNX Runtime）
│   ├── model/             bge-small-zh-v1.5 的 int8 ONNX 产物
│   └── tools/             模型导出脚本
├── scripts/
│   ├── acceptance_api.py      接口层验收脚本（28 节，含回收站与 5 组语义检索示例的断言）
│   ├── clean_start_smoke.py   空库从零启动的冒烟（迁移 + 10 份语料走完整链路）
│   ├── semantic_examples.py   5 组自然语言检索示例的复现脚本
│   └── persistence_check.py   容器重建前后的数据比对（停机由人工执行）
├── web/                   Vue 3 + Vite + TypeScript 前端
│   ├── e2e/               Playwright 端到端用例（33 条）
│   └── Dockerfile         构建前端并打包进 nginx 镜像
├── testdata/corpus/       验收用测试文档（10 份）
├── docs/                  架构设计、检索示例
├── logs/                  落盘的日志（绑定挂载，不进仓库）：api / worker / migrate 各一份
├── SELF-CHECK.md          测试与验收说明
├── .golangci.yml          静态检查配置
├── docker-compose.yml
└── Dockerfile             api / worker / migrate 共用镜像
```

---

## 八、自测

系统起来之后，按下面的顺序跑：

```bash
gofmt -l . && go vet ./... && golangci-lint run ./...   # 静态检查
go test ./internal/...                                  # 单元测试（51 条）

python scripts/acceptance_api.py          # 接口层验收（274 条断言）
python scripts/semantic_examples.py --reset   # 5 组检索示例
cd web && npx playwright test             # 浏览器端到端（33 条）
```

上面这些都在**已有数据的卷**上跑。还有两件必须单独做的事：

```bash
# 空库从零启动：另起一套 compose 项目（端口 8091），不碰现有数据
# LOG_HOST_DIR 必须换一个：日志文件名按角色固定，两套栈写同一个 api.log 会让轮转互相踩
LOG_HOST_DIR=./logs-clean WEB_PORT=8091 docker compose -p kaoheclean up -d --build
python scripts/clean_start_smoke.py
LOG_HOST_DIR=./logs-clean docker compose -p kaoheclean down
docker volume rm kaoheclean_pgdata kaoheclean_uploads && rm -rf logs-clean

# 持久化演练：停机那一步必须由人工执行，所以拆成两段
python scripts/persistence_check.py snapshot
docker compose down && docker compose up -d      # 不要加 -v
python scripts/persistence_check.py verify
```

每一步的细节见 [`SELF-CHECK.md`](SELF-CHECK.md) §3.6 与 §3.7。

每一层盯的是什么、本次跑出来的数字、以及**哪些地方没有验到**，
都在 [`SELF-CHECK.md`](SELF-CHECK.md)。

---

## 九、开发里程碑

| 里程碑 | 内容 | 状态 |
| --- | --- | --- |
| M1 | 部署骨架、健康检查、一键启动 | ✅ 已完成 |
| M2 | 上传 / 下载 / 列表 / 详情 / 归档 / 恢复 | ✅ 已完成 |
| M3 | 分类树、标签与筛选 | ✅ 已完成 |
| M4 | 向量边车、切分与索引任务队列 | ✅ 已完成 |
| M5 | 关键词检索与语义检索 | ✅ 已完成 |
| M6 | 异常态与交互反馈打磨 | ✅ 已完成 |
| M7 | 交付文档、5 组检索示例与验证脚本 | ✅ 已完成 |
| M8 | 删除与回收站：文件软删除 / 恢复 / 彻底删除，分类删除入口常显 | ✅ 已完成 |
| M9 | 日志系统：zap 结构化日志、请求链路、按角色落盘与轮转 | ✅ 已完成 |
| M10 | 列表长按拖拽：调整全局顺序、拖入分类，位置落库并在刷新后保留 | ✅ 已完成 |

每一步都对应一次提交，提交信息里写明了该里程碑的取舍。
