#!/usr/bin/env python3
"""从零启动的冒烟测试：对着一套全新的栈跑一遍最短的完整链路。

为什么需要它
------------
`acceptance_api.py` 验的是「各项功能对不对」，它跑在一套已经存在数据的系统上。
但验收条件第一条是「按 README 完成必要配置后，`docker compose up --build` 启动系统」——
那条路径要回答的是另一个问题：**数据库是空的、迁移从来没跑过的时候，系统能不能自己长出来。**

两者的盲区不一样。前者的盲区是「空库时某个迁移语句就错」，后者不会发现。

跑法
----
用另一个 compose 项目名起一套干净的栈，端口也换掉，这样不会碰到现在这套数据：

    WEB_PORT=8091 docker compose -p kaoheclean up -d --build
    python scripts/clean_start_smoke.py
    docker compose -p kaoheclean down
    docker volume rm kaoheclean_pgdata kaoheclean_uploads

项目名与接口地址是配对的（`--project` 与 `--base`），两边必须指向同一套栈：
脚本要清空数据、要查迁移记录，这两件事不存在接口里，只能进容器做。
配错的话会变成「读 A 栈、清 B 栈」——所以默认值就是上面这一对。

脚本开头会清空业务数据，所以对着一套已经在跑的系统重复执行也是安全的
（只会把数据清掉，不会破坏结构）——但要清楚它会清数据，别对着正式环境跑。
"""

from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

import acceptance_api as api

REPO_ROOT = api.REPO_ROOT
MIGRATIONS = REPO_ROOT / "internal" / "migrate" / "sql"

SEMANTIC_QUERY = "合并代码之前需要做什么检查"
SEMANTIC_EXPECTED = "01_代码提交规范_v2.1.md"
KEYWORD_QUERY = "发布检查清单"


def web_health(base: str) -> dict:
    """打接口服务里的健康检查。走的是对外那个端口，因此顺带验了反向代理。"""
    with urllib.request.urlopen(base + "/healthz", timeout=10) as resp:
        return json.loads(resp.read())


def wait_indexed(doc_ids: set[str], timeout: float = 120.0) -> dict[str, str]:
    """等所有文档的索引状态落定，返回 {docUID: indexStatus}。"""
    deadline = time.monotonic() + timeout
    settled: dict[str, str] = {}
    while time.monotonic() < deadline:
        _, listing, _ = api.call("GET", "/documents?pageSize=200")
        settled = {d["id"]: d["indexStatus"] for d in listing["items"]}
        pending = {i for i, st in settled.items() if st in ("pending", "processing")}
        if not pending & doc_ids:
            return settled
        time.sleep(0.4)
    return settled


def main() -> int:
    parser = argparse.ArgumentParser(description="从零启动的冒烟测试")
    parser.add_argument("--base", default="http://127.0.0.1:8091/api/v1")
    parser.add_argument(
        "--project", default="kaoheclean",
        help="compose 项目名。清空数据、查迁移记录都打在这个项目上，"
             "必须与 --base 指向的是同一套栈，否则会一边读一套一边清另一套",
    )
    args = parser.parse_args()
    api.BASE_URL = args.base
    api.COMPOSE_ARGS = ["-p", args.project]

    try:
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    except (AttributeError, OSError):
        pass

    print("== 1. 服务本身是活的 ==")
    try:
        health = web_health(args.base)
    except (urllib.error.URLError, OSError) as exc:
        print(f"  连不上 {args.base}：{exc}", file=sys.stderr)
        print("  先把栈起起来：WEB_PORT=8091 docker compose -p kaoheclean up -d --build", file=sys.stderr)
        return 2
    api.chk("状态为 ok", health.get("status"), "ok")
    api.chk("数据库连通", health.get("db"), "ok")

    print("== 2. 迁移是从头跑过的 ==")
    # 迁移文件名与 schema_migrations 里记的必须一一对应：
    # 少一个说明这个迁移被跳过了，多一个说明库里留着代码里已经没有的版本。
    expected = sorted(p.name for p in MIGRATIONS.glob("*.sql"))
    applied = sorted(api.sql("SELECT version FROM schema_migrations").splitlines())
    api.chk("已应用的迁移与迁移文件一致", applied, expected)

    print("== 3. 起点是空库 ==")
    api.reset()
    _, listing, _ = api.call("GET", "/documents?pageSize=200")
    api.chk("没有文件", listing["total"], 0)
    api.chk("没有分类", len(api.call("GET", "/categories")[1]["items"]), 0)

    print("== 4. 灌入 10 份语料 ==")
    ids: dict[str, str] = {}
    for name in api.TEXT_CORPUS + api.PDF_CORPUS:
        status, doc, _ = api.upload(name)
        api.chk(f"上传 {name}", status, 201)
        ids[name] = doc["id"]

    print("== 5. 索引流水线在空库上能跑通 ==")
    settled = wait_indexed(set(ids.values()))
    ready = sorted(n for n in ids if settled.get(ids[n]) == "ready")
    skipped = sorted(n for n in ids if settled.get(ids[n]) == "not_supported")
    api.chk("可抽取正文的 7 份都已索引", ready, sorted(api.TEXT_CORPUS))
    api.chk("3 份 PDF 都是仅存储（不是失败）", skipped, sorted(api.PDF_CORPUS))

    print("== 6. 两种检索都可用 ==")
    _, keyword, _ = api.call("GET", "/search?q=" + urllib.parse.quote(KEYWORD_QUERY))
    api.chk("关键词检索有结果", keyword["total"] > 0, True)
    _, semantic, _ = api.call(
        "POST", "/search/semantic", json_body={"query": SEMANTIC_QUERY, "topK": 5}
    )
    top = semantic["items"][0]["document"]["name"] if semantic["items"] else None
    api.chk("语义检索首位命中预期文件", top, SEMANTIC_EXPECTED)

    print("== 7. 下载逐字节一致 ==")
    name = api.MD
    _, blob, _ = api.call("GET", f"/documents/{ids[name]}/download")
    api.chk(
        "sha256 与原文件相同",
        hashlib.sha256(blob).hexdigest(),
        hashlib.sha256((api.CORPUS / name).read_bytes()).hexdigest(),
    )

    print("== 8. 分类与归档 ==")
    status, category, _ = api.call("POST", "/categories", json_body={"name": "冒烟分类"})
    api.chk("新建分类", status, 201)
    api.chk(
        "移动文件到新分类",
        api.call("PATCH", f"/documents/{ids[name]}", json_body={"categoryId": category["id"]})[0],
        200,
    )
    api.chk(
        "归档",
        api.call("PATCH", f"/documents/{ids[name]}", json_body={"archived": True})[0],
        200,
    )
    total, archived = len(ids), 1
    api.chk("默认列表少一份", api.call("GET", "/documents?pageSize=200")[1]["total"], total - archived)
    api.chk("归档区有一份", api.call("GET", "/documents?archived=true")[1]["total"], archived)

    print(f"\n结果：PASS={api.PASS} FAIL={api.FAIL}")
    return 1 if api.FAIL else 0


if __name__ == "__main__":
    sys.exit(main())
