FROM python:3.12-slim
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY bridge/ ./bridge/
# 非root运行：凭据经环境变量注入，容器内无文件面
USER 65534
CMD ["python3", "-m", "bridge.main"]
