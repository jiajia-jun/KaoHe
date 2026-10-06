#!/usr/bin/env python3
"""验收资料：5 组自然语言检索示例，以及每一组的预期命中文件。

这份脚本同时承担两件事：

1. 交付文档里的「5 组检索示例，说明预期命中的文件」——答案就是下面 EXAMPLES 这张表；
2. 让这张表可复现——对着跑起来的环境执行一次，逐条核对首位命中是否与预期一致。

每个示例打印三样东西：
  · 关键词检索（把整句原样当作关键词）：这几句都是日常说法，字面上和文档对不上，
    所以基本必然是 0 命中——这正是语义检索存在的理由；
  · 语义检索的首位命中及其相似度；
  · 命中片段，用来判断「命中了但牛头不对马嘴」和「确实答上了」的区别。

用法（在仓库根目录执行）：

    docker compose up -d
    python scripts/semantic_examples.py --reset       # 清空后重传 10 份语料再跑，交付文档里的表是这么来的
    python scripts/semantic_examples.py --ensure      # 不清空，缺什么补什么
    python scripts/semantic_examples.py               # 完全不动数据，直接跑
    python scripts/semantic_examples.py --reset --json docs/semantic-examples.json

关于 --reset：验收脚本跑归档、改名时会留下正文相同的副本，
两份一样的向量谁排第一纯属并列。要复现交付文档里那张表，
语料的构成必须是确定的，所以默认建议带 --reset。

关于复用 acceptance_api
-----------------------
multipart 上传、绕过 Windows 控制台代码页这些细节在 acceptance_api.py 里已经踩过一遍坑，
这里直接 import 它的 call/upload，避免同一段逻辑维护两份。
acceptance_api 的 main() 有 `if __name__ == "__main__"` 保护，import 不会触发验收流程。
"""

from __future__ import annotations

import argparse
import json
import pathlib
import sys
import time
import urllib.parse

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import acceptance_api as api  # noqa: E402

# 控制台是 GBK 时中文能显示但重定向到文件会乱码；统一按 UTF-8 写出，
# 这样 `> out.txt` 拿到的文件可以直接读。
if hasattr(sys.stdout, "reconfigure"):
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")

# (提问, 预期首位命中的文件, 为什么预期是它)
EXAMPLES: list[tuple[str, str, str]] = [
    (
        "合并代码之前需要做什么检查",
        "01_代码提交规范_v2.1.md",
        "文档里写的是「提交前自测」「评审要点」「合并请求」这类正式说法，"
        "没有任何一处出现「合并代码之前需要做什么检查」这串字。",
    ),
    (
        "容器起来了但是服务还是用不了",
        "09_本地部署故障排查_v1.3.txt",
        "这正是文档第二节讨论的情形（容器在线只说明进程存活），"
        "但原句用的是「健康接口」「日志」等术语。",
    ),
    (
        "线上出故障之后要做哪些复盘",
        "06_星桥项目_故障复盘_2026-09-21.md",
        "文档标题是「故障复盘」，正文讲时间线、影响面、改进项，"
        "没有「出事之后要做什么」这种口语表达。",
    ),
    (
        "团队成员平时是怎么找文件的",
        "07_星桥项目_用户访谈纪要_2026-09-12.txt",
        "这是访谈里用户自己描述的使用习惯，散在对话记录中，"
        "既不是标题也不是小标题。",
    ),
    (
        "发布之后怎么确认没问题",
        "05_星桥项目_发布检查清单_2026-09-18.txt",
        "文档是一份逐项打勾的执行记录，「确认没问题」这层意思要靠"
        "「验证」「回归」等条目名去推。",
    ),
]

READY_STATES = {"ready"}


def wait_ready(names: list[str], timeout: float = 180.0) -> dict[str, str]:
    """等到这些文件都索引完成（或失败），返回 文件名 -> 状态。"""
    deadline = time.time() + timeout
    last: dict[str, str] = {}
    while time.time() < deadline:
        status, res, _ = api.call("GET", "/documents?pageSize=100")
        if status != 200 or not isinstance(res, dict):
            time.sleep(1.0)
            continue
        last = {it["name"]: it["indexStatus"] for it in res.get("items", [])}
        if all(last.get(n) in ("ready", "failed", "skipped") for n in names):
            return last
        time.sleep(1.0)
    return last


