# 向量边车镜像。
#
# 单独一个镜像而不是塞进主镜像：主镜像是 distroless 风格的静态 Go 二进制，
# 而这里需要 Python 运行时与 ONNX Runtime 的 .so，两者的基础镜像与升级节奏都不一样。
#
# 模型文件直接打进镜像（见 model/model.json 说明来源），因此构建过程不需要联网取模型，
# 整个 `docker compose up --build` 对外部的依赖只剩拉取基础镜像与 PyPI 的这几个包。
FROM python:3.12-slim

# onnxruntime 的 CPU 版依赖 libgomp 做多线程推理，slim 镜像里没有，必须显式装上
RUN apt-get update \
 && apt-get install -y --no-install-recommends libgomp1 \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

COPY app.py ./
COPY model/ ./model/

# 模型加载失败要立刻让容器退出，而不是起一个「健康但不会算」的服务
ENV MODEL_DIR=/app/model \
    EMBED_PORT=8000 \
    PYTHONUNBUFFERED=1

EXPOSE 8000

# 没有 curl，用 python 自带的 urllib 探活，省掉一个依赖
HEALTHCHECK --interval=10s --timeout=5s --retries=12 --start-period=20s \
  CMD python -c "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:8000/healthz', timeout=3).status==200 else 1)"

CMD ["python", "app.py"]
