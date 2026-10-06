# 文件管理与知识检索平台

解决文件分散、分类混乱、查找困难和归档后难以复用的问题。
支持文件的**上传、整理、检索、下载、归档与删除**，并通过**真实的文本向量**提供语义检索。

---

## 快速开始

前置条件：Docker Desktop（含 Docker Compose v2），建议分配 4 GB 以上内存。

```bash
git clone <仓库地址> && cd KaoHe

cp .env.example .env
# 打开 .env，至少把 POSTGRES_PASSWORD 改成自己的口令

docker compose up --build
```

首次构建需要编译 Go、构建前端并拉取镜像，约需数分钟。待日志出现 `api 已启动` 后，
浏览器打开 <http://localhost:8080>。若 8080 被占用，改 `.env` 里的 `WEB_PORT` 即可。

```bash
docker compose down       # 停止
```

> ⚠️ **不要使用 `docker compose down -v`。**
> `-v` 会连同具名卷一起删除，文件、分类与归档状态将全部丢失，且无法通过重建镜像恢复。

更完整的说明 —— 配置项、服务构成、接口一览、排障、目录结构、自测入口 —— 见 [`docs/STARTUP.md`](docs/STARTUP.md)。

---

## 交付文档

| 文档 | 内容 |
| --- | --- |
| [`docs/STARTUP.md`](docs/STARTUP.md) | 启动说明：前置条件、启动与停止、功能走查、配置项、服务构成、接口一览、排障、目录结构、自测入口 |
| [`docs/TESTING.md`](docs/TESTING.md) | 测试与验收说明：六层测试怎么跑、验收条件逐条对照、没有验证到的部分 |
| [`docs/DESIGN.md`](docs/DESIGN.md) | 架构设计、组件来源与实现范围、关键取舍（§3，16 节）、已知限制（§5，16 条） |
| [`docs/RETRIEVAL-EXAMPLES.md`](docs/RETRIEVAL-EXAMPLES.md) | 5 组自然语言检索示例，含预期命中的文件与复现命令 |

---

## 服务构成

| 服务 | 说明 |
| --- | --- |
| `web` | nginx 托管前端静态资源，并把 `/api/` 反向代理到 `api` |
| `api` | Go + Gin，对外 HTTP 接口 |
| `worker` | 索引任务消费者（与 `api` 同一个镜像，`--mode=worker`） |
| `embed` | 文本向量边车（ONNX Runtime + BGE 中文模型），只提供 `POST /embed` |
| `db` | PostgreSQL 16 + pgvector + pg_trgm |
| `migrate` | 一次性任务，建扩展、表与索引；成功后 `api` 才启动 |

仅 `web` 映射宿主机端口（`WEB_PORT`，默认 8080），其余服务只在 compose 内网互通。
数据落在 `pgdata` 与 `uploads` 两个具名卷上，容器重建不影响数据。

---

## 已知边界

- **PDF 只做存储与在线预览**，正文索引是待补的功能而非设计取舍 ——
  验收语料的 PDF 正文实际取得到，见 [`docs/DESIGN.md`](docs/DESIGN.md) §3.5 的更正。
- **没有身份认证与权限**：任何能访问该端口的人都能读写全部文件。
- 其余没有验证到的部分集中在 [`docs/TESTING.md`](docs/TESTING.md) §6。
