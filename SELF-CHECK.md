# 自测与验收说明

> 这份文档回答两个问题：**下面这些结论是怎么跑出来的**，以及**哪些地方没有验到**。
>
> - 启动步骤：[`README.md`](README.md)
> - 架构设计、组件来源与实现范围、关键取舍：[`docs/DESIGN.md`](docs/DESIGN.md)
> - 5 组自然语言检索示例：[`docs/RETRIEVAL-EXAMPLES.md`](docs/RETRIEVAL-EXAMPLES.md)

测试分六层：静态检查、Go 单元测试、接口层验收脚本、浏览器端到端用例、从零启动的冒烟、持久化演练。
每一层盯的东西不同，缺一层就会留下一种盲区 —— 具体是哪一种，见每节开头。

---

## 一、结论速览

| # | 验收项 | 结论 | 主要证据 |
| --- | --- | --- | --- |
| 1 | Docker 部署 | ✅ | `Dockerfile` + `docker-compose.yml`；`docker compose up --build` 多次从零起来过，本次记录见 §4.1 |
| 2 | 上传下载 | ✅ | 接口 §1/§2/§7 逐字节比对、§8 Range；浏览器 01-documents 11 条 |
| 3 | 分类调整 | ✅ | 接口 §13–§16；浏览器 02-categories 11 条，含 `page.reload()` 后的归属 |
| 4 | 关键词搜索 | ✅ | 接口 §24；浏览器 03-search 5 条 |
| 5 | 语义检索 | ✅ | 接口 §25；5 组示例 5/5；向量生成→存储→检索全链路见 §4.5 |
| 6 | 归档恢复 | ✅ | 接口 §9（列表）+ §27（检索）；浏览器 01-documents |
| 7 | 数据持久化 | ✅ | `scripts/persistence_check.py`，容器重建前后 PASS=18 FAIL=0 |
| 8 | 异常处理 | ✅ | 接口 §12/§20/§21/§22/§26；浏览器 01-documents 的失败与重试用例 |
| 9 | 操作体验 | ⚠️ 部分 | 流程连贯性与空/错/加载态有用例覆盖；**多种屏幕尺寸没有自动化用例**，见 §6 |

本次全部记录：

| 层 | 命令 | 结果 |
| --- | --- | --- |
| 静态检查 | `gofmt -l .` / `go vet ./...` / `golangci-lint run ./...` | 干净 / 干净 / 0 issues |
| Go 单元测试 | `go test ./internal/...` | 25 条全绿（filekind 5、indexer 14、snippet 6） |
| Go 竞态检测 | `go test -race ./...` | 全绿（在 Linux 容器里跑，见 §3.2） |
| 接口层验收 | `python scripts/acceptance_api.py` | **PASS=238 FAIL=0**，27 节 |
| 浏览器端到端 | `cd web && npx playwright test` | **27 passed** |
| 语义检索示例 | `python scripts/semantic_examples.py --reset` | **5 例中 5 例**首位命中与预期一致 |
| 从零启动的冒烟 | `python scripts/clean_start_smoke.py` | **PASS=25 FAIL=0**（空库上从头跑迁移） |
| 持久化演练 | `python scripts/persistence_check.py verify` | **PASS=18 FAIL=0** |

---

## 二、怎么把系统跑起来

```bash
cp .env.example .env      # 至少改掉 POSTGRES_PASSWORD
docker compose up --build
```

等日志出现 `api 已启动`，浏览器打开 <http://localhost:8080>。

> ⚠️ 之后如果需要重启，用 `docker compose down` 或 `docker compose restart`。
> **不要用 `docker compose down -v`** —— `-v` 会删掉具名卷，文件、分类、归档状态全部丢失，
> 重建镜像也救不回来。

下面所有脚本都在仓库根目录执行，并且要求系统已经起来。

> **这几层共用同一套数据，必须一层跑完再跑下一层，不要并行。**
> 接口验收、浏览器端到端、检索示例都会清空并重建业务数据，
> 同时跑会出现「一边在读、一边被清」的假失败。
> 界面上顺手归档一份文件也同理 —— 它会改变检索的命中数。

---

## 三、五层测试怎么跑

### 3.1 静态检查

```bash
gofmt -l .                    # 应当没有输出
go vet ./...
golangci-lint run ./...       # 需要 v2 及以上
```

