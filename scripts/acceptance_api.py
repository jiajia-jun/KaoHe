#!/usr/bin/env python3
"""接口层验收脚本：对着一套已经跑起来的 compose 环境逐条断言。

用法（在仓库根目录执行）：

    docker compose up -d
    python scripts/acceptance_api.py            # 会先清空业务数据再开始
    python scripts/acceptance_api.py --keep     # 保留现有数据，只跑断言

为什么不用 curl 写这个脚本
--------------------------
在 Windows 上，curl.exe 会把命令行参数按 ANSI 代码页（中文机器上是 GBK）转码后再发出，
所以 `-F "file=@中文名.md"` 和 `-F "tags=故障,复盘"` 到达服务端时字节已经被损坏。
用它测非 ASCII 的接口会得出「服务端乱码」的错误结论。
这里用标准库手工拼 multipart，报文里的字节完全由脚本控制，
既避开了这个转码，也不需要额外安装 requests。
"""

from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

REPO_ROOT = pathlib.Path(__file__).resolve().parent.parent
CORPUS = REPO_ROOT / "testdata" / "corpus"

DEFAULT_BASE = "http://127.0.0.1:8080/api/v1"
MD = "06_星桥项目_故障复盘_2026-09-21.md"
PDF = "10_文档分类与归档规范_v1.0.pdf"
OTHER = "01_代码提交规范_v2.1.md"

# 全部语料：7 份可抽取正文（md/txt）+ 3 份 PDF（只存不索引）
TEXT_CORPUS = [
    "01_代码提交规范_v2.1.md",
    "02_发布检查清单_v1.4.md",
    "03_文件服务接口约定_v1.2.md",
    "05_星桥项目_发布检查清单_2026-09-18.txt",
    "06_星桥项目_故障复盘_2026-09-21.md",
    "07_星桥项目_用户访谈纪要_2026-09-12.txt",
    "09_本地部署故障排查_v1.3.txt",
]
PDF_CORPUS = [
    "04_星桥项目_需求与范围_v1.0.pdf",
    "08_检索技术说明_关键词与语义_v1.0.pdf",
    "10_文档分类与归档规范_v1.0.pdf",
]

PASS = 0
FAIL = 0
BASE_URL = DEFAULT_BASE

# 传给 docker compose 的附加参数，用来指向另一个项目（例如用 -p kaoheclean
# 起一套干净的栈做从零启动的冒烟）。默认是空的，也就是用 compose 的默认项目。
#
# sql() / reset() 这类操作会直接动容器的数据卷，如果脚本的接口地址指向 A 栈、
# 而这两处按默认项目打到 B 栈上，就会一边读 A 一边清 B。所以项目名必须能一起指定。
COMPOSE_ARGS: list[str] = []


def chk(desc: str, got: object, want: object) -> None:
    global PASS, FAIL
    if got == want:
        print(f"  PASS  {desc}")
        PASS += 1
    else:
        print(f"  FAIL  {desc}\n        got ={got!r}\n        want={want!r}")
        FAIL += 1


def call(
    method: str,
    path: str,
    *,
    files: dict | None = None,
    fields: dict | None = None,
    json_body: object = None,
    raw_body: bytes | None = None,
    headers: dict | None = None,
    ctype: str | None = None,
):
    """发一次请求，返回 (状态码, 解析后的 JSON 或原始字节, 响应头)。"""
    body = raw_body
    hdrs = dict(headers or {})

    if files is not None:
        boundary = uuid.uuid4().hex
        chunks = []
        for field, (fname, data, fct) in files.items():
            chunks.append(
                f'--{boundary}\r\n'
                f'Content-Disposition: form-data; name="{field}"; filename="{fname}"\r\n'
                f"Content-Type: {fct}\r\n\r\n".encode("utf-8")
                + data
                + b"\r\n"
            )
        for key, value in (fields or {}).items():
            chunks.append(
                f'--{boundary}\r\nContent-Disposition: form-data; name="{key}"\r\n\r\n'.encode("utf-8")
                + str(value).encode("utf-8")
                + b"\r\n"
            )
        chunks.append(f"--{boundary}--\r\n".encode("utf-8"))
        body = b"".join(chunks)
        hdrs["Content-Type"] = f"multipart/form-data; boundary={boundary}"
    elif json_body is not None:
        body = json.dumps(json_body, ensure_ascii=False).encode("utf-8")
        hdrs["Content-Type"] = "application/json"
    if ctype:
        hdrs["Content-Type"] = ctype

    req = urllib.request.Request(BASE_URL + path, data=body, method=method, headers=hdrs)
    try:
        with urllib.request.urlopen(req) as resp:
            payload, status, resp_headers = resp.read(), resp.status, dict(resp.headers)
    except urllib.error.HTTPError as exc:
        payload, status, resp_headers = exc.read(), exc.code, dict(exc.headers)

    try:
        return status, json.loads(payload.decode("utf-8")), resp_headers
    except Exception:
        return status, payload, resp_headers


def upload(name: str, fields: dict | None = None):
    data = (CORPUS / name).read_bytes()
    return call(
        "POST",
        "/documents",
        files={"file": (name, data, "application/octet-stream")},
        fields=fields,
    )


