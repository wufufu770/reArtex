**reArtex v0.13.1** —— 桌面控制（平台共有能力）与小程序渗透两大功能点全量落地；前后端对齐修复与界面文案修剪。

## 桌面控制（平台共有能力）

- agent-desktop（读无障碍树 + 稳定 ref）以 stdio MCP 注册：种子按 LookPath 解析（未安装跳过不落旗标，装好后下次启动补种）；可见性=全部 agent。
- ExecuteExtraTool 转发治理闭环：延迟工具经外层转发器调用时，guard/规整按载荷内层实际工具名与参数求值——启用名单、规则匹配、审计与审批回归真实身份；载荷畸形回退外层名（执行侧同拒，无旁路窗口）。
- 拦截系统接入：启用名单 8+75=83 项；四族规则——观测类 allow（12）/ 动作类 ask（33）/ 敏感读取类 ask（3：截图/剪贴板/通知）/ 高危类 deny（16）；以实测 75 个真实工具名快照钉死族间互斥与全覆盖，上游加新命令时测试失败提醒分族。
- 敏感输出元信息化：desktop_screenshot / clipboard_get / list_notifications 的持久层正文替换为元信息（工具名/字节数/时间），模型上下文不受影响。
- 「桌面操作」skill（mcps: agent-desktop 门控）：命令族说明、ref 重快照纪律、渗透场景用法。
- 闸门第五分支 HostLocalAction（任务路径桌面动作解锁）：任务宿主授权集合（tasks.host_targets；REST `PUT /api/tasks/{id}/host-targets` + 任务详情「业务建模」tab 编辑器）非空时桌面动作 scope 按构造通过，其余四道检查照跑；未配置集合的桌面动作在规整前以独立理由拒绝。授权变更写审计链。语义边界（如实）：任务级粗粒度授权 + guard 层逐动作人工审批的组合；窗口级精控排后续。

## 小程序渗透

- token 池：miniprogram_tokens 表（UNIQUE(task,app,label)），加密落库 AAD 绑 app:label；transcript 与审计面只有标签与指纹，明文仅在构造 Burp 重放 replacement 的 reveal 时刻进工具结果。
- 作用域工具 mp_token_save / mp_token_list / mp_token_get(reveal)（task_id 必填）+ REST（tokens CRUD、`GET /api/miniprogram/artifacts` 产物浏览）；保存与 reveal 走审计哈希链，凭据值不进审计载荷。
- read_design_artifacts 扩类：消费解包源码 app.json（页面注册树）与 project.config.json（编译开关）——建模 agent 直接做设计层。
- skills/wechat-miniprogram：解包后端探针（实测可用性）、包发现（版本分支）、解包适配器、配置矫正（五关两开 + componentFramework/plugins）、内容级校验（退出码即判定）；包获取为人工口子。
- 专用 miniprogram agent（六阶段：包获取→解包还原→配置矫正→运行时→接口发现→越权矩阵→结论复核；阶段→kind 映射）。
- chrome-devtools MCP 种子（LookPath，未安装跳过）——接口发现阶段的浏览器操控入口。
- 内嵌浏览器：`/mp-runtime/` 反代到 wxa-runner dev server（settings `miniprogram_runtime_url`，操作者经 REST 设置，agent 不可改）；小程序渗透页内嵌面板同源直接调试点击。SSRF 面关闭：scheme 仅 http/https、单固定目标（路径转发 host 恒等配置值）。wxa-runner 需 Node≥18。

## 前端修复与文案修剪

- sidebar 补「小程序渗透」入口；skill 绑定种子修复（desktop-ops / wechat-miniprogram 此前未绑任何 agent，桌面动作面与小程序链路不可用）。
- 偏锋探索入口去查询参数：sidebar 指向 /function/business-graph，模式由页内「业务图 / 偏锋探索」标签切换；老路径 308 兼容链保留。
- 界面文案修剪（业务建模图 / 差异面板 / 偏锋总览 / 小程序渗透 / 任务建模 tab）：砍标语、提示文字、次要徽章，空态只留一句事实；零逻辑改动，`git revert` 单提交整体回滚。

## 兼容与升级

- 全新库首启自动播种：miniprogram agent、桌面拦截规则与 83 项启用名单、mp 工具与绑定（chrome-devtools 已安装才种）。
- 桌面动作默认拒绝：tasks.host_targets 未配置 = 拒绝；需任务级配置宿主授权集合，动作面另经 guard 逐动作审批。
- DDL 幂等自动应用（db/schema_miniprogram.sql 走 Ensure*Schema 启动接线），对既有库无破坏性。
- 工作目录固化不变：版本更新只换二进制，解包产物落 data_dir/artifacts/miniprogram/<appid>/，工作空间页可见。

## 验证

- 真库集成测试（biz/edge/desktop 往返、TestEdgeTasksOverview）；intercept 解包三钉、desktop 分族覆盖钉、sod 闸门分支钉；db/server 包回归；go build/vet、tsc、biome 全绿。
- 镜像交付门（CI 内建）：编译+vet+全量测试通过才产出二进制；ghcr 多架构镜像发布后冒烟——真实起服务、/api/health 版本核对、环境块洁净性验收。
