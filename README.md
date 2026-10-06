# 文件管理与知识检索平台

解决文件分散、分类混乱、查找困难和归档后难以复用的问题。支持文件的上传、整理、检索、
下载与归档，并通过**真实的文本向量**提供语义检索。

> 当前进度：**M1 已完成** —— 部署骨架可一键启动，页面能显示后端与数据库的连通状态。
> 业务功能按里程碑推进，见文末「里程碑」。

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

## 二、验证部署是否成功

页面顶部显示「系统状态」，应看到**接口服务 ok / 数据库 ok**。
这一条链路同时穿过四层：浏览器 → nginx → api → PostgreSQL，能显示出来即说明
编排、反向代理与数据库迁移都已就绪。

也可以在命令行直接验证：

```bash
curl -s http://localhost:8080/api/v1/healthz
# {"db":"ok","service":"api","status":"ok","time":"..."}
```

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

`db`、`api`、`worker`、`embed` 均**不映射宿主机端口**，只在 compose 内网互通，
因此不存在与本机已有服务抢端口的问题。

---

## 四、服务构成

| 服务 | 说明 |
| --- | --- |
| `web` | nginx 托管前端静态资源，并把 `/api/` 反向代理到 `api` |
| `api` | Go + Gin，对外 HTTP 接口 |
| `worker` | 索引任务消费者（与 `api` 同一个镜像，`--mode=worker`） |
| `embed` | 文本向量生成边车（ONNX Runtime + BGE 中文模型） |
| `db` | PostgreSQL 16 + pgvector + pg_trgm |
| `migrate` | 一次性任务，建扩展、表与索引；成功后 `api` 才启动 |

`api`、`worker`、`migrate` 由**同一个镜像**以不同 `--mode` 启动，避免三份依赖版本漂移。

### 数据持久化

| 具名卷 | 内容 |
| --- | --- |
| `pgdata` | PostgreSQL 数据目录 |
| `uploads` | 上传的原始文件 |

容器重启或重新创建（`docker compose down && docker compose up`）后，数据仍然存在。

---

## 五、排障

**页面打不开** —— 先看容器状态与日志，不要一上来就重建数据库：

```bash
docker compose ps
docker compose logs --tail=100 api
docker compose logs --tail=100 web
```

**接口失败但页面能开** —— 多半是 `api` 未通过健康检查，`web` 因此没有启动。
`docker compose logs api` 会给出具体原因。

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
│   ├── storage/           上传文件落盘（临时文件 + rename 原子替换）
│   ├── store/             数据访问：文档、分类、索引任务
│   ├── httpapi/           HTTP 路由与处理器
│   └── migrate/           迁移执行器（SQL 经 go:embed 编入二进制）
│       └── sql/           迁移文件，按文件名顺序执行
├── scripts/
│   └── acceptance_api.py  接口层验收脚本
├── web/                   Vue 3 + Vite + TypeScript 前端
│   └── e2e/               Playwright 端到端用例
├── testdata/corpus/       验收用测试文档（10 份）
├── docker-compose.yml
└── Dockerfile
```

---

## 七、里程碑

| 里程碑 | 内容 | 状态 |
| --- | --- | --- |
| M1 | 部署骨架、健康检查、一键启动 | ✅ 已完成 |
| M2 | 上传 / 下载 / 列表 / 详情 / 归档 / 恢复 | ✅ 已完成 |
| M3 | 分类树、标签与筛选 | ✅ 已完成 |
| M4 | 向量边车、切分与索引任务队列 | 计划中 |
| M5 | 关键词检索与语义检索 | 计划中 |
| M6 | 异常态与交互反馈打磨 | 计划中 |
| M7 | 交付文档、5 组检索示例与验证脚本 | 计划中 |
