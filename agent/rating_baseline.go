package agent

// RatingBaselineDoc 是漏洞评级基准的【唯一语义源】（E1-7，方案 v10 §9）。
//
// 为什么从 report_finding 的工具描述里抽出来：基准此前只在工具描述里——worker /
// planner 的系统提示词完全看不到它，模型要在「决定报不报」的当下却拿不到判级
// 标准（只有真调 report_finding 传错了 severity 才会被报错文案教一次）。抽成常量
// 后三处共用同一语义：① worker/planner 系统提示词开篇注入；② report_finding
// 描述保留全文（历史口径）；③ severity 非法值的报错文案带短版。
// 防漂移：rating_baseline_test.go 断言三处都含基准核心句。
const RatingBaselineDoc = "\n\n【漏洞评级基准（拿不准降一档）】critical=可验证敏感数据外泄/RCE/数据库完全控制/认证绕过接管/支付金额篡改/大规模数据泄露；high=可验证 SQLi 可读数据/SSRF 可达内网或云元数据/IDOR 可读他人敏感数据/未授权后台/可执行文件上传/反序列化 RCE；medium=有前置条件的利用（存储型 XSS 有场景/敏感操作 CSRF/可利用 CORS 泄露了具体数据/目录遍历有限读取）；low=需用户交互（反射 XSS）/仅影响自己/有限 CSRF/带凭据或利用链的未授权读取（可下载数据库凭据、可读写他人数据）；info=【纯信息泄露与暴露类一律 info，不算低危】：cookie/日志/备份/配置文件可读但不含可用凭据或利用链、版本号/技术栈/中间件指纹、目录列表、报错回显、侦察线索、未证实候选面（如「疑似注入面未取得 sink」「未验证的开放重定向参数」）——漏洞类别是信息泄露（info_disclosure）且没有拿到凭据或可继续利用的链，severity 一律写 info 并说明缺什么才升级。\n\n【绝对不报（0 元）】CORS 跨域配置（除非证明窃取了具体数据）、Sourcemap 泄露、HTTP 安全头缺失（X-Frame-Options/CSP）、版本号/中间件指纹、Self-XSS、SSL/TLS 配置警告（除非严重降级）、单独的开放重定向（无链式利用）、Rate limiting 缺失、任何没有 PoC 的发现。\n\n【组合升权】单独低价值可组合变高价值：信息泄露+CVE=可利用 1day；XSS+CSRF=账户接管；SSRF+云元数据=云接管；JWT 密钥泄露+伪造=认证绕过；文件上传+解析漏洞=webshell；IDOR+批量遍历=大规模泄露。有组合链时在 summary 写明链条并按组合后结果评级。"
