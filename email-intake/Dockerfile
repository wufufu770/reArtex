FROM python:3.12-slim
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY intake.py main.py ./
# 非root运行：IMAP 口令经环境变量注入，状态文件挂卷
USER 65534
CMD ["python3", "main.py"]
