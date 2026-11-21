---
name: audit-config-hardening
description: 配置与暴露面加固核查：密钥泄露、CORS/TLS 缺陷、不安全默认值、敏感文件暴露的系统性盘点。用户要求「配置审查/暴露面盘点/信息泄露审计」时加载。
---

# 配置与暴露面加固核查

在**已授权**前提下，系统性盘点目标的**配置缺陷与不该暴露的暴露面**。产出纪律与平台一致：单独的「安全头缺失/版本指纹」是 0 元信息不单独报——**本 skill 的价值在于把配置线索升级成可利用链**（泄露的密钥 → 接管；开放的调试面 → RCE）。

## 与平台工作流的分工

- 每一项核查结果先 `record_fact`（observed/inferred + 证据一行）；只有组合出**可利用链**时才 `report_finding`。
- 组合升权是本 skill 的核心产出方式：配置缺陷单看是 info，链起来是 high/critical（平台 report_finding 的组合升权口径）。

## 六个盘点面

### 1. 密钥与凭据泄露

- 前端产物：JS/SourceMap 里的 API key、第三方服务 token（按前缀对号：AKIA/gsk/sk-/ghp/AIza…）、注释里的测试凭据；
- 目录遍历常见点位：`.env`、`.git/config`、`config.php.bak`、`/server-status`、dump 文件（`*.sql`、`*.dump`、`backup.zip`）；
- **拿到密钥必须验证有效性**（用它实际调一次对应服务——限授权范围内）才升级 finding；无效密钥记 fact。

### 2. CORS 与跨域

- `Access-Control-Allow-Origin` 反射 Origin + `Allow-Credentials: true` = 可窃取带凭据响应——**必须用实际 Origin 头验证反射**，别信文档；
- 通配 `*` + credentials 并存（浏览器会忽略，但记 fact）；
- origin 子域通配（`*.target.com`）时核查可注册/可 XSS 的子域是否命中通配。

### 3. TLS 与 HTTP 行为

- 降级接受（http 到敏感端点的 301 跳转前是否已泄凭据）、HSTS 缺失（fact 级）；
- 非常规端口/隐藏主机上的旧服务（证书透明 + 端口扫描面已由 recon 铺，这里只核对应用层）。

### 4. 调试与管理面

- `/debug`、`/actuator`（env/heapdump/gateway）、`/?debug=1`、phpinfo、Swagger/GraphQL playground 暴露；
- **暴露 + 信息可取 = 已有链**（actuator env 泄露密钥直接升级）；只暴露不可取记 fact。

### 5. 不安全默认值

- 默认凭据（admin/admin、nacos/nacos 等按产品对照表）——**登录成功才算 finding**；
- 示例应用/安装页残留（install.php 可重装、setup 向导未删）；
- 目录列表开启且目录内有敏感内容。

### 6. 错误处理泄露

- 栈回显（带框架版本/物理路径/SQL 片段）、详细错误码把「存在性探测」变成免费 oracle（报错区分用户不存在 vs 密码错）；
- 泄露内容能拼出链的（路径 → 配合其它 LFI）升级，裸报错记 fact。

## 纪律

- **零元项不单独报**：安全头缺失、版本号、Cookie 属性缺失、目录列表无敏感内容——落 fact 供组合，不占 finding 带宽（与 report_finding 的绝对不报清单一致）。
- 密钥验证只在授权范围内、最小动作（能证明有效即停，不横向用）。
- 发现的凭据/密钥不进 fact 明文——写「形态 + 后四位 + 有效与否」，完整值留在 evidence 密文面（遵循平台脱敏口径）。
