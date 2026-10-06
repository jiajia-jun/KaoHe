# 文件管理与知识检索平台

解决文件分散、分类混乱、查找困难和归档后难以复用的问题。
支持文件的**上传、整理、检索、下载与归档**，并通过**真实的文本向量**提供语义检索。

> 交付物索引：架构设计与关键取舍见 [`docs/DESIGN.md`](docs/DESIGN.md)，
> 测试与验收说明见 [`SELF-CHECK.md`](SELF-CHECK.md)，
> 5 组自然语言检索示例见 [`docs/RETRIEVAL-EXAMPLES.md`](docs/RETRIEVAL-EXAMPLES.md)。

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

---

## 二、五分钟走一遍

| 步骤 | 在哪里 | 做什么 |
| --- | --- | --- |
| 1 | 浏览器 →「系统状态」 | 应看到**接口服务 ok / 数据库 ok**，说明四层链路（浏览器 → nginx → api → PostgreSQL）已通 |
| 2 | 文件管理（首页） | 点右上角**上传文件**，把 `testdata/corpus/` 里的文档拖进去。上传时有进度条 |
| 3 | 文件管理 | 看**索引状态**列：Markdown/TXT 会从「待索引」走到「已索引」，PDF 显示「仅存储」 |
| 4 | 左侧分类树 | 点「新建」建几个分类，把文件拖到分类上，或打开详情抽屉改归属 |
| 5 | 知识检索 →「关键词检索」 | 搜 `发布检查清单`：文件名命中的排在前面，正文提到的也带出片段 |
| 6 | 知识检索 →「语义检索」 | 输入 `容器起来了但是服务还是用不了`（整句在关键词检索里 0 命中） |
| 7 | 文件管理 | 把某个文件**归档**，它会从默认列表消失，切到「已归档」标签页还能找到 |

第 6 步是这套系统最值得看的一处：**用与原文完全不同的措辞，找到对应的文档和片段。**
完整的 5 组示例（含预期命中的文件）见 [`docs/RETRIEVAL-EXAMPLES.md`](docs/RETRIEVAL-EXAMPLES.md)。

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

`db`、`api`、`worker`、`embed` 均**不映射宿主机端口**，只在 compose 内网互通，
因此不存在与本机已有服务抢端口的问题。

`SEMANTIC_MIN_SCORE` 的值是按当前向量模型在验收语料上标定出来的，
换模型后必须重新标定 —— 标定方法与实测数据见 `docs/DESIGN.md` §4.3。

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

## 五、排障

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

**容器重建后资料不见了** —— 核对是否误用了 `docker compose down -v`，
以及 compose 项目名是否被改动（改名会让应用连到另一组新卷上）。
先执行 `docker volume ls` 记录旧卷位置，不要急着删卷。

**首次构建卡住** —— 多为镜像或 npm 拉取慢，可配置国内镜像源后重试。

---

## 六、目录结构

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
│   ├── httpapi/           HTTP 路由与处理器
│   └── migrate/sql/       迁移文件，按文件名顺序执行
├── services/embed/        向量边车（Python + ONNX Runtime）
│   ├── model/             bge-small-zh-v1.5 的 int8 ONNX 产物
│   └── tools/             模型导出脚本
├── scripts/
│   ├── acceptance_api.py      接口层验收脚本（27 节，含 5 组语义检索示例的断言）
│   ├── clean_start_smoke.py   空库从零启动的冒烟（迁移 + 10 份语料走完整链路）
│   ├── semantic_examples.py   5 组自然语言检索示例的复现脚本
│   └── persistence_check.py   容器重建前后的数据比对（停机由人工执行）
├── web/                   Vue 3 + Vite + TypeScript 前端
│   ├── e2e/               Playwright 端到端用例（27 条）
│   └── Dockerfile         构建前端并打包进 nginx 镜像
├── testdata/corpus/       验收用测试文档（10 份）
├── docs/                  架构设计、检索示例
├── SELF-CHECK.md          测试与验收说明
├── .golangci.yml          静态检查配置
├── docker-compose.yml
└── Dockerfile             api / worker / migrate 共用镜像
```

---

## 七、自测

系统起来之后，按下面的顺序跑：

```bash
gofmt -l . && go vet ./... && golangci-lint run ./...   # 静态检查
go test ./internal/...                                  # 单元测试（25 条）

python scripts/acceptance_api.py          # 接口层验收（238 条断言）
python scripts/semantic_examples.py --reset   # 5 组检索示例
cd web && npx playwright test             # 浏览器端到端（27 条）
```

上面这些都在**已有数据的卷**上跑。还有两件必须单独做的事：

```bash
# 空库从零启动：另起一套 compose 项目（端口 8091），不碰现有数据
WEB_PORT=8091 docker compose -p kaoheclean up -d --build
python scripts/clean_start_smoke.py
docker compose -p kaoheclean down && docker volume rm kaoheclean_pgdata kaoheclean_uploads

# 持久化演练：停机那一步必须由人工执行，所以拆成两段
python scripts/persistence_check.py snapshot
docker compose down && docker compose up -d      # 不要加 -v
python scripts/persistence_check.py verify
```

每一步的细节见 [`SELF-CHECK.md`](SELF-CHECK.md) §3.6 与 §3.7。

每一层盯的是什么、本次跑出来的数字、以及**哪些地方没有验到**，
都在 [`SELF-CHECK.md`](SELF-CHECK.md)。

---

## 八、开发里程碑

| 里程碑 | 内容 | 状态 |
| --- | --- | --- |
| M1 | 部署骨架、健康检查、一键启动 | ✅ 已完成 |
| M2 | 上传 / 下载 / 列表 / 详情 / 归档 / 恢复 | ✅ 已完成 |
| M3 | 分类树、标签与筛选 | ✅ 已完成 |
| M4 | 向量边车、切分与索引任务队列 | ✅ 已完成 |
| M5 | 关键词检索与语义检索 | ✅ 已完成 |
| M6 | 异常态与交互反馈打磨 | ✅ 已完成 |
| M7 | 交付文档、5 组检索示例与验证脚本 | ✅ 已完成 |

每一步都对应一次提交，提交信息里写明了该里程碑的取舍。