def ensure_corpus(reset: bool) -> None:
    """准备好语料：--reset 时先清空业务数据，再补齐缺的语料并等索引完成。

    为什么需要一个清空开关：只要库里存在正文相同的两份文件（验收脚本跑归档、
    改名时会留下这样的副本），它们的向量就几乎一样，谁排第一纯属并列。
    要复现交付文档里那张表，得让语料的构成是确定的。
    """
    if reset:
        api.reset()
        print("已清空业务数据，重新上传 10 份语料\n")

    status, res, _ = api.call("GET", "/documents?pageSize=100")
    if status != 200 or not isinstance(res, dict):
        raise SystemExit(f"无法读取文件列表（HTTP {status}），先确认 docker compose up -d 已经跑起来")

    have = {it["name"] for it in res.get("items", [])}
    missing = [n for n in api.TEXT_CORPUS + api.PDF_CORPUS if n not in have]
    if missing:
        print(f"补齐语料 {len(missing)} 份：{', '.join(missing)}")
        for name in missing:
            code, body, _ = api.upload(name)
            if code != 201:
                raise SystemExit(f"上传 {name} 失败：HTTP {code} {body}")
    print("等待索引完成……")

    states = wait_ready(api.TEXT_CORPUS)
    bad = [n for n in api.TEXT_CORPUS if states.get(n) != "ready"]
    for name in api.TEXT_CORPUS:
        print(f"  {states.get(name, '缺失'):>8}  {name}")
    if bad:
        raise SystemExit(f"这些语料没有走到 ready，无法用于语义检索：{', '.join(bad)}")
    print()


def run_examples() -> tuple[int, list[dict]]:
    """跑一遍 5 个示例。返回 (没有对上的条数, 明细)。"""
    records: list[dict] = []
    bad = 0

    for index, (query, expected, why) in enumerate(EXAMPLES, start=1):
        rows: list[str] = []

        # 关键词检索走 GET，整句要 URL 编码
        kw_status, kw, _ = api.call("GET", "/search?q=" + urllib.parse.quote(query))
        kw_total = kw["total"] if kw_status == 200 else f"HTTP {kw_status}"

        se_status, se, _ = api.call("POST", "/search/semantic", json_body={"query": query, "topK": 5})
        if se_status != 200:
            print(f"[{index}] {query}\n  语义检索失败：HTTP {se_status} {se}\n")
            records.append(
                {"query": query, "expected": expected, "why": why, "error": f"HTTP {se_status}", "matched": False}
            )
            bad += 1
            continue

        items = se["items"]
        top = items[0] if items else None
        ok = bool(top) and top["document"]["name"] == expected
        if not ok:
            bad += 1

        rows.append(f"  关键词检索（整句）：{'0 命中' if kw_total == 0 else str(kw_total) + ' 命中'}")
        if top:
            rows.append(
                f"  语义检索首位：{top['document']['name']}"
                f"  相似度 {top['score']:.4f}（阈值 {se['minScore']}）"
            )
            for match in top["matches"][:2]:
                one_line = " ".join(match["text"].split())
                rows.append(f"      · {one_line}")
        else:
            rows.append(f"  语义检索无结果（阈值 {se['minScore']}，没有片段达到该相似度）")
        rows.append(f"  预期：{expected}  → {'符合' if ok else '不符'}")

        records.append(
            {
                "query": query,
                "expected": expected,
                "why": why,
                "keywordTotal": kw_total,
                "minScore": se["minScore"],
                "top": None
                if not top
                else {
                    "name": top["document"]["name"],
                    "score": top["score"],
                    "matches": [m["text"] for m in top["matches"]],
                },
                "matched": ok,
            }
        )

        print(f"[{index}] {query}")
        print("\n".join(rows))
        print(f"  说明：{why}\n")

    print(f"结果：{len(EXAMPLES)} 例中 {len(EXAMPLES) - bad} 例首位命中与预期一致")
    return bad, records


def main() -> int:
    parser = argparse.ArgumentParser(description="5 组自然语言检索示例的复现脚本")
    parser.add_argument("--ensure", action="store_true", help="先补齐语料并等索引完成")
    parser.add_argument(
        "--reset",
        action="store_true",
        help="先清空业务数据再重新上传 10 份语料（复现交付文档里的表格用这个）",
    )
    parser.add_argument("--base", default=api.DEFAULT_BASE, help="API 根地址")
    parser.add_argument("--json", dest="json_path", help="把结果另存为 JSON（供交付文档引用）")
    args = parser.parse_args()

    api.BASE_URL = args.base

    if args.reset:
        ensure_corpus(reset=True)
    elif args.ensure:
        ensure_corpus(reset=False)

    bad, records = run_examples()

    if args.json_path:
        out = pathlib.Path(args.json_path)
        out.write_text(
            json.dumps({"minScore": records[0].get("minScore") if records else None, "examples": records},
                       ensure_ascii=False, indent=2),
            encoding="utf-8",
        )
        print(f"\n明细已写入 {out}")

    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