`golangci-lint` 的检查项写在仓库根目录的 `.golangci.yml` 里，
其中有一条 errcheck 豁免（收尾型的 `Close` 不判错）附了理由，改动前请先读那段注释。

### 3.2 Go 单元测试

```bash
go test ./internal/...

# 竞态检测。本机的 Windows 工具链跑不起来（退出码 0xc0000139，
# race 运行时与 MinGW 不匹配），用 Linux 容器跑：
docker run --rm -v "$PWD:/src" -w /src -e CGO_ENABLED=1 golang:1.27-alpine \
  sh -c 'apk add --no-cache gcc musl-dev && go test -race ./...'
```

覆盖的是三块**纯函数 + 一处并发**，都是逻辑密度高、出错了却不会报错只会算错的地方：

| 包 | 测什么 |
| --- | --- |
| `internal/snippet` | 命中片段的取窗：按字符不按字节、大小写折叠、省略号只在真截断时加 |
| `internal/indexer` | 切片不丢字不超上限、行边界优先、重叠尾巴；向量边车客户端的维度与错误处理 |
| `internal/filekind` | 格式清单大小写不敏感，以及「收不收」与「抽不抽正文」两处判断用的是同一份数据 |

`internal/indexer/embed_test.go` 里的 `TestEmbedderIsSafeForConcurrentUse` 值得单独说一句：
它**不带 `-race` 跑也会通过**，因为它盯的是数据竞争，而竞争平时看不出症状。
这条用例做过反向验证 —— 把 `Embedder.dimension` 改回普通 `int`，`go test -race` 会在这里报
DATA RACE。所以改那块代码时请连带 `-race` 一起跑。

### 3.3 接口层验收（`scripts/acceptance_api.py`）

```bash
docker compose up -d
python scripts/acceptance_api.py            # 会先清空业务数据再开始
python scripts/acceptance_api.py --keep     # 保留现有数据，只跑断言
```

27 节、238 条断言，逐条对着验收条件写。用 Python 标准库手拼 multipart 而不用 `curl`
是有原因的，理由写在脚本文件头：Windows 上的 `curl.exe` 会按本地代码页转码命令行参数，
用它测中文文件名和中文标签会得出「服务端乱码」的错误结论。

这一层跑完会把库里的数据弄得很脏（归档、改名、重试各留一份副本），
**要接着跑 §3.5 的示例请先 `--reset`**，理由见 `docs/RETRIEVAL-EXAMPLES.md`。

### 3.4 浏览器端到端（Playwright）

```bash
cd web
npx playwright test            # 27 条
npx playwright test --ui       # 想看过程时用
```

用的是系统已装的 Edge（`channel: 'msedge'`），不需要额外下载浏览器。
`workers: 1`、`fullyParallel: false`：用例之间有共享的服务端状态，并行跑会互相干扰。

三个 spec 分别对应文件、分类、检索三条主线。
注意 `npm run build` **不做类型检查**，类型检查是单独一条 `npm run type-check`。

### 3.5 5 组自然语言检索示例（`scripts/semantic_examples.py`）

```bash
python scripts/semantic_examples.py --reset
python scripts/semantic_examples.py --reset --json docs/semantic-examples.json
```

`--reset` 会清空业务数据、重新灌入 10 份语料、等索引完成，再跑 5 组提问。
必须重置才能复现交付文档里那张表，原因见 `docs/RETRIEVAL-EXAMPLES.md` 最后一节。

### 3.6 从零启动的冒烟测试

前面几层都是在一套已经存在的具名卷上跑的，也就是说**迁移文件从来没在一个空库上从头跑过**。
验收条件第一条却是「按 README 完成必要配置后 `docker compose up --build` 启动系统」，
所以这一步必须单独验。

用另一个 compose 项目名起一套干净的栈，端口也换掉 —— 这样不会碰到现在这套数据，
也不用冒着删卷的风险去验：

```bash
WEB_PORT=8091 docker compose -p kaoheclean up -d --build

# 对着 8091 跑一次冒烟：健康接口 → 校验迁移与迁移文件一一对应 → 确认起点是空库 →
# 上传 10 份语料 → 等索引 → 关键词检索 → 语义检索 → 下载逐字节比对 → 建分类 / 移动 / 归档
python scripts/clean_start_smoke.py

# 验完拆掉，注意只删这个项目的卷
docker compose -p kaoheclean down
docker volume rm kaoheclean_pgdata kaoheclean_uploads
```

