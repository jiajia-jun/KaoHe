#!/usr/bin/env python3
"""文本向量服务。

一个只做一件事的常驻进程：收一批文本，返回每条的 512 维向量。
Go 侧的 worker 通过 HTTP 调用它，自己不链接任何推理库 ——
ONNX Runtime 是 C++ 生态，和 Go 绑在一起会把构建复杂度全压到主镜像上。

为什么用标准库的 http.server 而不是 FastAPI
--------------------------------------------
这里只有两个接口，没有路由分组、依赖注入、序列化中间件这类需求。
换成框架要多装三个包，而镜像里每多一个包就多一处构建时可能出问题的地方。
ThreadingHTTPServer 已经能应付并发——ONNX Runtime 的 session.run 本身是线程安全的。

接口
----
    GET  /healthz  -> {"status":"ok","dimension":512,...}
    POST /embed    -> 请求 {"texts":["...","..."]}
                      响应 {"dimension":512,"count":2,"vectors":[[512 个浮点],...]}

向量已做 L2 归一化，因此检索端用余弦距离即可，与 pgvector 的 vector_cosine_ops 一致。
"""

from __future__ import annotations

import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import numpy as np
import onnxruntime as ort
from tokenizers import Tokenizer

MODEL_DIR = Path(os.environ.get("MODEL_DIR", Path(__file__).resolve().parent / "model"))
PORT = int(os.environ.get("EMBED_PORT", "8000"))
# 一个批次最多几条。上限存在的意义是别让单个请求把内存吃光，
# worker 那边按 16 条一批发，远小于这个数。
MAX_BATCH = int(os.environ.get("EMBED_MAX_BATCH", "64"))
MAX_CHARS = 2000  # 单条文本的上限，超出直接截断；512 token 本来也装不下更多

# BERT 系的特殊符号编号。词表里是固定的，写死比每次去查词表便宜。
PAD_ID, PAD_TOKEN = 0, "[PAD]"


class Embedder:
    """模型与分词器。进程启动时装载一次，之后只读。"""

    def __init__(self, model_dir: Path) -> None:
        onnx_path, tok_path = model_dir / "model.onnx", model_dir / "tokenizer.json"
        for path in (onnx_path, tok_path):
            if not path.is_file():
                raise FileNotFoundError(f"缺少模型文件 {path}")

        meta = json.loads((model_dir / "model.json").read_text(encoding="utf-8"))

        opts = ort.SessionOptions()
        # 与 0.0.0.0 上的并发请求数相匹配：单个请求内不再开线程，
        # 让不同请求真正并行，而不是在同一个核上互相抢。
        opts.intra_op_num_threads = int(os.environ.get("EMBED_THREADS", "0")) or 0
        self.session = ort.InferenceSession(
            str(onnx_path), sess_options=opts, providers=["CPUExecutionProvider"]
        )

        # 分词器只用 tokenizers 库从 tokenizer.json 还原，不引入 transformers：
        # 后者会连带拖进 torch，镜像直接涨到 1 GB 以上，而这里只需要分词。
        self.tokenizer = Tokenizer.from_file(str(tok_path))
        self.tokenizer.enable_truncation(max_length=meta["max_tokens"])
        self.tokenizer.enable_padding(pad_id=PAD_ID, pad_token=PAD_TOKEN)

        self.dimension = int(meta["dimension"])
        self.model_name = meta["source"]
        self._lock = threading.Lock()  # 只为串行化日志，推理本身不需要锁

    def embed(self, texts: list[str]) -> list[list[float]]:
        encodings = self.tokenizer.encode_batch([t[:MAX_CHARS] for t in texts])
        feeds = {
            "input_ids": np.array([e.ids for e in encodings], dtype=np.int64),
            "attention_mask": np.array([e.attention_mask for e in encodings], dtype=np.int64),
        }
        # bge 是单句模型，token_type_ids 全 0；导出时刻意保留了这支输入
        feeds["token_type_ids"] = np.zeros_like(feeds["input_ids"])

        hidden = self.session.run(["last_hidden_state"], feeds)[0]
        # CLS 池化（取首 token）再 L2 归一化，与模型自带的 1_Pooling/config.json 一致
        vectors = hidden[:, 0, :]
        norms = np.linalg.norm(vectors, axis=1, keepdims=True)
        vectors = vectors / np.maximum(norms, 1e-12)
        return vectors.tolist()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    embedder: Embedder  # 由 serve() 注入

    def log_message(self, fmt, *args):  # 默认格式没有时间戳，换成单行访问日志
        sys.stderr.write("embed %s %s\n" % (self.address_string(), fmt % args))

    def _send(self, status: int, payload: dict) -> None:
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:  # noqa: N802 —— 标准库要求的命名
        if self.path.split("?")[0] != "/healthz":
            self._send(404, {"code": "not_found", "message": f"未知路径 {self.path}"})
            return
        self._send(200, {
            "status": "ok",
            "service": "embed",
            "model": self.embedder.model_name,
            "dimension": self.embedder.dimension,
            "pooling": "cls",
            "normalized": True,
        })

    def do_POST(self) -> None:  # noqa: N802
        if self.path.split("?")[0] != "/embed":
            self._send(404, {"code": "not_found", "message": f"未知路径 {self.path}"})
            return

        length = int(self.headers.get("Content-Length") or 0)
        if length <= 0 or length > 8 << 20:
            self._send(400, {"code": "bad_request", "message": "请求体为空或过大"})
            return
        try:
            payload = json.loads(self.rfile.read(length).decode("utf-8"))
            texts = payload["texts"]
            if not isinstance(texts, list) or not texts:
                raise ValueError("texts 必须是非空数组")
            if not all(isinstance(t, str) for t in texts):
                raise ValueError("texts 的元素必须是字符串")
        except (ValueError, KeyError, UnicodeDecodeError, json.JSONDecodeError) as exc:
            self._send(400, {"code": "bad_request", "message": f"请求体不合法：{exc}"})
            return

        if len(texts) > MAX_BATCH:
            self._send(400, {
                "code": "bad_request",
                "message": f"单次最多 {MAX_BATCH} 条，收到 {len(texts)} 条",
            })
            return

        try:
            vectors = self.embedder.embed(texts)
        except Exception as exc:  # 推理失败属于服务端问题，让 worker 走重试
            self.log_message("推理失败: %r", exc)
            self._send(500, {"code": "internal_error", "message": "向量生成失败"})
            return

        self._send(200, {
            "dimension": self.embedder.dimension,
            "count": len(vectors),
            "vectors": vectors,
        })


def main() -> int:
    embedder = Embedder(MODEL_DIR)
    Handler.embedder = embedder
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    print(
        f"embed 已启动 端口={PORT} 模型={embedder.model_name} 维度={embedder.dimension}",
        flush=True,
    )
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
