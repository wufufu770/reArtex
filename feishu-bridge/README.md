# reArtex 飞书通道桥

独立 Python 服务（不进 Go 单体）：飞书 WebSocket 长连接收指令 + 审批广播卡推手机。
零公网依赖——飞书长连接不需要回调 URL，桥对 reArtex 只有出向 REST。

## 能力

| 方向 | 链路 |
|---|---|
| 入向指令 | 飞书私聊 → WS 推送 → 命令路由 → reArtex REST（`/status` `/start` `/pause` `/resume` `/approve` `/deny`） |
| 出向审批 | reArtex ask pending 落库 → 广播端点 → 飞书审批卡（私聊）→ 人用 `/approve` `/deny` 裁决 |

安全纪律：
- **allowlist 宁漏勿误**：`FEISHU_ALLOWED_OPEN_IDS` 之外的私聊一律拒收并告警；空集=拒绝所有指令（审批卡仍可投递，只发收）。
- **首响锁**：同一审批多渠道/多成员并发裁决只接受第一响。
- **凭据隔离**：飞书 app_id/secret、平台 JWT 只存在于桥的环境变量——不进 reArtex 进程、不进库、不进日志。
- **广播鉴权**：reArtex→桥用 `X-Bridge-Secret` 共享口令（服务端 settings 键 `feishu_bridge_secret` 同值）。
- **决策面零新增**：裁决回调走既有 `POST /api/intercept/pending/{id}/decide`——与审批页/群聊卡同一入口，审计链不变。

## 飞书应用准备（开放平台，一次性）

1. [开放平台](https://open.feishu.cn) → 创建**企业自建应用**，记下 App ID / App Secret。
2. 应用能力 → 开启「机器人」。
3. 权限管理 → 申请 `im:message`（收发单聊消息）+ `im:message:send_as_bot`，发布版本。
4. 事件与回调 → 订阅方式选 **「使用长连接接收事件」**（这就是零端口的关键），订阅事件 `im.message.receive_v1`。
5. 把你的飞书 open_id 加进 `FEISHU_ALLOWED_OPEN_IDS`（开放平台「通讯录」或消息调试页可查）。

## 运行

环境变量：

| 变量 | 说明 |
|---|---|
| `REARTEX_BASE_URL` | 平台地址（默认 `http://127.0.0.1:8787`） |
| `REARTEX_API_TOKEN` | 平台 JWT（登录后从浏览器 localStorage `artex_token` 取） |
| `FEISHU_APP_ID` / `FEISHU_APP_SECRET` | 飞书应用凭据 |
| `FEISHU_ALLOWED_OPEN_IDS` | 逗号分隔的 open_id 白名单（**必配**，空=拒收所有指令） |
| `BRIDGE_SHARED_SECRET` | 广播口令（与服务端 settings 键 `feishu_bridge_secret` 同值；强烈建议设置） |
| `BRIDGE_HOST` / `BRIDGE_PORT` | 广播端点监听（默认 127.0.0.1:8791——只监回环） |

```bash
pip install -r requirements.txt
python3 -m bridge.main
```

服务端开关（reArtex 侧，SQL 运维通道；空=通道关闭）：

```sql
UPDATE settings SET value='http://127.0.0.1:8791/internal/approval' WHERE key='feishu_bridge_url';
UPDATE settings SET value='<与 BRIDGE_SHARED_SECRET 同值>' WHERE key='feishu_bridge_secret';
```
（settings 表无行时用 INSERT；改后即时生效，无需重启平台。）

## 验证

1. 桥日志出现「飞书 WebSocket 长连接启动」。
2. 飞书私聊机器人发 `/status` → 返回任务总览。
3. 触发一个会 ask 的工具调用（如未放行的 Bash 规则）→ 收到审批卡 → `/approve <编号>` → 平台审批页该行变已裁决。