`--project`（compose 项目名）与 `--base`（接口地址）是配对的，默认值就是上面这一对。
两处必须指向同一套栈：脚本要清空数据、要读 `schema_migrations`，这两件事都不在接口里，
只能进容器做。配错会变成「读 A 栈、清 B 栈」。

两个容易踩的地方，脚本里都写了注释：索引成功的终态是 `ready` 而不是 `indexed`；
PDF 的状态是 `not_supported`（仅存储）而不是 `failed`。

**本次记录**：`migrate` 在空库上依次应用了 `0001_init.sql` 与 `0002_category_no_self_parent.sql`
（`applied=2`），五个服务全部 healthy；冒烟 **PASS=25 FAIL=0** ——
迁移记录与迁移文件一致，10 份文件全部落位，7 份 `ready`、3 份 `not_supported`，
两种检索都有结果且语义检索首位命中 `01_代码提交规范_v2.1.md`，
下载 sha256 与原文件一致，建分类 / 移动 / 归档后默认列表与归档区计数都对。

### 3.7 持久化演练（`scripts/persistence_check.py`）

这条验收条件没法一个脚本跑完，因为中间那一步停机必须在脚本之外发生 ——
脚本依赖的服务正好就是被停掉的那个。所以拆成两段：

```bash
# 先在界面上传几份文件、建几个分类、归档一份，然后：
python scripts/persistence_check.py snapshot

docker compose down            # 注意：不要加 -v
docker compose up -d

python scripts/persistence_check.py verify
```

`verify` 逐项比对停机前后的：文件清单（含每份文件的 sha256 与原文件逐字节比对）、
分类树、归档集合、关键词检索结果（含顺序）、语义检索首位与相似度，
并检查挂载的具名卷确实是同一份。挂到新卷上等于数据从零开始，
那种「没报错但东西没了」是最难查的一种。

---

## 四、验收条件逐条对照

### 4.1 Docker 部署

> 提供 Dockerfile 和 Docker Compose 配置，按 README 完成必要配置后，可通过 `docker compose up --build` 启动系统

`Dockerfile` 一条多阶段构建，产出 `api` / `worker` / `migrate` 三个角色共用的镜像
（同一个二进制，靠 `--mode` 区分），避免三份依赖版本各自漂移。
`web` 用的是独立的 `web/Dockerfile`（Node 构建 → nginx 托管）。

`.env.example` 里列了全部必填项，`docker-compose.yml` 用 `${POSTGRES_PASSWORD:?未设置：请先执行 cp .env.example .env}`
这种写法，缺了它会直接报出该怎么办，而不是起来一个连不上数据库的容器。

只有 `web` 映射宿主机端口，`db` / `api` / `worker` / `embed` 都只在 compose 内网互通，
不会跟本机已有服务抢端口。

**本次记录**：`docker compose build api` → `docker compose up -d`，五个服务全部起来，
`api` 与 `db` 健康检查为 `healthy`。

这一条的重点是**空库**：在既有的卷上起得来，不代表迁移语句在一个从没跑过的库上也对。
所以另外用 `-p kaoheclean` 起了一套干净的栈（端口 8091，不碰现有数据），
在空库上从头跑完迁移再冒烟一遍 —— 见 §3.6。

### 4.2 上传下载

> PDF、TXT、Markdown 均可上传和下载，下载内容与原文件一致

「下载内容与原文件一致」是逐字节比的，不是比大小 ——
接口 §7 与持久化演练里都对下载回来的字节算了 sha256。
PDF 的下载另外验了 Range 请求（§8），因为在线预览依赖它。

**本次记录**：接口 §1/§2/§7/§8 全部 PASS；浏览器 01-documents 11 条 PASS。

### 4.3 分类调整

> 新建分类、移动文件后，列表和筛选结果正确，刷新后仍保留

接口 §13–§16 覆盖建树、归属、移动、删除；其中删除分类用 `ON DELETE SET NULL`，
文件转为「未分类」而不是被一起删掉。

「刷新后仍保留」这一半是浏览器用例 `02-categories.spec.ts` 里那条
「刷新后分类结构与文件归属都还在」验的，它真的调了 `page.reload()` ——
前端如果把状态放在内存里，这条会失败。

**本次记录**：接口 §13–§16 全部 PASS；浏览器 02-categories 11 条 PASS。

### 4.4 关键词搜索

