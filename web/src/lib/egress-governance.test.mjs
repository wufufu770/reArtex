import { egressGovernanceView } from "./egress-governance.ts";
import assert from "node:assert/strict";
import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

// 禁用措辞必须**拼接构造**，不能直接写成字面量。
//
// 这个用例扫描的目录包含它自己；清单里若直接写着那个词，扫描器必然命中自己
// —— 那是自伤，不是被扫描物有问题。拼接之后，扫描仍能覆盖包括本文件在内的
// 全部源文件（更强的断言：那个词在整个 src/ 里都不出现）。
const FORBIDDEN_CLAIM = "出网" + "已管控";
const LIB_DIR = dirname(fileURLToPath(import.meta.url));
const SRC_DIR = join(LIB_DIR, "..");
const REPO_ROOT = join(SRC_DIR, "..", "..");
const SELF_FILE = relative(REPO_ROOT, fileURLToPath(import.meta.url));

const CHANNELS = [
  "通道甲：这条通道没有任务身份，不受任务范围校验",
  "通道乙：这条通道不经过本平台的出网策略",
  "通道丙：这条通道出厂即关闭，未在生效",
];

function collectSourceFiles(dir) {
  const files = [];
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) {
      files.push(...collectSourceFiles(full));
    } else {
      files.push(full);
    }
  }
  return files;
}

function readSourceFiles(dir) {
  return collectSourceFiles(dir)
    .filter((file) => /\.(ts|tsx|mjs|js|css|json)$/.test(file))
    .map((file) => ({ file: relative(REPO_ROOT, file), text: readFileSync(file, "utf8") }));
}

test("a non-empty uncovered-channel list is incomplete and every channel string survives verbatim", () => {
  const view = egressGovernanceView({ governed: false, uncovered_channels: CHANNELS });
  assert.equal(view.status, "incomplete");
  assert.equal(view.tone, "amber");
  assert.equal(view.label, "出网治理不完整");
  assert.equal(view.channelCount, CHANNELS.length);
  assert.deepEqual(view.channels, CHANNELS);
  for (const channel of CHANNELS) {
    assert.ok(view.channels.includes(channel), `channel missing from the view: ${channel}`);
  }
});

test("the frontend derives completeness from the channel list, not from the governed flag", () => {
  const view = egressGovernanceView({ governed: true, uncovered_channels: CHANNELS });
  assert.equal(view.status, "incomplete");
  assert.notEqual(view.tone, "green");
  assert.deepEqual(view.channels, CHANNELS);
});

test("an empty channel list is complete", () => {
  const view = egressGovernanceView({ governed: true, uncovered_channels: [] });
  assert.equal(view.status, "complete");
  assert.equal(view.channelCount, 0);
  assert.deepEqual(view.channels, []);
});

test("a failed fetch is unknown, never governed", () => {
  for (const view of [egressGovernanceView(null), egressGovernanceView(undefined)]) {
    assert.equal(view.status, "unknown");
    assert.notEqual(view.status, "complete");
    assert.notEqual(view.tone, "green");
    assert.deepEqual(view.channels, []);
    assert.equal(view.channelCount, null);
  }
});

test("a payload without a channel list is unknown rather than complete", () => {
  const view = egressGovernanceView({});
  assert.equal(view.status, "unknown");
  assert.notEqual(view.status, "complete");
  assert.deepEqual(view.channels, []);
});

test("no rendered state contains an unqualified 'egress is governed' claim", () => {
  const views = [
    egressGovernanceView({ governed: false, uncovered_channels: CHANNELS }),
    egressGovernanceView({ governed: true, uncovered_channels: [] }),
    egressGovernanceView(null),
  ];
  for (const view of views) {
    const rendered = JSON.stringify(view);
    assert.equal(rendered.includes(FORBIDDEN_CLAIM), false, `forbidden claim found in ${rendered}`);
  }
});

test("the frontend source never contains the forbidden 'egress is governed' claim", () => {
  for (const { file, text } of readSourceFiles(SRC_DIR)) {
    assert.equal(text.includes(FORBIDDEN_CLAIM), false, `forbidden claim found in ${file}`);
  }
});

test("server strings are rendered as plain text, never as markup", () => {
  const source = readFileSync(join(SRC_DIR, "components", "egress-governance-notice.tsx"), "utf8");
  assert.equal(source.includes("dangerouslySetInnerHTML"), false);
});

test("the frontend does not duplicate the server's uncovered-channel list", (t) => {
  const goFile = join(REPO_ROOT, "server", "egress_governance.go");
  if (!existsSync(goFile)) {
    t.skip(`not found: ${goFile}`);
    return;
  }
  const goSource = readFileSync(goFile, "utf8");
  const block = goSource.match(/var\s+egressUncoveredChannels\s*=\s*\[\]string\{([\s\S]*?)\n\}/);
  if (!block) {
    t.skip("egressUncoveredChannels literal not found in the Go source");
    return;
  }
  const serverChannels = [...block[1].matchAll(/"([^"]+)"/g)].map((match) => match[1]);
  assert.ok(serverChannels.length > 0, "no channel strings parsed from the Go source");

  for (const { file, text } of readSourceFiles(SRC_DIR)) {
    if (file === SELF_FILE) continue;
    for (const channel of serverChannels) {
      assert.equal(text.includes(channel), false, `server channel string duplicated in ${file}`);
    }
  }
});
