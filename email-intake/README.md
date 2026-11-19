# reArtex 邮箱任务通道

独立 Python 侧车（不进 Go 单体）：IMAP 轮询授权发件人的邮件 → 直建 reArtex 任务
（来源标注在任务描述里）→ 可选经飞书桥 `/internal/notice` 推创建通知。

## 信任语义

**allowlist 内发件人 = 可信操作者**（与飞书 `/start` 命令同一信任层级）：邮件到达
即建任务，描述自动标注「邮件任务（来自 <发件人>，主题：…）」。allowlist 外发件人
拒收并告警——宁漏勿误。任务目标 = 邮件主题 + 正文要点（≤300 字符）。

已知邮件 UID 落本地状态文件（`STATE_FILE`），重启不重复处理。

## 环境变量

| 变量 | 说明 |
|---|---|
| `IMAP_HOST` / `IMAP_PORT` / `IMAP_USER` / `IMAP_PASSWORD` | IMAP 收件箱（端口默认 993 SSL） |
| `EMAIL_ALLOWED_SENDERS` | 逗号分隔的授权发件人地址（与域名至少配一项） |
| `EMAIL_ALLOWED_DOMAINS` | 逗号分隔的授权域名（整域放行） |
| `POLL_INTERVAL_SEC` | 轮询间隔（默认 120s） |
| `STATE_FILE` | 已处理 UID 状态文件（默认 ./email_state.json） |
| `REARTEX_BASE_URL` / `REARTEX_API_TOKEN` | 平台地址 + JWT |
| `BRIDGE_NOTICE_URL` | 飞书桥 `/internal/notice`（可选；空=不推通知） |
| `BRIDGE_SHARED_SECRET` | 桥共享口令（与桥侧同值） |

```bash
pip install -r requirements.txt   # 仅 httpx（IMAP/email 走标准库）
python3 main.py
```

## 邮件格式约定

- **主题** = 任务目标主干；**正文** = 要点补充（自动截取前 300 字符）。
- 目标中出现的主机/域名会被平台既有的 scope/seed 链路解析（与手工建任务一致）。

## 验证

1. allowlist 发件人发一封主题为目标的邮件。
2. 侧车日志出现「邮件任务已创建 #N」。
3. 平台任务列表出现该任务（描述带来源标注）。
