# 微信 iLink 任务通道（侧车）

个人微信任务通道：扫码登录 → 长轮询收消息 → allowlist → 命令面/直建任务。
**零公网依赖**（getupdates 长轮询拉模式，无回调 URL），与 email-intake 同架构。

## 信任语义

`WECHAT_ALLOWED_USERS` 白名单内的 user_id = 可信操作者（与飞书 `/start`、
邮箱通道同层）；白名单外拒收并告警。**空集拒绝启动（宁漏勿误）**。

## 协议来源与风险

协议基于 2026-03 社区逆向文档（`@tencent-weixin/openclaw-weixin` 1.0.2，
iLink 私有协议）。字段名全部集中在 `ilink.py` 的 `FIELD` 映射区——实测有
出入时只改那一处。注意：

- 腾讯条款保留限制/拦截/终止权（4.7/7.2）——核心业务别只依赖此通道；
- 群聊（group_id）可能需要额外权限，本侧车只接 1v1 消息；
- 未公开速率限制，长轮询自然限频。

## 环境变量

| 变量 | 必填 | 说明 |
|---|---|---|
| `WECHAT_ALLOWED_USERS` | 是 | 允许操作的 user_id 列表（逗号分隔；`xxx@im.wechat`）。**空集拒绝启动**。首次可先空跑一次看日志里的拒收告警拿 user_id，再配进来 |
| `ILINK_TOKEN_FILE` | 否 | 登录态持久化文件（默认 `./ilink_state.json`），含 bot_token + 游标 |
| `REARTEX_BASE_URL` | 否 | reArtex 地址（默认 `http://127.0.0.1:8787`） |
| `REARTEX_API_TOKEN` | 否 | reArtex API token（有认证部署时必填） |
| `BRIDGE_NOTICE_URL` | 否 | 飞书桥 `/internal/notice` 地址（任务创建通知，可选） |
| `BRIDGE_SHARED_SECRET` | 否 | 飞书桥共享口令（配了 notice_url 就要配） |

## 运行

```bash
pip install -r requirements.txt
export WECHAT_ALLOWED_USERS="xxx@im.wechat"
python main.py
```

首次启动（无登录态）会在终端打印二维码内容——把它转成二维码（任意二维码
生成器）用微信扫码，确认后 token 自动持久化，下次启动免扫码。

## 命令面

| 命令 | 说明 |
|---|---|
| `/tasks` | 列最近 5 条任务 |
| `/create 描述 \| 目标` | 直建任务 |
| `/help` | 命令说明 |
| 其它裸文本 | 直建任务（正文要点 ≤300 字符，与邮箱通道同口径） |
