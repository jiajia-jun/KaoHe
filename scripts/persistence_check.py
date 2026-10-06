#!/usr/bin/env python3
"""数据持久化演练：容器重建前后各取一次快照，逐项比对。

为什么单独成一个脚本，而不是并进 acceptance_api.py
--------------------------------------------------
验收条件里的「容器重启或重新创建后，文件、分类和归档状态仍存在，检索功能正常」
没法在一个脚本里端到端跑完：中间那一步 `docker compose down` 必须在脚本之外发生 ——
脚本自己依赖的服务正好就是被停掉的那个，而且要停机这件事应该由人看着它发生。

所以拆成两段，中间那次停机由操作者执行：

    python scripts/persistence_check.py snapshot     # 停机前
    docker compose down                              # 注意：不要加 -v
    docker compose up -d
    python scripts/persistence_check.py verify       # 启动后

`verify` 会把两段快照逐字段比对，并检查两个具名卷确实是同一份
（挂到新卷上等于数据从零开始，那种「没报错但东西没了」最难查）。

快照默认写在 .persistence-snapshot.json，已在 .gitignore 里。
"""

from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import subprocess
import sys
import urllib.parse

import acceptance_api as api

REPO_ROOT = api.REPO_ROOT
DEFAULT_SNAPSHOT = REPO_ROOT / ".persistence-snapshot.json"

# 比对用的两条查询：一条走字面匹配，一条走向量。
# 两条都取是有意的 —— 它们依赖的东西不同（pg_trgm 索引 / pgvector 里的向量），
# 只验一条的话，另一条的数据是否还在重启后依然可用就没人看。
KEYWORD_QUERY = "发布检查清单"
SEMANTIC_QUERY = "合并代码之前需要做什么检查"

# 相似度是 float32 算出来的，重建容器不会改变它，但没必要按位比对
SCORE_EPSILON = 1e-6


def volume_mounts() -> dict[str, str]:
    """返回 {容器名: 挂载的具名卷名}，用来确认重启前后用的是同一份存储。"""
    mounts: dict[str, str] = {}
    for service in ("api", "db"):
        cid = subprocess.run(
            ["docker", "compose", "ps", "-q", service],
            capture_output=True, cwd=REPO_ROOT,
        ).stdout.decode("utf-8", "replace").strip()
        if not cid:
            continue
        raw = subprocess.run(
            ["docker", "inspect", cid, "--format", "{{json .Mounts}}"],
            capture_output=True, cwd=REPO_ROOT,
        ).stdout.decode("utf-8", "replace").strip()
        try:
            entries = json.loads(raw)
        except json.JSONDecodeError:
            continue
        for entry in entries:
            # 只记具名卷：绑定挂载（如果将来有）的比较方式不一样，不混进来
            if entry.get("Type") == "volume" and entry.get("Name"):
                mounts[f"{service}:{entry.get('Destination')}"] = entry["Name"]
    return mounts


def take_snapshot() -> dict:
    """把「重启后应当原样还在」的东西全部抓下来。"""
    docs = []
    for archived in (False, True):
        _, listing, _ = api.call("GET", f"/documents?pageSize=200&archived={str(archived).lower()}")
        for d in listing["items"]:
            # 逐字节校验原文件：只比 sizeBytes 会漏掉「长度对但内容变了」的情况，
            # 而下载内容与原文件一致本身就是一条独立的验收条件
            status, blob, _ = api.call("GET", f"/documents/{d['id']}/download")
            docs.append({
                "id": d["id"],
                "name": d["name"],
                "categoryId": d["categoryId"],
                "tags": d["tags"],
                "archived": d["archived"],
                "indexStatus": d["indexStatus"],
                "sizeBytes": d["sizeBytes"],
                "sha256": hashlib.sha256(blob).hexdigest() if status == 200 else None,
            })
    docs.sort(key=lambda d: d["id"])

    _, tree, _ = api.call("GET", "/categories")
    categories = sorted(
        (
            {"id": c["id"], "name": c["name"], "parentId": c["parentId"], "depth": c["depth"]}
            for c in tree["items"]
        ),
        key=lambda c: c["id"],
    )

    _, keyword, _ = api.call("GET", "/search?q=" + urllib.parse.quote(KEYWORD_QUERY))
    _, semantic, _ = api.call("POST", "/search/semantic", json_body={"query": SEMANTIC_QUERY, "topK": 5})

    return {
        "documents": docs,
        "categories": categories,
        "volumes": volume_mounts(),
        "keyword": {
            "query": KEYWORD_QUERY,
            "ids": [it["document"]["id"] for it in keyword["items"]],
        },
        "semantic": {
            "query": SEMANTIC_QUERY,
            "topId": semantic["items"][0]["document"]["id"] if semantic["items"] else None,
            "score": semantic["items"][0]["score"] if semantic["items"] else None,
        },
    }


