---
name: 小程序渗透
description: 微信小程序渗透全链：包获取（人工口子）→ 解密解包还原源码工程 → wxa-runner 运行时 → 接口发现 → 越权矩阵（浏览器发现 + Burp 重放）。产物回写 assets/findings/业务建模。
tags: [miniprogram, wechat, mobile, pentest, wxapkg]
---

# 小程序渗透

对微信小程序做从包到接口的完整渗透。链路：**包获取（人工口子）→ 解密解包还原 → 配置矫正 → 运行时 → 接口发现 → 越权矩阵 → 结论回写**。每一步的裁决都在平台确定性层（ask 门/评级基准/kind 容量闸），你不自行判定失败与切换。

## 阶段与意图种类

| 阶段 | kind | 内容 |
|---|---|---|
| 包获取 | recon | **人工口子**：人供 wxapkg 文件，或保持微信进程存活后跑 scripts/discover_package.sh（密钥一部分在微信进程内存里，不能全自动） |
| 解包还原 | recon | scripts/unpack.sh：解密+解包+反编译还原+配置矫正+内容级校验，产物落 artifacts/miniprogram/<appid>/ |
| 运行时 | recon | wxa-runner 吃还原后的源码工程（Node≥18；基础库 3.7.0 默认可用）；`?path=pages/detail/detail&id=1` 直达页面 |
| 接口发现 | recon | chrome-devtools-mcp（stdio）驱动浏览器做一次认证态的接口发现与敏感信息提取 |
| 越权矩阵 | exploit | **Burp MCP（sse）重放层**：同一批已发现请求逐个换 token 池角色凭据，比对响应差异——差异成立即越权证据 |
| 结论复核 | verify | 发现过评级基准与 severity/status 白名单落库；业务逻辑类差异可交业务建模出 biz_layer_diffs |

## 包获取（人工口子，不可省）

解密密钥 = PBKDF2(wxid, "saltiest", 1000 轮) 派生 AES 密钥 + 头 1024 字节 AES-CBC + 其余字节按 wxid 倒数第二位单字节异或；**新版微信要从运行中进程内存取密钥**——微信进程必须存活。跑 `bash skills/wechat-miniprogram/scripts/discover_package.sh` 做路径探测（按微信版本分支），拿到包后走 unpack.sh。**不要**试图让 agent 自行完成密钥提取闭环。

## 解包与校验（失败会伪装成成功）

`bash skills/wechat-miniprogram/scripts/unpack.sh <wxapkg路径> <appid>`：
1. 解密解包（后端由 scripts/probe_unpackers.sh 实测决定：wux1an CLI / Gwxapkg / Wedecode / unveilr fork）；
2. 反编译还原编译态（app-service.js / page-frame.js 的 $gwx 生成函数）→ 源码工程；
3. 配置矫正：project.config.json 的 es6/postcss/minified/enhance/minifyWXML 五关，ignoreUploadUnusedFiles/ignoreDevUnusedFiles 两开（否则动态 require 被依赖分析剔除，页面注册整体失败）；app.json 的 componentFramework 必须是字符串、plugins 的 subpackage 字段删除、`__plugin__` 页面从 pages 剔除；
4. **内容级校验（退出码即判定）**：wxml 非空且不含页面路径占位符、componentFramework 是字符串、app.js 可静态解析。工具退出码 0 + 日志满屏 Saved file **不代表成功**——以校验为准。

产物落 `artifacts/miniprogram/<appid>/`（工作空间页可见）。**二次解包**：新 app 资产登记后用 insert_assets 的新记录 + 平台资产取代链（superseded）表达版本演进，不要手改旧记录。

## 越权矩阵（两层拆分）

- **浏览器层（一次）**：setup.ts 在启动前预置基准账号凭据 → 接口清单、参数结构、响应特征。逐角色切 token 需要反复重启运行时——不在浏览器层做矩阵。
- **重放层（矩阵）**：token 池（mp_token_save 按 admin/userA/userB/anonymous 存，transcript 只有标签与指纹）→ Burp resend_with_replacements 逐请求换凭据 → 响应差异逐条记录。**同一请求、不同身份、响应差异**才是越权证据，不是一个布尔结论。
- Burp 扩展发包审批默认开启（可按主机放行）；跨角色破坏性请求**必须走任务路径**（ask 门 + 审计），会话路径不发起真实请求。

## 桌面控制（微信 PC 客户端）

客户端内嵌小程序页的原生交互（原生对话框、客户端鉴权态、设置面板）用**桌面控制**通道：加载「桌面操作」skill 解锁动作面（观测类会话开局即有）。任务路径上的桌面动作需平台闸门的宿主授权分支就位；会话路径直接可用且走 ask 审批。

## 证据纪律

- findings 必须能回放到证据：HTTP 类绑 traffic（baseline/proof/verification/supporting），未复核的发现进不了报告（评级基准把未证实候选面降为 info）。
- 业务逻辑缺陷占严重 web 漏洞大头且模式匹配看不见——它们以 biz_layer_diffs + 可回放证据的组合呈现，不接受"看着像结论"的输出。
- 跨角色破坏性请求、修改密码、删除数据：一律停，等人授权。
