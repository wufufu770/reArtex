// js_audit.go：npm 依赖发现（B-08A，纯逻辑、零网络）。
//
// 唯一目标：从任务工作区解析 package.json 的 dependencies + devDependencies，
// 按 workspaces 布局覆盖多包，package-lock.json 优先取精确版本。产出的依赖
// 清单只是「发现面」——供后续 OSV 批量查询卡（B-08B/C）做情报匹配；本文件
// 不做任何 OSV 请求、不写 finding、不创建任务、不扩大 scope（CVE/OSV 情报
// 永远只是线索，包安全原则）。
//
// 路径纪律（卡要求 1）：root 必须是已由既有 pathguard 解析出的任务工作区绝对
// 路径（server/workspace.go wsResolve 同源：s.m.dir / agent m.workDir 共享工
// 作目录）。本函数拒绝相对路径；并在函数内部二次收紧——任何符号链接一律不
// 跟随（文件与目录皆然），目录真实路径经 EvalSymlinks 复核必须落在 root 内，
// 软链接不能逃逸 workspace；node_modules/.git 不属工作区清单面，整枝剪除。
//
// 版本纪律（卡要求 3/4）：
//   - package-lock.json（v1 dependencies / v2+ packages 两代格式）里有该依赖
//     的精确版本 ⇒ 取锁文件版本，Locked=true（锁是「实际安装了什么」的权威
//     记录——锁文件优先）；
//   - 无锁可定版 ⇒ 原样保留 manifest 范围串，Locked=false（绝不从范围串猜测
//     或解析出「精确版本」——无法确定就如实保留不确定）；
//   - npm 别名（npm:）、git/URL、file:/link:、workspace: 协议、通配/空版本等
//     形态绝不从 spec 字符串本身强行转成精确版本；有锁则版本取自锁并如实
//     标注 Reason，无锁则 Locked=false 并标注 unsupported 原因；
//   - 非法 JSON 清单进 Skipped（带原因），绝不静默。
//
// 预算纪律（卡要求 2/5）：清单文件数、目录深度、单文件大小、单清单依赖条数
// 全部封顶；去重键=ecosystem+name+version（同名同版本跨清单只记一条）。
//
// 与 B-05 的关系：本文件只产出依赖事实（名字/版本/是否锁定），三态判定
// （affected/not_affected/maybe）属 B-08C 区间适配，绝不在这里提前下结论。
package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// 发现预算常量（确定性封顶，防无界遍历/无界解析）。
const (
	jsMaxManifestFiles = 500     // package.json 数量上限
	jsMaxDepth         = 16      // 相对 root 的目录深度上限
	jsMaxFileBytes     = 1 << 20 // 单 package.json / package-lock.json 大小上限（1 MiB）
	jsMaxDepsPerPkg    = 500     // 单清单依赖条数上限
	jsMaxSpecLen       = 128     // 保留原始 spec 串的最大长度（超出截断，防畸形膨胀）
	jsEcosystemNPM     = "npm"
)

// JSDep 是一条被发现的依赖（事实记录，不是判定结论）。
type JSDep struct {
	Ecosystem string `json:"ecosystem"`        // 恒 "npm"（本卡只发现 npm 生态）
	Name      string `json:"name"`             // 依赖名（npm alias 场景=别名键名）
	Version   string `json:"version"`          // 锁定精确版本（Locked=true）或 manifest 范围串原样（Locked=false）
	Locked    bool   `json:"locked"`           // 版本是否来自 package-lock.json
	Dev       bool   `json:"dev"`              // 来自 devDependencies
	Manifest  string `json:"manifest"`         // 相对 root 的 package.json 路径（provenance）
	Reason    string `json:"reason,omitempty"` // 空=正常；否则 unsupported/取锁原因（如实标注）
}

// JSSkip 是一条无法正常解析的记录（清单/锁文件/目录级），绝不静默吞掉。
type JSSkip struct {
	Path   string `json:"path"`   // 相对 root 的路径
	Reason string `json:"reason"` // 跳过原因（非法 JSON/过大/超限/软链接逃逸等）
}

// JSDiscovery 是一次依赖发现的完整产出。Deps/Skipped 恒非 nil（空结果=合法
// 空列表，绝不 nil——调用方按「没有 package.json」处理而非失败）。
type JSDiscovery struct {
	Deps    []JSDep  `json:"deps"`
	Skipped []JSSkip `json:"skipped"`
}