def sql(query: str) -> str:
    """直接查库，用于验证“不该落库的确实没落库”这类界面看不到的断言。"""
    result = subprocess.run(
        ["docker", "compose", *COMPOSE_ARGS, "exec", "-T", "db",
         "psql", "-U", "kaohe", "-d", "kaohe", "-tAc", query],
        capture_output=True,
        cwd=REPO_ROOT,
    )
    if result.returncode != 0:
        raise RuntimeError(f"查询失败：{result.stderr.decode('utf-8', 'replace').strip()}")
    # 显式按 UTF-8 解码，不用 text=True：text=True 按本机 locale 解码，
    # 在中文 Windows 上就是 GBK，任何一次带中文的查询都会在这里抛
    # UnicodeDecodeError。数据库存的是 UTF-8，按 UTF-8 读回去才是对的。
    return result.stdout.decode("utf-8", "replace").strip()


def doc_pk(uid: str) -> int:
    """把对外标识 doc_xxx 换成表里的数字主键。

    直接查库时才用得上：document_chunks / index_jobs 的外键指向数字主键，
    而接口对外只用 doc_uid，两者不能混着写进 SQL。
    """
    return int(sql(f"SELECT id FROM documents WHERE doc_uid = '{uid}'"))


def compose(*args: str) -> subprocess.CompletedProcess:
    # 不传 text=True：解码发生在 subprocess 的读取线程里，且是即刻的，
    # 按本机 locale（中文 Windows 上是 GBK）解码，碰到非 UTF-8 字节就抛，
    # 哪怕调用方根本不看输出（本脚本的四个调用点都不看）。
    return subprocess.run(
        ["docker", "compose", *COMPOSE_ARGS, *args], capture_output=True, cwd=REPO_ROOT
    )


def wait_index(doc_id: str, want: set[str], timeout: float = 90.0) -> str:
    """轮询文档状态直到落在 want 里，返回最后看到的状态。

    索引是后台异步做的，接口本身是同步返回的，所以这里必须轮询 ——
    这也正是验收条件里「文件应显示索引处理状态」的实际形态。
    """
    deadline = time.monotonic() + timeout
    seen = "未知"
    while time.monotonic() < deadline:
        _, doc, _ = call("GET", f"/documents/{doc_id}")
        seen = doc["indexStatus"]
        if seen in want:
            return seen
        time.sleep(0.3)
    return seen


def wait_all_index(doc_ids: list[str], timeout: float = 180.0) -> None:
    """等到这批文档全部脱离 pending/processing 为止。"""
    deadline = time.monotonic() + timeout
    remaining = set(doc_ids)
    while remaining and time.monotonic() < deadline:
        _, listing, _ = call("GET", "/documents?pageSize=100")
        unsettled = {
            d["id"]
            for d in listing["items"]
            if d["indexStatus"] in ("pending", "processing")
        }
        remaining = unsettled & set(doc_ids)
        if remaining:
            time.sleep(0.4)


def reset() -> None:
    # categories 必须一起清：documents 引用它，单独清 documents 会把它留下，
    # 后面的分类用例就会带着上一轮的残留数据开始
    sql("TRUNCATE documents, index_jobs, document_chunks, categories RESTART IDENTITY CASCADE;")
    subprocess.run(
        ["docker", "compose", *COMPOSE_ARGS, "exec", "-T", "api",
         "sh", "-c", "rm -rf /data/uploads/*"],
        capture_output=True,
        cwd=REPO_ROOT,
    )


