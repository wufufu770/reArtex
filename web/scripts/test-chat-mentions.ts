// mention 纯逻辑测试（ts-node 跑，零依赖）。
//
// 覆盖两块：
//   1. 触发判定（activeMention）—— 触发字符参数化后，"/" 只在行首或空白后命中，
//      "@" 保持旧规则（允许紧贴单词）。
//   2. token 分流（mentionToken）—— agent 类落成裸 "@agentkey "（后端定向必醒靠它），
//      记录类落成 "@[kind#id label]"（引用解析靠它）。
//
// 用例里的每一条都是「改了实现就会红」的护栏：旧 @ 的行为不许变。

import { activeMention, mentionKinds, mentionSearch, mentionToken } from "../src/lib/chat-mentions";
import assert from "node:assert/strict";

// ---- 触发判定：/ 只在行首或空白后 ----
assert.equal(activeMention("/pen", 4, "/")?.query, "pen");
assert.equal(activeMention("hi /pen", 7, "/")?.query, "pen");
assert.equal(activeMention("curl /usr/bin", 12, "/"), null, "路径不能触发");
assert.equal(activeMention("a/b", 3, "/"), null, "单词中间的斜杠不能触发");
assert.equal(activeMention("/", 1, "/")?.query, "");
assert.equal(activeMention("第一行\n/pen", 8, "/")?.query, "pen", "换行后算行首");

// ---- 默认仍是 @，且允许紧贴单词（邮箱@x 这类） ----
assert.equal(activeMention("邮箱@x", 4)?.query, "x");
assert.equal(activeMention("x @pen", 6)?.query, "pen");

// ---- 旧 @ 行为回归（改动前就成立，改动后必须还成立） ----
assert.equal(activeMention("user@example.com", 16), null, "邮箱不能触发 @");
assert.equal(activeMention("@漏洞 SQL", 8)?.query, "漏洞 SQL", "查询串允许含空格（类型别名 + 名称）");
assert.deepEqual(activeMention("查看@漏洞 后面的文字", 5), { start: 2, end: 5, query: "漏洞" });
assert.equal(activeMention("@漏洞\n下一行", 8), null, "换行结束查询");
assert.equal(mentionSearch("").categories.length, 9, "可搜索的记录类型仍是 9 类（成员不参与服务端搜索）");
assert.equal(mentionSearch("漏").categories[0].kind, "finding");
assert.equal(mentionSearch("漏洞").kind, "finding");

// ---- token 分流 ----
assert.equal(mentionToken({ kind: "agent", id: -1, label: "渗透", description: "", agentKey: "pentest" }), "@pentest ");
assert.equal(mentionToken({ kind: "finding", id: 12, label: "SQL 注入", description: "" }), "@[漏洞#12 SQL 注入]");

// ---- 成员组排在最前 ----
assert.equal(mentionKinds[0].kind, "agent", "成员组应排在最前");
assert.equal(mentionKinds[0].label, "成员");

console.log("chat-mentions 逻辑测试通过");
