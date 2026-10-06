#!/usr/bin/env python3
"""把 BAAI/bge-small-zh-v1.5 导出为 ONNX，并做 int8 动态量化。

这个脚本不参与容器运行，只用来生成 services/embed/model/ 下的两个文件。
放在仓库里是为了让「模型从哪来、怎么变成 ONNX」这件事可复现，而不是一个黑箱二进制。

前置（只在开发机上装，不进容器）：
    pip install torch transformers onnx onnxruntime numpy

用法：
    # 1. 从 ModelScope 取原始权重（HuggingFace 在境内不可达）
    mkdir -p tmp/model && cd tmp/model
    for f in config.json tokenizer.json tokenizer_config.json vocab.txt \
             special_tokens_map.json 1_Pooling/config.json model.safetensors; do
      curl -sSL -o "$f" --create-dirs \\
        "https://www.modelscope.cn/api/v1/models/BAAI/bge-small-zh-v1.5/repo?Revision=master&FilePath=$f"
    done

    # 2. 导出 + 量化 + 校验
    python services/embed/tools/export_onnx.py --src tmp/model --out services/embed/model

为什么用 int8 而不是 fp32
------------------------
fp32 的 model.onnx 有 95 MB，量化后 24 MB。差别只在于要不要把模型提交进仓库：
不自带就得在镜像构建时下载，而这份 ONNX 在 ModelScope 上并不存在，只能在构建时
现装 torch 再转换 —— 那会把 `docker compose up --build` 变成一个下载几百 MB
且随时可能失败的操作。量化后直接入库，构建时只需要 onnxruntime 这一个依赖。
代价是数值上不再是逐位相同，所以脚本最后会实测两者的余弦相似度并打印出来。
"""

from __future__ import annotations

import argparse
import json
import pathlib

import numpy as np
import torch
import torch.nn as nn
from transformers import AutoModel, AutoTokenizer


class LastHiddenState(nn.Module):
    """只把 last_hidden_state 暴露出去。

    模型原本返回一个带多个字段的输出对象，导出成 ONNX 后会变成一堆无用的输出节点。
    """

    def __init__(self, model: nn.Module) -> None:
        super().__init__()
        self.model = model

    def forward(self, input_ids, attention_mask, token_type_ids):
        return self.model(
            input_ids=input_ids,
            attention_mask=attention_mask,
            token_type_ids=token_type_ids,
        ).last_hidden_state


def to_onnx(src: pathlib.Path, out: pathlib.Path) -> pathlib.Path:
    torch.set_grad_enabled(False)
    tokenizer = AutoTokenizer.from_pretrained(src)
    model = AutoModel.from_pretrained(src).eval()

    # 用一段真实中文跑通前向，顺带定下导出的示例输入
    probe = tokenizer(["把文件按项目归档"], return_tensors="pt")
    ids = probe["input_ids"]
    mask = probe["attention_mask"]
    types = probe.get("token_type_ids", torch.zeros_like(ids))

    # sequence 与 batch 都是动态的：切分后的片段长短不一，检索时又按批调用
    dynamic = {name: {0: "batch", 1: "sequence"} for name in ("input_ids", "attention_mask", "token_type_ids")}

    fp32 = out / "model.fp32.onnx"
    out.mkdir(parents=True, exist_ok=True)
    torch.onnx.export(
        LastHiddenState(model),
        (ids, mask, types),
        str(fp32),
        input_names=["input_ids", "attention_mask", "token_type_ids"],
        output_names=["last_hidden_state"],
        dynamic_axes={**dynamic, "last_hidden_state": {0: "batch", 1: "sequence"}},
        opset_version=17,
        do_constant_folding=True,
        dynamo=False,
    )
    return fp32


def quantize(fp32: pathlib.Path, out: pathlib.Path) -> pathlib.Path:
    from onnxruntime.quantization import QuantType, quantize_dynamic

    int8 = out / "model.onnx"
    # per_channel=True：每个输出通道各用一个缩放系数，而不是整个张量共用一个。
    # 体积完全一样，实测与 fp32 的余弦相似度从 0.966 提到 0.999 上下 ——
    # 整个张量共用量化范围时，权重大的通道会把小通道压成同一个整数，信息就丢了。
    quantize_dynamic(str(fp32), str(int8), weight_type=QuantType.QInt8, per_channel=True)
    return int8