def cmd_snapshot(path: pathlib.Path) -> int:
    snap = take_snapshot()
    path.write_text(json.dumps(snap, ensure_ascii=False, indent=2), encoding="utf-8")
    print(f"快照已写入 {path}")
    print(f"  文件 {len(snap['documents'])} 份（其中归档 "
          f"{sum(1 for d in snap['documents'] if d['archived'])} 份）")
    print(f"  分类 {len(snap['categories'])} 个")
    print(f"  具名卷 {len(snap['volumes'])} 个：{', '.join(sorted(snap['volumes'].values()))}")
    print(f"  关键词「{KEYWORD_QUERY}」命中 {len(snap['keyword']['ids'])} 份")
    print(f"  语义「{SEMANTIC_QUERY}」首位 {snap['semantic']['topId']}"
          f"（相似度 {snap['semantic']['score']}）")
    if not snap["documents"]:
        print("\n  注意：当前库里没有任何文件，这样的快照证明不了什么。"
              "\n  先在界面上传几份文件、建几个分类、归档一份，再取快照。", file=sys.stderr)
        return 1
    return 0


def cmd_verify(path: pathlib.Path) -> int:
    before = json.loads(path.read_text(encoding="utf-8"))
    after = take_snapshot()

    before_docs = {d["id"]: d for d in before["documents"]}
    after_docs = {d["id"]: d for d in after["documents"]}

    print(f"== 文件（{len(before_docs)} 份） ==")
    api.chk("文件数量不变", len(after_docs), len(before_docs))
    api.chk("文件标识集合不变", sorted(after_docs), sorted(before_docs))
    for doc_id in sorted(set(before_docs) & set(after_docs)):
        was, now = before_docs[doc_id], after_docs[doc_id]
        api.chk(f"  {now['name']}：逐字段不变", now, was)

    print("== 分类 ==")
    api.chk("分类树不变", after["categories"], before["categories"])

    print("== 归档状态 ==")
    api.chk(
        "归档集合不变",
        sorted(d["id"] for d in after["documents"] if d["archived"]),
        sorted(d["id"] for d in before["documents"] if d["archived"]),
    )

    print("== 检索 ==")
    api.chk("关键词检索结果不变（含顺序）", after["keyword"]["ids"], before["keyword"]["ids"])
    api.chk("语义检索首位不变", after["semantic"]["topId"], before["semantic"]["topId"])
    before_score, after_score = before["semantic"]["score"], after["semantic"]["score"]
    if before_score is None or after_score is None:
        api.chk("语义检索有结果", after_score is not None, True)
    else:
        api.chk("语义相似度不变", abs(after_score - before_score) < SCORE_EPSILON, True)

    print("== 持久化存储 ==")
    api.chk("挂载的具名卷与停机前是同一份", after["volumes"], before["volumes"])
    for where, name in sorted(before["volumes"].items()):
        if after["volumes"].get(where) == name:
            print(f"  PASS  {where} → {name}")

    print(f"\n结果：PASS={api.PASS} FAIL={api.FAIL}")
    return 1 if api.FAIL else 0


def main() -> int:
    parser = argparse.ArgumentParser(description="数据持久化演练")
    parser.add_argument("action", choices=("snapshot", "verify"))
    parser.add_argument("--file", type=pathlib.Path, default=DEFAULT_SNAPSHOT)
    parser.add_argument("--base", default=api.DEFAULT_BASE, help=f"接口基地址，默认 {api.DEFAULT_BASE}")
    args = parser.parse_args()

    api.BASE_URL = args.base

    try:
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    except (AttributeError, OSError):
        pass

    if args.action == "snapshot":
        return cmd_snapshot(args.file)
    if not args.file.exists():
        print(f"找不到快照 {args.file}，请先执行 snapshot", file=sys.stderr)
        return 2
    return cmd_verify(args.file)


if __name__ == "__main__":
    sys.exit(main())
