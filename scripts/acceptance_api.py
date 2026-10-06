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

PASS = 0
FAIL = 0
BASE_URL = DEFAULT_BASE


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
        ["docker", "compose", "exec", "-T", "db", "psql", "-U", "kaohe", "-d", "kaohe", "-tAc", query],
        capture_output=True,
        text=True,
        cwd=REPO_ROOT,
    )
    if result.returncode != 0:
        raise RuntimeError(f"查询失败：{result.stderr.strip()}")
    return result.stdout.strip()


def reset() -> None:
    # categories 必须一起清：documents 引用它，单独清 documents 会把它留下，
    # 后面的分类用例就会带着上一轮的残留数据开始
    sql("TRUNCATE documents, index_jobs, document_chunks, categories RESTART IDENTITY CASCADE;")
    subprocess.run(
        ["docker", "compose", "exec", "-T", "api", "sh", "-c", "rm -rf /data/uploads/*"],
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

    print(f"\n结果：PASS={PASS} FAIL={FAIL}")
    return 1 if FAIL else 0


if __name__ == "__main__":
    sys.exit(main())