// DiscoverJSDeps 从 root（任务工作区绝对路径）递归发现 npm 依赖。零网络、
// 零写入：只读 package.json / package-lock.json。
func DiscoverJSDeps(root string) (*JSDiscovery, error) {
	// pathguard 前提：root 必须是绝对路径（生产调用方=既有工作区 pathguard
	// 解析出的任务工作目录；本函数二次收紧，绝不接受任意相对/空路径）。
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("workspace root 必须是绝对路径（由任务工作区 pathguard 解析）")
	}
	// root 本身可能是符号链接（部署形态）：以真实路径为逃逸判定基准。
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("workspace root 不可读: %w", err)
	}
	disc := &JSDiscovery{Deps: []JSDep{}, Skipped: []JSSkip{}}
	depSeen := map[string]bool{}
	manifestCount := 0
	seenDirs := map[string]bool{} // 真实路径去重——循环 workspaces/重复入队防护

	// 手工 BFS（filepath.WalkDir 不便做深度剪枝）：queue 存相对 root 的 slash 路径。
	queue := []string{"."}
	for len(queue) > 0 {
		dirRel := queue[0]
		queue = queue[1:]
		dirAbs := filepath.Join(root, filepath.FromSlash(dirRel))
		// 二次收紧：真实路径必须仍落在 root 内（软链接目录一律不跟随——
		// ReadDir 对符号链接返回 ModeSymlink，不会进子目录队列，这里是纵深）。
		dirReal, err := filepath.EvalSymlinks(dirAbs)
		if err != nil {
			disc.Skipped = append(disc.Skipped, JSSkip{Path: dirRel, Reason: "目录不可读或符号链接越出工作区"})
			continue
		}
		if !jsWithinRoot(realRoot, dirReal) {
			disc.Skipped = append(disc.Skipped, JSSkip{Path: dirRel, Reason: "符号链接越出工作区（拒绝逃逸）"})
			continue
		}
		if seenDirs[dirReal] {
			continue // 循环 workspaces/软链接往返：解析一次即可
		}
		seenDirs[dirReal] = true
		entries, err := os.ReadDir(dirAbs)
		if err != nil {
			continue // 目录读失败：无清单可发现，不伪造结果
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		depth := 0
		if dirRel != "." {
			depth = strings.Count(dirRel, "/") + 1
		}
		var subdirs []string
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				// node_modules=第三方安装域（锁文件的领地）、.git=版本控制内部
				// ——都不是工作区自有清单面，整枝剪除（不记 skip：标准排除）。
				if name == "node_modules" || name == ".git" {
					continue
				}
				if depth+1 > jsMaxDepth {
					// 深度闸：超深目录不入队；若其中恰有清单，如实记一条 skip。
					if _, err := os.Lstat(filepath.Join(dirAbs, name, "package.json")); err == nil {
						disc.Skipped = append(disc.Skipped, JSSkip{
							Path:   path.Join(dirRel, name, "package.json"),
							Reason: fmt.Sprintf("超深度上限 %d", jsMaxDepth),
						})
					}
					continue
				}
				subdirs = append(subdirs, path.Join(dirRel, name))
				continue
			}
			// 符号链接文件（含 package.json 的软链接）：一律不跟随——软链接不能
			// 逃逸 workspace（卡要求 5）。锁文件的软链接 skip 由 jsLoadLock 在
			// 实际读取点归因（这里重复记账会产生双条噪声）。
			if e.Type()&os.ModeSymlink != 0 {
				if name == "package.json" {
					disc.Skipped = append(disc.Skipped, JSSkip{
						Path:   path.Join(dirRel, name),
						Reason: "软链接不跟随（防逃逸）",
					})
				}
				continue
			}
			if name != "package.json" {
				continue
			}
			if manifestCount >= jsMaxManifestFiles {
				disc.Skipped = append(disc.Skipped, JSSkip{
					Path:   path.Join(dirRel, name),
					Reason: fmt.Sprintf("清单数量超上限 %d", jsMaxManifestFiles),
				})
				continue
			}
			manifestCount++
			jsParseManifest(root, dirRel, disc, depSeen)
		}
		queue = append(queue, subdirs...)
	}

	// 稳定输出：manifest 路径 → 名字 → 版本（确定性，测试与下游消费可复现）。
	sort.SliceStable(disc.Deps, func(i, j int) bool {
		x, y := &disc.Deps[i], &disc.Deps[j]
		if x.Manifest != y.Manifest {
			return x.Manifest < y.Manifest
		}
		if x.Name != y.Name {
			return x.Name < y.Name
		}
		return x.Version < y.Version
	})
	sort.SliceStable(disc.Skipped, func(i, j int) bool { return disc.Skipped[i].Path < disc.Skipped[j].Path })
	return disc, nil
}