> 能通过文件名及 TXT／Markdown 正文关键词找到目标文件，分类筛选同时生效

实现是 pg_trgm 的 GIN 三元组索引 + `ILIKE`，中文不需要分词。
这一节的断言是**行为式**的，不写死命中份数：语料里提到过某个词的文档会随语料变化，
而这一节要验的是「文件名命中排在前面」「正文命中带得出片段」这两条排序规则。

空结果、空关键词（400）、超长关键词（400）、非法分类筛选（400）也一并验了。

**本次记录**：接口 §24 全部 PASS；浏览器 03-search 5 条 PASS。

### 4.5 语义检索

> 使用与原文措辞不同、含义相关的问题，能返回相关文档及片段；代码中存在真实的向量生成、存储和检索流程

先说「真实的向量生成、存储和检索」这一句，因为它最容易被糊过去。链路是：

| 环节 | 在哪 | 做什么 |
| --- | --- | --- |
| 生成 | `services/embed/app.py` | ONNX Runtime 跑 bge-small-zh-v1.5（int8），CLS 池化 + L2 归一化，输出 512 维 |
| 生成 | `internal/indexer/embed.go` | Go 侧的边车客户端，分批调用、校验维度与条数 |
| 存储 | `internal/store/jobs.go` + `internal/migrate/sql/` | worker 领任务，写入 `document_chunks.embedding vector(512)`，建 HNSW `vector_cosine_ops` 索引 |
| 检索 | `internal/store/search.go` | 用 `<=>` 算余弦距离，按片段打分、按文档聚合 |

没有调用任何托管检索服务；模型是本地加载的（`services/embed/model/model.onnx`，24 MB）。

再说「措辞不同」。5 组示例都刻意满足同一个条件：**把整句话原样丢进关键词检索，命中数为 0**。
完整对照表与原文片段见 [`docs/RETRIEVAL-EXAMPLES.md`](docs/RETRIEVAL-EXAMPLES.md)。

相似度下限 `SEMANTIC_MIN_SCORE` 默认 `0.5`，是按当前模型在验收语料上标定出来的：
相关结果的首位相似度在 0.618~0.717，语料里没有对应内容时的最高分在 0.44~0.47，
0.5 落在这段空档里。标定过程与换模型后必须重新标定的原因见 `docs/DESIGN.md` §4.3。

**本次记录**：接口 §25 全部 PASS；`semantic_examples.py --reset` 5/5；
浏览器 03-search 的语义用例先证明关键词 0 命中、再切到语义标签页搜到目标文件。

### 4.6 归档恢复

> 归档文件不出现在默认列表和检索结果中，在归档区仍可找到；恢复后可重新检索和下载

这条件有两半，分在两节里验：§9 验列表那一半，§27 验检索那一半 ——
只验列表的话，「归档了但还能搜到」这种最影响使用的漏法不会被发现。
§27 里归档之后同时查了关键词检索与语义检索，再确认归档区找得到、下载仍然逐字节一致。

**本次记录**：接口 §9、§27 全部 PASS；浏览器 01-documents 的归档用例 PASS。

### 4.7 数据持久化

> 容器重启或重新创建后，文件、分类和归档状态仍存在，检索功能正常；部署配置应正确挂载持久化存储

两个具名卷 `pgdata` 与 `uploads`；`docker compose down` 不删具名卷，所以重建容器后数据还在。
「部署配置应正确挂载持久化存储」是**检查实际挂载**，不是读 compose 文件——
`persistence_check.py` 用 `docker inspect` 取出容器真正挂的卷名，并确认停机前后是同一份。

**本次记录**：演练前 10 份文件（其中 1 份归档）、2 个分类；
`docker compose down` → `up -d` 后 api 容器 ID 从 `8384e268…` 变成 `d2bc472d…`（确实重建了），
比对结果 **PASS=18 FAIL=0**，逐字段含 sha256 全部一致。

### 4.8 异常处理

> 对超限文件、上传失败、索引失败和无搜索结果提供明确反馈，允许合理重试

| 情形 | 反馈 | 验在哪 |
| --- | --- | --- |
| 超限文件 | 413 + 说清上限是多少 | 接口 §12 |
| 不支持的格式 | 400 + 列出支持哪些 | 接口 §3；浏览器 01-documents |
| 上传失败 | 错误原因可见，可重试 | 接口 §12；浏览器 01-documents |
| 索引失败 | `failed` 状态 + 错误原因 + 「重新索引」入口 | 接口 §20/§21/§22；浏览器 01-documents |
| 无搜索结果 | 与「还没搜过」区分开的空态 | 接口 §24；浏览器 01-documents、03-search |
| 向量边车不可用 | 语义检索 503，其余功能不受影响 | 接口 §26 |

