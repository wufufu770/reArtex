<!--
  发布说明：打 tag 前把这份文件改成新版本的内容，release workflow 会把它作为
  Release 正文（自动生成的提交列表会附在其后）。
  写「这一版装了什么 / 变了什么 / 还欠着什么」，别写提交列表（那是自动的）。
-->

**reArtex v0.4.1** —— W-A 波次：把「已登记的活路径」逐条闭合或如实降级。**功能面不变**（执行模型、任务编排、探索引擎、对外 API 保持上游原样），动的是边界、留痕与交付面。

## 判定层：分级审核 + 审批治理

- **短路表三档化**：`deny`（凭据汇 / 读控制面秘密）、`ask`（真高危：改可执行面、改既有行为、删证据库）、**显式 allow**（低危日常：新建技能文件）。
  显式 allow 是代码常量**主动判定放行**、仍插在判定最前求值 —— 与「不命中短路表」的漏判有本质区别，故「平台工具绕白名单」的原漏洞不重开；allow 档判定也进审计链。
- **DB 规则的 ask 类补默认超时**（60 秒，超时即拒）：种子规则照抄 `timeout_enabled=false` 就是「审批无限等待」，该窗口关闭。
- **审批防疲劳计数**：同一 agent 在时间窗内反复对同一高危面请求审批，超阈值才自动拒绝（默认 5 次 / 600 秒，可热改）。单次或少量请求**完全不受影响**。

## 路径判定：Bash 面不再只靠字符串预筛

- 入参里的路径片段现在**按工具工作目录解析成真实路径**再判定：`cat ../../../jwt.key`、`cat $(realpath ../../../jwt.key)` 这类经 shell 展开才现形的路径已被拒，软链与父目录软链也覆盖。判定点仍在工具策略层（Stage 1 Deny，任何 permission mode 绕不过）。
- **工具层的拒绝接入审计链**：该层早于 `PreToolUse` hook 求值（hook 不会被调用），原先拿不到留痕；现由 server 注入的记录器补一条，链的规范化与串行化与其他判定动作一致。

## 凭据供给面

- **数据库口令改由 docker secret 文件供给**（`/run/secrets/pg_dsn`）：`docker inspect` 不再看得见口令。配套：`/run/secrets` 进平台秘密清单（agent 读不到）、只读挂载的 DSN 文件不再要求「读入即销毁」。
- **`llm_profiles.api_key` 可加密落库**（可选）：AES-256-GCM，主密钥由 `ARTEX_MASTER_KEY_FILE` 供给（不进环境块、不进库），历史明文行启动时自动迁移；AAD 绑定行名，改名触发重加密。未配主密钥时行为与从前完全一致。

## 门禁与交付

- **发布冒烟新增环境块洁净性阻断门**：每次发布都验「应用进程树的环境块内无非空平台秘密」。
- 工具层拒绝的留痕缺口补齐后，「谁在何时想干什么」在审计链上可查。

## 如实降级（未闭合）

- **`/proc/<pid>/mem` 读同 UID 进程内存仍可做到**：同 UID 的 ptrace **不需要** `CAP_SYS_PTRACE`，故 `cap_drop` 挡不住它（实测确认）。真正的闭合要让工具进程与 artex **不同 UID**（属 per-task 隔离那一档）。镜像里留了 `verify-ptrace-blocked.sh` 供自查，它**故意不进发布门**。

## 升级须知

- **compose 部署需要 `./init-pg-dsn-secret.sh` 这一步**（由 `.env` 生成 secret 文件），且**镜像至少要 v0.4.1** —— 更早的镜像不认只读 secret 挂载，会崩溃重启。
- 完整说明见 [README](https://github.com/wufufu770/reArtex#重点改了什么) 与 [CHANGELOG](https://github.com/wufufu770/reArtex/blob/main/CHANGELOG.md)。

**Full Changelog**: https://github.com/wufufu770/reArtex/compare/v0.4.0...v0.4.1