// jsParseManifest 解析单个 package.json：依赖两段（deps 优先、devDep 随后）+
// workspaces 形态校验 + 邻接 package-lock.json 定版。
func jsParseManifest(root, dirRel string, disc *JSDiscovery, depSeen map[string]bool) {
	mRel := path.Join(dirRel, "package.json")
	mAbs := filepath.Join(root, filepath.FromSlash(mRel))
	skip := func(reason string) {
		disc.Skipped = append(disc.Skipped, JSSkip{Path: mRel, Reason: reason})
	}
	st, err := os.Stat(mAbs)
	if err != nil {
		skip("清单不可读")
		return
	}
	if st.Size() > jsMaxFileBytes {
		skip(fmt.Sprintf("文件过大（上限 %d 字节）", jsMaxFileBytes))
		return
	}
	raw, err := os.ReadFile(mAbs)
	if err != nil {
		skip("清单不可读")
		return
	}
	if len(raw) > jsMaxFileBytes { // 读取窗口内的竞态兜底
		skip(fmt.Sprintf("文件过大（上限 %d 字节）", jsMaxFileBytes))
		return
	}
	var mf struct {
		Name            string            `json:"name"`
		Workspaces      json.RawMessage   `json:"workspaces"`
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(raw, &mf); err != nil {
		skip("非法 JSON")
		return
	}

	// workspaces 形态校验（数组形 / 对象形）：声明的模式只允许落在工作区内
	// （../ 与绝对路径一律拒绝）。成员目录是 root 内真实目录，主 BFS 自然
	// 覆盖——这里显式校验声明形态，越界模式如实记录（软链接成员同样不会
	// 被跟随，与逃逸纪律一致）。
	for _, pat := range jsWorkspacePatterns(mf.Workspaces) {
		if pat == "" {
			continue
		}
		cleanPat := filepath.ToSlash(filepath.Clean(pat))
		if filepath.IsAbs(cleanPat) || cleanPat == ".." || strings.HasPrefix(cleanPat, "../") {
			skip(fmt.Sprintf("workspace 模式越出工作区，拒绝：%s", pat))
		}
	}

	lock := jsLoadLock(root, dirRel, disc)

	budget := jsMaxDepsPerPkg
	addSection := func(specs map[string]string, dev bool) {
		names := make([]string, 0, len(specs))
		for n := range specs {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, name := range names {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if budget <= 0 {
				skip(fmt.Sprintf("依赖条数超上限 %d", jsMaxDepsPerPkg))
				return
			}
			budget--
			spec := strings.TrimSpace(specs[name])
			ver, locked, reason := jsResolveSpec(name, spec, dirRel, lock)
			dep := JSDep{
				Ecosystem: jsEcosystemNPM,
				Name:      name,
				Version:   ver,
				Locked:    locked,
				Dev:       dev,
				Manifest:  mRel,
				Reason:    reason,
			}
			// 去重键=ecosystem+name+version（卡要求 5）：同名同版本跨清单只记
			// 一条（首见为准；BFS 顺序+段序固定=确定性）。
			key := dep.Ecosystem + "|" + dep.Name + "|" + dep.Version
			if !depSeen[key] {
				depSeen[key] = true
				disc.Deps = append(disc.Deps, dep)
			}
		}
	}
	addSection(mf.Dependencies, false)
	addSection(mf.DevDependencies, true)
}

// jsWorkspacePatterns 归一 workspaces 字段（数组形 ["packages/*"] / 对象形
// {"packages":[...]}）；畸形形态返回 nil 并由调用方 skip 归因之外直接忽略
// （workspaces 缺失=普通单包，不是错误）。
func jsWorkspacePatterns(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr
	}
	var obj struct {
		Packages []string `json:"packages"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return obj.Packages
	}
	return nil
}

// jsLockFile 是 package-lock.json 的两代格式（v1 dependencies / v2+ packages）。
type jsLockFile struct {
	LockfileVersion int `json:"lockfileVersion"`
	Packages        map[string]struct {
		Version string `json:"version"`
	} `json:"packages"`
	Dependencies map[string]struct {
		Version string `json:"version"`
	} `json:"dependencies"`
}

// jsLoadLock 读取 dirRel 邻接的 package-lock.json（不存在=nil）。锁文件的软
// 链接一律不跟随（防逃逸）——walk 循环的符号链接闸管不到这条独立读取路径，
// 这里用 Lstat 自查；非法 JSON/过大如实记 skip——绝不把坏锁静默当「无锁」。
func jsLoadLock(root, dirRel string, disc *JSDiscovery) *jsLockFile {
	lRel := path.Join(dirRel, "package-lock.json")
	lAbs := filepath.Join(root, filepath.FromSlash(lRel))
	st, err := os.Lstat(lAbs)
	if err != nil {
		return nil // 无锁文件：合法常态（Locked=false 路径）
	}
	if st.Mode()&os.ModeSymlink != 0 {
		disc.Skipped = append(disc.Skipped, JSSkip{Path: lRel, Reason: "软链接不跟随（防逃逸）"})
		return nil
	}
	if st.Size() > jsMaxFileBytes {
		disc.Skipped = append(disc.Skipped, JSSkip{Path: lRel, Reason: fmt.Sprintf("锁文件过大（上限 %d 字节）", jsMaxFileBytes)})
		return nil
	}
	raw, err := os.ReadFile(lAbs)
	if err != nil {
		disc.Skipped = append(disc.Skipped, JSSkip{Path: lRel, Reason: "锁文件不可读"})
		return nil
	}
	if len(raw) > jsMaxFileBytes {
		disc.Skipped = append(disc.Skipped, JSSkip{Path: lRel, Reason: fmt.Sprintf("锁文件过大（上限 %d 字节）", jsMaxFileBytes)})
		return nil
	}
	var l jsLockFile
	if err := json.Unmarshal(raw, &l); err != nil {
		disc.Skipped = append(disc.Skipped, JSSkip{Path: lRel, Reason: "锁文件非法 JSON"})
		return nil
	}
	if l.Packages == nil && l.Dependencies == nil {
		return nil // 空锁：无条目可定版
	}
	return &l
}

// jsLockVersion 查 (dirRel, name) 的锁定精确版本：v2+ packages 按目录定位键
// （"node_modules/<name>" / "<dir>/node_modules/<name>"，scoped 名天然兼容），
// v1 旧格式仅根级 dependencies。版本必须过精确 semver 形态闸才可信——锁里
// 出现畸形版本串绝不原样采信。
func jsLockVersion(l *jsLockFile, dirRel, name string) string {
	if l == nil || name == "" {
		return ""
	}
	if l.Packages != nil {
		key := "node_modules/" + name
		if dirRel != "." {
			key = dirRel + "/node_modules/" + name
		}
		if e, ok := l.Packages[key]; ok && jsExactSemver(e.Version) {
			return e.Version
		}
	}
	if dirRel == "." && l.Dependencies != nil {
		if e, ok := l.Dependencies[name]; ok && jsExactSemver(e.Version) {
			return e.Version
		}
	}
	return ""
}

// jsResolveSpec 按「锁文件优先」裁决一条依赖的版本三元组：
//   - 锁里有精确版本 ⇒ (lockVer, true, reason)；spec 形态本身不受支持时如实
//     标注「版本取自锁文件」（锁是实际安装记录——不是从 spec 猜出来的）；
//   - 无锁 ⇒ 正常范围串原样保留 (spec, false, "")；unsupported 形态绝不强转，
//     标注 unsupported 原因（skip 精确判定的依据，B-08C 将按 Locked=false 保
//     守处理）。
func jsResolveSpec(name, spec, dirRel string, lock *jsLockFile) (string, bool, string) {
	unsup := jsUnsupportedReason(spec)
	if v := jsLockVersion(lock, dirRel, name); v != "" {
		if unsup != "" {
			return v, true, fmt.Sprintf("spec 为%s形态，版本取自锁文件（不从 spec 强转）", unsup)
		}
		return v, true, ""
	}
	if unsup != "" {
		return jsTruncateSpec(spec), false, fmt.Sprintf("unsupported：%s；无锁可定版，保留原 spec", unsup)
	}
	return jsTruncateSpec(spec), false, ""
}

// jsUnsupportedReason 归类不可从 spec 字符串定版的形态（卡要求 4）。
func jsUnsupportedReason(spec string) string {
	switch {
	case spec == "" || spec == "*":
		return "通配/空版本"
	case strings.HasPrefix(spec, "npm:"):
		return "npm 别名"
	case strings.HasPrefix(spec, "git+") || strings.HasPrefix(spec, "git@") ||
		strings.HasPrefix(spec, "github:") || strings.Contains(spec, "://"):
		return "git/URL 版本"
	case strings.HasPrefix(spec, "file:") || strings.HasPrefix(spec, "link:"):
		return "file/link 本地路径"
	case strings.HasPrefix(spec, "workspace:"):
		return "workspace 协议"
	default:
		return ""
	}
}

// jsExactSemver 判定精确版本形态（MAJOR.MINOR.PATCH[-pre][+build]）——范围串
// （^ ~ >= * x-range 等）一律不匹配。
var jsExactSemverRe = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.\-]+)?(\+[0-9A-Za-z.\-]+)?$`)

func jsExactSemver(s string) bool {
	return jsExactSemverRe.MatchString(s)
}

// jsTruncateSpec 截断畸形超长 spec（防清单里的膨胀串进下游），截断处如实留 "…"。
func jsTruncateSpec(spec string) string {
	if len(spec) <= jsMaxSpecLen {
		return spec
	}
	return spec[:jsMaxSpecLen] + "…"
}

// jsWithinRoot 判定 real 是否落在 root 内（真实路径 containment，与
// server/workspace.go wsResolve 同语义：相等或带分隔符前缀）。
func jsWithinRoot(root, real string) bool {
	if real == root {
		return true
	}
	return strings.HasPrefix(real, root+string(os.PathSeparator))
}