def main() -> int:
    global BASE_URL

    parser = argparse.ArgumentParser(description="接口层验收脚本")
    parser.add_argument("--base", default=DEFAULT_BASE, help=f"接口基地址，默认 {DEFAULT_BASE}")
    parser.add_argument("--keep", action="store_true", help="保留现有数据，跳过开头的清空")
    args = parser.parse_args()
    BASE_URL = args.base

    if not CORPUS.is_dir():
        print(f"找不到语料目录 {CORPUS}，请在仓库根目录执行本脚本", file=sys.stderr)
        return 2

    if args.keep:
        print("== 0. 跳过清空（--keep） ==")
    else:
        print("== 0. 清空业务数据 ==")
        reset()
        print("  已清空")

    print("== 1. 上传 Markdown（中文文件名 + 中文标签） ==")
    status, doc, _ = upload(MD, {"tags": "故障,复盘"})
    chk("状态码", status, 201)
    chk("文件名逐字符一致", doc["name"], MD)
    chk("标签", doc["tags"], ["故障", "复盘"])
    chk("indexStatus", doc["indexStatus"], "pending")
    chk("storageStatus", doc["storageStatus"], "stored")
    chk("sizeBytes", doc["sizeBytes"], len((CORPUS / MD).read_bytes()))
    chk("categoryId 为空", doc["categoryId"], None)
    chk("archived 初始为 false", doc["archived"], False)
    uid = doc["id"]
    chk("已建索引任务", sql("SELECT count(*) FROM index_jobs"), "1")

    print("== 2. 上传 PDF（不抽取正文，不应建任务） ==")
    status, pdf_doc, _ = upload(PDF)
    chk("状态码", status, 201)
    chk("indexStatus", pdf_doc["indexStatus"], "not_supported")
    chk("任务数仍为 1", sql("SELECT count(*) FROM index_jobs"), "1")

    print("== 3. 不支持的格式 ==")
    status, err, _ = call(
        "POST", "/documents", files={"file": ("x.exe", b"MZ\x90\x00", "application/octet-stream")}
    )
    chk("状态码", status, 400)
    chk("错误码", err["code"], "unsupported_type")
    chk("未落库", sql("SELECT count(*) FROM documents"), "2")

    print("== 4. 引用不存在的分类 ==")
    status, err, _ = upload(OTHER, {"categoryId": "999999"})
    chk("状态码", status, 400)
    chk("错误码", err["code"], "bad_request")

    print("== 5. 列表 ==")
    status, listing, _ = call("GET", "/documents")
    chk("状态码", status, 200)
    chk("total", listing["total"], 2)
    chk("items 是数组", isinstance(listing["items"], list), True)
    chk("分页字段", (listing["page"], listing["pageSize"]), (1, 20))

    print("== 6. 详情 ==")
    status, one, _ = call("GET", f"/documents/{uid}")
    chk("状态码", status, 200)
    chk("id 一致", one["id"], uid)
    chk("回传文件名一致", one["name"], MD)
    chk("不对外暴露存储键", "storageKey" in one, False)

    print("== 7. 下载与原文件逐字节一致 ==")
    status, blob, resp_headers = call("GET", f"/documents/{uid}/download")
    source = (CORPUS / MD).read_bytes()
    chk("状态码", status, 200)
    chk("sha256 相同", hashlib.sha256(blob).hexdigest(), hashlib.sha256(source).hexdigest())
    chk("Content-Type", resp_headers.get("Content-Type"), "text/markdown; charset=utf-8")
    disposition = resp_headers.get("Content-Disposition", "")
    chk("附件形式", disposition.startswith("attachment"), True)
    chk("按 RFC 5987 编码中文名", "filename*=UTF-8''" in disposition, True)
    chk("中文名已百分号编码", "%E6%95%85%E9%9A%9C" in disposition, True)

    print("== 8. Range 请求（PDF 在线预览依赖） ==")
    status, part, _ = call("GET", f"/documents/{uid}/download", headers={"Range": "bytes=0-9"})
    chk("状态码 206", status, 206)
    chk("返回 10 字节", len(part), 10)
    chk("内容与源文件前 10 字节相同", part, source[:10])
    _, _, resp_headers = call("GET", f"/documents/{uid}/download?inline=true")
    chk("inline=true 改为内联", resp_headers.get("Content-Disposition", "").startswith("inline"), True)

    print("== 9. 归档 / 恢复 ==")
    status, updated, _ = call("PATCH", f"/documents/{uid}", json_body={"archived": True})
    chk("状态码", status, 200)
    chk("archived", updated["archived"], True)
    chk("默认列表剩 1 条", call("GET", "/documents")[1]["total"], 1)
    chk("归档列表 1 条", call("GET", "/documents?archived=true")[1]["total"], 1)
    _, updated, _ = call("PATCH", f"/documents/{uid}", json_body={"archived": False})
    chk("恢复后 archived", updated["archived"], False)
    chk("默认列表回到 2 条", call("GET", "/documents")[1]["total"], 2)

    print("== 10. 改名 / 覆盖标签 / 移出分类 ==")
    status, updated, _ = call(
        "PATCH",
        f"/documents/{uid}",
        json_body={"name": "故障复盘-已改名.md", "tags": ["故障"], "categoryId": None},
    )
    chk("状态码", status, 200)
    chk("新文件名", updated["name"], "故障复盘-已改名.md")
    chk("标签被整体覆盖为 1 个", updated["tags"], ["故障"])
    chk("分类为空", updated["categoryId"], None)
    chk("改名后仍可下载", call("GET", f"/documents/{uid}/download")[0], 200)

    print("== 11. 错误路径 ==")
    status, err, _ = call("GET", "/documents/doc_0000000000000000")
    chk("不存在的文档 404", status, 404)
    chk("错误码", err["code"], "not_found")
    chk("非法 JSON 400", call("PATCH", f"/documents/{uid}", raw_body=b"not-json", ctype="application/json")[0], 400)
    chk("空文件名 400", call("PATCH", f"/documents/{uid}", json_body={"name": "  "})[0], 400)
    chk("缺 file 字段 400", call("POST", "/documents", files={}, fields={"categoryId": "1"})[0], 400)
    chk("文件名超长 400", call("PATCH", f"/documents/{uid}", json_body={"name": "x" * 201})[0], 400)
    status, err, _ = call("PATCH", f"/documents/{uid}", json_body={"tags": ["发布,复盘"]})
    chk("标签含逗号 400", status, 400)
    chk("  提示指明了原因", "逗号" in err["message"], True)
    # 必须用互不相同的标签：重复项会被去重，["a"]*21 去重后只剩 1 个，测不到数量上限
    many = [f"t{i}" for i in range(21)]
    status, err, _ = call("PATCH", f"/documents/{uid}", json_body={"tags": many})
    chk("21 个不同标签 400", status, 400)
    chk("  提示指明了上限", "20" in err["message"], True)
    status, updated, _ = call("PATCH", f"/documents/{uid}", json_body={"tags": many[:20]})
    chk("恰好 20 个标签可接受", status, 200)
    chk("  标签全部写入", len(updated["tags"]), 20)
    _, updated, _ = call("PATCH", f"/documents/{uid}", json_body={"tags": ["发布", "发布 ", " 发布"]})
    chk("去重与去空白后只剩 1 个", len(updated["tags"]), 1)
    chk("非法分类筛选 400", call("GET", "/documents?categoryId=abc")[0], 400)

    print("== 12. 超过上传上限（大于后端 20 MiB、小于 nginx 的 100m，必须由后端拒绝） ==")
    big = b"\0" * (21 * 1024 * 1024)
    status, err, _ = call("POST", "/documents", files={"file": ("big.txt", big, "text/plain")})
    chk("状态码 413", status, 413)
    chk("错误码", err["code"], "file_too_large")
    chk("提示含上限数值", "20" in err["message"], True)
    chk("未落库", sql("SELECT count(*) FROM documents"), "2")

    print("== 13. 分类：创建与层级 ==")
    status, root, _ = call("POST", "/categories", json_body={"name": "研发"})
    chk("创建顶层分类 201", status, 201)
    chk("名称", root["name"], "研发")
    chk("父分类为空", root["parentId"], None)
    root_id = root["id"]

    status, child, _ = call("POST", "/categories", json_body={"name": "前端", "parentId": root_id})
    chk("创建子分类 201", status, 201)
    chk("挂在父分类下", child["parentId"], root_id)
    chk("层级为 1", child["depth"], 1)
    child_id = child["id"]

    status, err, _ = call("POST", "/categories", json_body={"name": "研发"})
    chk("同级重名 409", status, 409)
    chk("错误码", err["code"], "conflict")
    chk("空分类名 400", call("POST", "/categories", json_body={"name": "  "})[0], 400)
    chk("分类名超长 400", call("POST", "/categories", json_body={"name": "x" * 41})[0], 400)
    chk("父分类不存在 400", call("POST", "/categories", json_body={"name": "孤儿", "parentId": 999999})[0], 400)

    status, tree, _ = call("GET", "/categories")
    chk("分类树 200", status, 200)
    chk("顶层只有 1 个", len(tree["items"]), 1)
    chk("顶层含 1 个子分类", len(tree["items"][0]["children"]), 1)

    print("== 14. 分类：文件归属与筛选 ==")
    status, updated, _ = call("PATCH", f"/documents/{uid}", json_body={"categoryId": child_id})
    chk("移动文件 200", status, 200)
    chk("categoryId 已更新", updated["categoryId"], child_id)
    chk("categoryName 一并返回", updated["categoryName"], "前端")

    _, listing, _ = call("GET", f"/documents?categoryId={child_id}")
    chk("按子分类筛选命中 1 条", listing["total"], 1)
    _, listing, _ = call("GET", f"/documents?categoryId={root_id}")
    chk("按父分类筛选能筛到子分类的文件", listing["total"], 1)

    _, listing, _ = call("GET", "/documents?categoryId=none")
    chk("未分类筛选只剩 PDF", listing["total"], 1)
    chk("  且确实是那个 PDF", listing["items"][0]["id"], pdf_doc["id"])

    print("== 15. 分类：改名与移动 ==")
    status, renamed, _ = call("PATCH", f"/categories/{child_id}", json_body={"name": "移动端"})
    chk("改名 200", status, 200)
    chk("新名称", renamed["name"], "移动端")
    _, doc_after, _ = call("GET", f"/documents/{uid}")
    chk("文件显示的分类名同步更新", doc_after["categoryName"], "移动端")

    status, err, _ = call("PATCH", f"/categories/{root_id}", json_body={"parentId": child_id})
    chk("移到自己的子分类下 400", status, 400)
    chk("错误码", err["code"], "bad_request")
    chk("  提示说明了原因", "自己" in err["message"], True)

    status, moved_root, _ = call("PATCH", f"/categories/{child_id}", json_body={"parentId": None})
    chk("移到顶层 200", status, 200)
    chk("父分类已清空", moved_root["parentId"], None)
    chk("层级回到 0", moved_root["depth"], 0)

    print("== 16. 分类：删除 ==")
    chk("删除不存在的分类 404", call("DELETE", "/categories/999999")[0], 404)

    # 用自己的父子结构来测删除，不复用第 13、15 节建的那两个：
    # 第 15 节已经把「移动端」移到了顶层，再假设「研发」还有子分类就会测到别的东西上。
    _, group, _ = call("POST", "/categories", json_body={"name": "归档组"})
    group_id = group["id"]
    _, subgroup, _ = call("POST", "/categories", json_body={"name": "归档子类", "parentId": group_id})
    subgroup_id = subgroup["id"]

    chk("删除仍有子分类的父分类 409", call("DELETE", f"/categories/{group_id}")[0], 409)
    _, tree, _ = call("GET", "/categories")
    chk("  被拒后父分类仍在", [n["name"] for n in tree["items"]].count("归档组"), 1)

    _, moved, _ = call("PATCH", f"/documents/{uid}", json_body={"categoryId": subgroup_id})
    chk("把文件移入该子分类", moved["categoryId"], subgroup_id)

    status, result, _ = call("DELETE", f"/categories/{subgroup_id}")
    chk("删除子分类 200", status, 200)
    chk("移出分类的文件数为 1", result["movedDocuments"], 1)
    _, doc_after, _ = call("GET", f"/documents/{uid}")
    chk("文件仍在", doc_after["id"], uid)
    chk("文件已变为未分类", doc_after["categoryId"], None)
    chk("文件未归档", doc_after["archived"], False)
    chk("原文件仍可下载", call("GET", f"/documents/{uid}/download")[0], 200)

    chk("父分类此时已无子分类，可删除", call("DELETE", f"/categories/{group_id}")[0], 200)

    # 收尾：把前面几节建的分类清干净，「移动端」下的文件已在上面移走，所以是 0
    status, result, _ = call("DELETE", f"/categories/{child_id}")
    chk("删除已无文件的分类 200", status, 200)
    chk("  移出文件数为 0", result["movedDocuments"], 0)
    chk("删除「研发」200", call("DELETE", f"/categories/{root_id}")[0], 200)
    chk("分类树已空", len(call("GET", "/categories")[1]["items"]), 0)

    print("== 17. 索引：从待索引异步走到已索引 ==")
    # 第 1 节已经断言了上传后立刻是 pending；这里要验的是后台会把它推到终态。
    reached = wait_index(uid, {"ready", "failed"})
    chk("最终落到 ready", reached, "ready")
    _, doc, _ = call("GET", f"/documents/{uid}")
    uid_pk = doc_pk(uid)
    chk("索引失败的说明已清空", doc["indexError"], None)
    chk("已生成正文片段", int(sql(f"SELECT count(*) FROM document_chunks WHERE document_id={uid_pk}")) > 0, True)
    chk(
        "片段向量维数是 512",
        sql(f"SELECT DISTINCT vector_dims(embedding) FROM document_chunks WHERE document_id={uid_pk}"),
        "512",
    )
    chk(
        "片段正文非空",
        sql(f"SELECT count(*) FROM document_chunks WHERE document_id={uid_pk} AND content <> ''"),
        sql(f"SELECT count(*) FROM document_chunks WHERE document_id={uid_pk}"),
    )
    chk("任务已标记 done", sql(f"SELECT status FROM index_jobs WHERE document_id={uid_pk}"), "done")

    print("== 18. 索引：PDF 只存不索引 ==")
    pdf_pk = doc_pk(pdf_doc["id"])
    chk("PDF 的 indexStatus", pdf_doc["indexStatus"], "not_supported")
    chk("PDF 没有索引任务", sql(f"SELECT count(*) FROM index_jobs WHERE document_id={pdf_pk}"), "0")
    chk("PDF 没有正文片段", sql(f"SELECT count(*) FROM document_chunks WHERE document_id={pdf_pk}"), "0")
    status, err, _ = call("POST", f"/documents/{pdf_doc['id']}/reindex")
    chk("对 PDF 触发重新索引 400", status, 400)
    chk("错误码", err["code"], "bad_request")
    chk("  提示说明了原因", "格式" in err["message"] or "支持" in err["message"], True)

    print("== 19. 索引：重新索引幂等 ==")
    chk("重新索引不存在的文档 404", call("POST", "/documents/doc_0000000000000000/reindex")[0], 404)

    # 先把 worker 停下来再连点两次：否则第一次很可能在两次调用之间就被处理完了，
    # 「没有产生第二个任务」就可能只是因为第一个已经 done，而不是因为接口幂等。
    compose("stop", "worker")
    try:
        status, first, _ = call("POST", f"/documents/{uid}/reindex")
        chk("重新索引 200", status, 200)
        chk("状态回到 pending", first["indexStatus"], "pending")
        chk("上一次的失败说明被清掉", first["indexError"], None)
        chk("连点第二次仍 200", call("POST", f"/documents/{uid}/reindex")[0], 200)
        chk(
            "同一文档只有 1 个进行中的任务",
            sql(f"SELECT count(*) FROM index_jobs WHERE document_id={uid_pk} AND status IN ('queued','running')"),
            "1",
        )
    finally:
        compose("start", "worker")

    chk("worker 恢复后重新索引完成", wait_index(uid, {"ready", "failed"}), "ready")
    chk(
        "重建后片段仍在",
        int(sql(f"SELECT count(*) FROM document_chunks WHERE document_id={uid_pk}")) > 0,
        True,
    )

    print("== 20. 索引失败：重试、终态与原因 ==")
    # 只有空白字符的文本抽不出任何正文，是构造「索引失败」最省事也最真实的办法。
    status, blank_doc, _ = call(
        "POST",
        "/documents",
        files={"file": ("空白.txt", "   \n\t\n".encode("utf-8"), "text/plain")},
    )
    chk("上传空白文本 201", status, 201)
    chk("初始为 pending", blank_doc["indexStatus"], "pending")
    blank_pk = doc_pk(blank_doc["id"])
    # 把上限压到 1，第一次失败就是终态；否则默认要等 15s + 30s 两轮退避
    sql(f"UPDATE index_jobs SET max_attempts=1 WHERE document_id={blank_pk}")
    chk("最终落到 failed", wait_index(blank_doc["id"], {"failed"}), "failed")
    _, failed_doc, _ = call("GET", f"/documents/{blank_doc['id']}")
    chk("带上了失败原因", (failed_doc["indexError"] or "") != "", True)
    chk("失败的任务记为 failed", sql(f"SELECT status FROM index_jobs WHERE document_id={blank_pk}"), "failed")
    chk("失败时没有留下半份片段", sql(f"SELECT count(*) FROM document_chunks WHERE document_id={blank_pk}"), "0")

    print("== 21. 索引失败不影响原文件的保存与下载 ==")
    status, blob, _ = call("GET", f"/documents/{blank_doc['id']}/download")
    chk("索引失败的文件仍可下载", status, 200)
    chk("内容与原文件逐字节一致", blob, "   \n\t\n".encode("utf-8"))
    _, listing, _ = call("GET", "/documents?pageSize=100")
    chk("仍出现在列表里", any(d["id"] == blank_doc["id"] for d in listing["items"]), True)
    _, still, _ = call("GET", f"/documents/{blank_doc['id']}")
    chk("没有被自动归档", still["archived"], False)

    print("== 22. 索引失败后重试可以成功 ==")
    status, retry_doc, _ = upload(OTHER)
    chk("上传一份可抽取正文的 Markdown 201", status, 201)
    chk("先正常索引成功", wait_index(retry_doc["id"], {"ready", "failed"}), "ready")
    retry_pk = doc_pk(retry_doc["id"])
    # 手工把它按回失败态，模拟「上一次因为边车没起来而失败」的历史记录。
    # 原因用 ASCII：这串字要经过 docker compose exec 传给 psql，
    # 而 Windows 上非 ASCII 的命令行参数不保证原样到达。
    sql(
        f"UPDATE documents SET index_status='failed', index_error='simulated failure' WHERE id={retry_pk};"
        f"UPDATE index_jobs SET status='failed' WHERE document_id={retry_pk}"
    )
    _, back, _ = call("GET", f"/documents/{retry_doc['id']}")
    chk("已置为 failed", back["indexStatus"], "failed")
    status, queued, _ = call("POST", f"/documents/{retry_doc['id']}/reindex")
    chk("重试接口 200", status, 200)
    chk("状态回到 pending", queued["indexStatus"], "pending")
    chk("重试后成功", wait_index(retry_doc["id"], {"ready", "failed"}), "ready")
    _, done, _ = call("GET", f"/documents/{retry_doc['id']}")
    chk("失败原因已被清掉", done["indexError"], None)
    chk("重新生成了片段", int(sql(f"SELECT count(*) FROM document_chunks WHERE document_id={retry_pk}")) > 0, True)

    print("== 23. 全部语料：7 份可检索 + 3 份仅存储 ==")
    corpus_docs = {}
    for name in TEXT_CORPUS + PDF_CORPUS:
        status, item, _ = upload(name)
        chk(f"上传 {name}", status, 201)
        corpus_docs[name] = item["id"]
    wait_all_index(list(corpus_docs.values()))

    _, listing, _ = call("GET", "/documents?pageSize=100")
    by_name = {d["name"]: d for d in listing["items"]}
    chk("文本语料全部 ready", [by_name[n]["indexStatus"] for n in TEXT_CORPUS], ["ready"] * len(TEXT_CORPUS))
    chk("PDF 语料全部 not_supported", [by_name[n]["indexStatus"] for n in PDF_CORPUS], ["not_supported"] * len(PDF_CORPUS))
    chk(
        "没有任何一份停在中间态",
        [d["indexStatus"] for d in listing["items"] if d["indexStatus"] in ("pending", "processing")],
        [],
    )
    indexed = int(sql("SELECT count(*) FROM documents WHERE index_status='ready'"))
    chk("可检索文档数 ≥ 7", indexed >= 7, True)
    chk(
        "所有片段都是 512 维",
        sql(
            "SELECT count(*) FROM document_chunks WHERE vector_dims(embedding) <> 512"
        ),
        "0",
    )
    chk(
        "片段序号从 0 开始且连续",
        sql(
            "SELECT count(*) FROM ("
            "  SELECT document_id, count(*) AS n, max(ordinal) AS mx, min(ordinal) AS mn"
            "  FROM document_chunks GROUP BY document_id"
            ") t WHERE mn <> 0 OR mx <> n - 1"
        ),
        "0",
    )

    print("== 24. 关键词检索 ==")
    status, res, _ = call("GET", "/search?q=" + urllib.parse.quote("健康接口"))
    chk("状态码", status, 200)
    chk("只命中正文提到它的那一份", res["total"], 1)
    hit = res["items"][0]
    chk("  命中文件", hit["document"]["name"], "09_本地部署故障排查_v1.3.txt")
    chk("  不是文件名命中", hit["nameHit"], False)
    chk("  正文命中段数", hit["bodyHits"] >= 1, True)
    chk("  片段里带了关键词", "健康接口" in hit["matches"][0]["text"], True)
    chk("  片段两端有省略号", hit["matches"][0]["text"].startswith("…"), True)

    status, res, _ = call("GET", "/search?q=" + urllib.parse.quote("发布检查清单"))
    chk("按文件名也能命中", status, 200)
    items = res["items"]
    name_hits = [it["document"]["name"] for it in items if it["nameHit"]]
    body_only = [it["document"]["name"] for it in items if not it["nameHit"]]
    # 不写死命中份数：正文里提到过这个词的文档会随语料变化，
    # 而这一节要验的是「文件名命中排在前面」「正文命中带得出片段」这两条规则。
    chk("文件名命中的就这两份（排在前面）", name_hits,
        ["05_星桥项目_发布检查清单_2026-09-18.txt", "02_发布检查清单_v1.4.md"])
    chk("  它们确实排在最前面", [it["nameHit"] for it in items[:2]], [True, True])
    chk("  其余都是正文命中", all(not it["nameHit"] for it in items[2:]), True)
    chk("正文里提到它的包含访谈纪要", "07_星桥项目_用户访谈纪要_2026-09-12.txt" in body_only, True)
    chk("每一条都带回了命中片段", all(len(it["matches"]) >= 1 for it in items), True)

    chk("检索不存在的词返回空", call("GET", "/search?q=" + urllib.parse.quote("不存在的词xyz"))[1]["total"], 0)
    chk("空关键词 400", call("GET", "/search?q=")[0], 400)
    chk("关键词过长 400", call("GET", "/search?q=" + "x" * 201)[0], 400)
    chk("非法分类筛选 400",
        call("GET", "/search?q=" + urllib.parse.quote("检查") + "&categoryId=abc")[0], 400)

    print("== 25. 语义检索 ==")
    # 前面几节为了验归档和重试，留下了几份正文完全相同的副本
    # （故障复盘-已改名.md、第二次上传的代码提交规范）。两份同样的内容谁排前面
    # 本就并列，拿它验排序只会验出噪音。这里把第 23 节上传的那 7 份语料归到
    # 一个分类下、检索时限定该分类，顺带把「语义检索也认分类筛选」一起验了。
    _, semantic_cat_body, _ = call("POST", "/categories", json_body={"name": "语义验收语料"})
    semantic_cat = semantic_cat_body["id"]
    _, empty_cat_body, _ = call("POST", "/categories", json_body={"name": "语义验收空分类"})
    empty_cat = empty_cat_body["id"]
    for name in TEXT_CORPUS:
        call("PATCH", f"/documents/{corpus_docs[name]}", json_body={"categoryId": semantic_cat})
    _, scoped, _ = call("GET", f"/documents?pageSize=100&categoryId={semantic_cat}")
    chk("语料已归入同一分类", sorted(d["name"] for d in scoped["items"]), sorted(TEXT_CORPUS))

    # 与交付文档里的 5 组示例保持一致；scripts/semantic_examples.py 是这两处的可复现版本
    examples = [
        ("合并代码之前需要做什么检查", "01_代码提交规范_v2.1.md"),
        ("容器起来了但是服务还是用不了", "09_本地部署故障排查_v1.3.txt"),
        ("线上出故障之后要做哪些复盘", "06_星桥项目_故障复盘_2026-09-21.md"),
        ("团队成员平时是怎么找文件的", "07_星桥项目_用户访谈纪要_2026-09-12.txt"),
        ("发布之后怎么确认没问题", "05_星桥项目_发布检查清单_2026-09-18.txt"),
    ]
    for query, expected in examples:
        status, res, _ = call(
            "POST", "/search/semantic",
            json_body={"query": query, "topK": 5, "categoryId": semantic_cat},
        )
        chk(f"「{query}」状态码", status, 200)
        chk("  首位命中预期文件", res["items"][0]["document"]["name"], expected)
        chk("  相似度高于阈值", res["items"][0]["score"] > res["minScore"], True)
        chk("  返回了相关片段", len(res["items"][0]["matches"]) >= 1, True)
        chk("  片段带相似度", isinstance(res["items"][0]["matches"][0].get("score"), float), True)
        chk("  用的是同一套阈值", res["minScore"], 0.5)
        # 关键词检索对整句是无效的：这正是语义检索要解决的那类提问
        chk("  整句在关键词检索里检索不到",
            call("GET", "/search?q=" + urllib.parse.quote(query))[1]["total"], 0)

    # 向量检索永远会返回「最像的几条」，阈值是把它变成空结果的开关
    status, res, _ = call("POST", "/search/semantic", json_body={"query": "合并代码", "minScore": 0.99})
    chk("阈值调高到 0.99 后返回空", (status, res["total"]), (200, 0))

    # 分类筛选在语义检索里同样生效：限定到一个空分类，再像的片段也不该漏出来
    status, res, _ = call(
        "POST", "/search/semantic",
        json_body={"query": "合并代码之前需要做什么检查", "categoryId": empty_cat},
    )
    chk("限定到空分类时返回空", (status, res["total"]), (200, 0))

    # 只有索引完成的文档才有向量，索引失败的文件不该出现在结果里
    _, res, _ = call("POST", "/search/semantic", json_body={"query": "空白文本", "minScore": 0.01})
    chk("索引失败的文件不会出现在语义检索里",
        any(it["document"]["name"] == "空白.txt" for it in res["items"]), False)

    chk("空描述 400", call("POST", "/search/semantic", json_body={"query": "  "})[0], 400)
    chk("描述过长 400", call("POST", "/search/semantic", json_body={"query": "x" * 201})[0], 400)
    chk("非法 JSON 400", call("POST", "/search/semantic", raw_body=b"oops", ctype="application/json")[0], 400)
    chk("非法分类 400", call("POST", "/search/semantic",
        json_body={"query": "发布", "categoryId": -1})[0], 400)

    print("== 26. 向量边车不可用时的降级 ==")
    # 边车还活着时先取一次关键词检索的结果，等下拿它跟停掉边车之后的比对
    kw_query = "/search?q=" + urllib.parse.quote("健康接口")
    kw_before = call("GET", kw_query)[1]

    compose("stop", "embed")
    try:
        status, err, _ = call("POST", "/search/semantic", json_body={"query": "合并代码之前需要做什么检查"})
        chk("语义检索 503", status, 503)
        chk("错误码区别于内部错误", err["code"], "service_unavailable")
        chk("  提示指向向量服务", "向量" in err["message"], True)

        # 一个可选能力不可用，不该把文件管理和另一种检索一起拖下水。
        #
        # 比的是「停边车前后的同一次检索结果一致」，不写死命中份数：
        # 这套数据同时会被浏览器端到端用例和使用者本人在界面上改动，
        # 归档一份文件就会让命中数变，写死数字等于把别人的操作记成这里的失败。
        # 这一节要验的本来就是「不受影响」，前后一致才是正题。
        #
        # （曾经写死过 total==1，验的是「健康接口」只出现在 09 里。09 一旦被归档，
        #   这条就红，而应用的行为完全正确 —— 归档文件本就不该出现在检索结果里，
        #   那正是第 27 节要验的验收条件。断言粒度选错，会把正确行为报成缺陷。）
        _, kw_after, _ = call("GET", kw_query)
        chk("关键词检索结果与停边车前一致",
            [it["document"]["id"] for it in kw_after["items"]],
            [it["document"]["id"] for it in kw_before["items"]])
        # 结果集为空时「一致」是句废话，所以另按文件名搜一次本脚本自己造、
        # 且全程未归档的那份文档，确认检索这条路是真通着的。
        _, by_name, _ = call("GET", "/search?q=" + urllib.parse.quote("故障复盘-已改名"))
        chk("按文件名仍搜得到本脚本的文档",
            [it["document"]["id"] for it in by_name["items"]], [uid])
        chk("文件列表不受影响", call("GET", "/documents?pageSize=100")[0], 200)
        chk("下载不受影响", call("GET", f"/documents/{uid}/download")[0], 200)
    finally:
        compose("start", "embed")

    # 等边车重新加载好模型再往下走，否则后面的用例会继续拿到 503
    for _ in range(120):
        if call("POST", "/search/semantic", json_body={"query": "发布检查清单"})[0] == 200:
            break
        time.sleep(0.5)
    chk("边车恢复后语义检索可用",
        call("POST", "/search/semantic", json_body={"query": "发布检查清单"})[0], 200)

    # 验收条件原文：「归档文件不出现在默认列表和检索结果中，在归档区仍可找到；
    # 恢复后可重新检索和下载」。上面第 9 节只验了列表那一半，检索这一半在这里补上。
    print("== 27. 归档文件在检索里的可见性 ==")
    target = corpus_docs["05_星桥项目_发布检查清单_2026-09-18.txt"]
    _, before, _ = call("GET", "/search?q=" + urllib.parse.quote("发布检查清单"))
    chk("归档前能被关键词检索到", any(it["document"]["id"] == target for it in before["items"]), True)

    status, _, _ = call("PATCH", f"/documents/{target}", json_body={"archived": True})
    chk("归档 200", status, 200)
    try:
        _, kw, _ = call("GET", "/search?q=" + urllib.parse.quote("发布检查清单"))
        chk("归档后不出现在关键词检索里",
            any(it["document"]["id"] == target for it in kw["items"]), False)
        _, sem, _ = call("POST", "/search/semantic",
                         json_body={"query": "发布之后怎么确认没问题", "topK": 10})
        chk("归档后不出现在语义检索里",
            any(it["document"]["id"] == target for it in sem["items"]), False)

        # 「在归档区仍可找到」：归档区就是 archived=true 的那一份列表
        _, arch, _ = call("GET", "/documents?pageSize=100&archived=true")
        chk("在归档区仍能找到", any(d["id"] == target for d in arch["items"]), True)
        status, blob, _ = call("GET", f"/documents/{target}/download")
        chk("归档后仍可下载", (status, blob == (CORPUS / "05_星桥项目_发布检查清单_2026-09-18.txt").read_bytes()),
            (200, True))
    finally:
        call("PATCH", f"/documents/{target}", json_body={"archived": False})

    _, back, _ = call("GET", "/search?q=" + urllib.parse.quote("发布检查清单"))
    chk("恢复后重新出现在检索结果里",
        any(it["document"]["id"] == target for it in back["items"]), True)

    print(f"\n结果：PASS={PASS} FAIL={FAIL}")
    return 1 if FAIL else 0


if __name__ == "__main__":
    sys.exit(main())