索引失败不影响原文件的保存与下载：§21 专门断言失败之后原文件仍能下载且逐字节一致。
重试是有限次的（`max_attempts`），退避 15s/30s，超过上限才转 `failed` ——
无限重试会把一个必然失败的任务变成永久占用。

### 4.9 操作体验

> 上传、整理、查找和下载流程连贯，主要入口清晰，常见屏幕尺寸下无明显布局问题

**验到的**：上传有进度条；列表页有加载态、空态、错误态三种区分；
只要还有文件停在待索引/索引中，列表每 1.5 秒静默刷新，全部落定后自动停下；
索引状态用不同标签与颜色区分「已索引 / 索引失败 / 仅存储」，
`not_supported` 是正常状态而非失败，详情页也不会给它「重新索引」按钮 ——
给一个按了也不会成功的按钮比不给更糟。

**没验到的**：多种屏幕尺寸。布局是按响应式写的（栅格 + 断点），
但**没有针对不同视口跑过自动化用例**，只在开发时手工拖过窗口。见 §6。

---

## 五、功能要求对照

出题方的 5 条功能要求与实现位置：

| 功能要求 | 实现 |
| --- | --- |
| 1. 分类管理 | 目录树（递归 CTE，选父分类能筛到子分类）+ 标签，一份文件一个主归属 |
| 2. 文件管理 | 上传/列表/详情/下载/归档/恢复；PDF、TXT、Markdown；列表展示名称、分类、大小、上传时间；上传有进度 |
| 3. 关键词检索 | pg_trgm 索引 + `ILIKE`，文件名与正文都可搜，结果带命中片段；与分类筛选同时生效 |
| 4. 语义检索 | 真实向量链路（见 §4.5）；覆盖 TXT 与 Markdown；结果展示来源文件与相关片段；索引状态可见、失败可重试、失败不影响原文件 |
| 5. 持久化与交互 | 原文件落具名卷、元信息在数据库；页面有加载/空/成功/失败四种反馈 |

可选扩展里做了 **PDF 在线预览**（`?inline=true` + Range 请求，配合浏览器内置 PDF 阅读器）。
其余三项（版本管理、知识库问答、外部项目关联）没有做，理由见 `docs/DESIGN.md` §5。

---

## 六、没有验证到的部分

按「如果有人照着这份文档验收，会踩到什么」的顺序列：

1. **多种屏幕尺寸没有自动化用例**。响应式是按栅格写的，但只手工拖过窗口。
   如果验收方在平板或窄屏上打开发现布局问题，属于我没有验到的地方。
2. **并发上传没有压力测试**。接口验收是串行的。多用户同时上传时，
   `FOR UPDATE SKIP LOCKED` 的任务领取与部分唯一索引在数据库层是有保证的，
   但没有用并发用例实测过。
3. **`-race` 只在 Linux 容器里跑过**，本机 Windows 工具链跑不起来（0xc0000139）。
   交付的镜像是 Linux，所以这不算盲区；但如果有人在 Windows 上直接 `go test -race`，会失败。
4. **语义检索的相关性只用 5 组示例衡量过**。没有标注数据集，没有 recall/precision 指标，
   阈值是按这 5 组正例加一批无对应内容的提问标定出来的。
5. **PDF 正文检索不到**，这是设计取舍不是缺陷（`docs/DESIGN.md` §3.5）。
   需要说明的是，验收语料里那 3 份 PDF 用 `pdftotext` 也只能提取出 77~104 个非空白字符，
   中文一个都取不到 —— 它们的内嵌字体缺少 Unicode 映射，即使接上解析库也搜不到。
6. **没有身份认证与权限**。任何能访问端口的人都能读写全部文件，见 `docs/DESIGN.md` §5 第 1 条。
7. **`scripts/persistence_check.py` 需要人工在两个脚本之间执行停机**。
   停机这件事应该由人看着发生，所以刻意没有做成一个脚本自动跑完。
8. **浏览器用例用的是系统 Edge（`channel: 'msedge'`）**，只在 Edge 上跑过，没有跨浏览器矩阵。