def embed_onnx(session, tokenizer, texts: list[str]) -> np.ndarray:
    """CLS 池化 + L2 归一化 —— bge 系列的标准用法，见模型自带的 1_Pooling/config.json。"""
    enc = tokenizer(texts, padding=True, truncation=True, max_length=512, return_tensors="np")
    feeds = {
        "input_ids": enc["input_ids"].astype(np.int64),
        "attention_mask": enc["attention_mask"].astype(np.int64),
        "token_type_ids": enc.get(
            "token_type_ids", np.zeros_like(enc["input_ids"])
        ).astype(np.int64),
    }
    hidden = session.run(["last_hidden_state"], feeds)[0]
    cls = hidden[:, 0, :]
    return cls / np.linalg.norm(cls, axis=1, keepdims=True)


def embed_torch(model, tokenizer, texts: list[str]) -> np.ndarray:
    enc = tokenizer(texts, padding=True, truncation=True, max_length=512, return_tensors="pt")
    with torch.no_grad():
        hidden = model(**enc).last_hidden_state
    cls = hidden[:, 0, :].numpy()
    return cls / np.linalg.norm(cls, axis=1, keepdims=True)


SAMPLES = [
    "发布前要做哪些检查",
    "代码提交有什么规范",
    "上次线上故障是怎么回事",
    "接口路径怎么约定的",
    "本地起不来怎么排查",
]


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--src", default="tmp/model", help="原始模型目录")
    ap.add_argument("--out", default="services/embed/model", help="输出目录")
    args = ap.parse_args()

    src, out = pathlib.Path(args.src), pathlib.Path(args.out)
    if not (src / "model.safetensors").is_file():
        print(f"缺少 {src/'model.safetensors'}，先按脚本头部说明下载原始权重")
        return 2

    fp32 = to_onnx(src, out)
    print(f"已导出 fp32: {fp32}  {fp32.stat().st_size/1e6:.1f} MB")

    int8 = quantize(fp32, out)
    print(f"已量化 int8: {int8}  {int8.stat().st_size/1e6:.1f} MB")

    import onnxruntime as ort

    tokenizer = AutoTokenizer.from_pretrained(src)
    model = AutoModel.from_pretrained(src).eval()

    ref = embed_torch(model, tokenizer, SAMPLES)
    for tag, path in (("fp32", fp32), ("int8", int8)):
        sess = ort.InferenceSession(str(path), providers=["CPUExecutionProvider"])
        got = embed_onnx(sess, tokenizer, SAMPLES)
        dim = got.shape[1]
        # 逐句比对：整体平均会被表现好的句子拉高，看最差的那一句才有意义
        cos = [float(a @ b) for a, b in zip(ref, got)]
        print(f"  {tag}: 维度={dim} 与 PyTorch 的余弦相似度 最低={min(cos):.6f} 平均={np.mean(cos):.6f}")
        if dim != 512:
            print(f"  !! 维度不是 512，与 document_chunks.embedding 的定义不符")
            return 1

    # tokenizer.json 要一起入库：容器里只装 tokenizers，不装 transformers，
    # 直接由 tokenizer.json 还原分词器，避免拖进一整套框架。
    (out / "tokenizer.json").write_bytes((src / "tokenizer.json").read_bytes())
    meta = {
        "source": "BAAI/bge-small-zh-v1.5",
        "mirror": "ModelScope",
        "license": "MIT",
        "dimension": 512,
        "pooling": "cls",
        "normalize": "l2",
        "max_tokens": 512,
        "quantization": "dynamic int8 (QInt8 weights)",
        "note": "由 services/embed/tools/export_onnx.py 生成",
    }
    (out / "model.json").write_text(json.dumps(meta, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print("已写入 tokenizer.json 与 model.json")

    fp32.unlink()  # 中间产物，留着会让仓库多出 95 MB
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
