package traffic

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// A-01：平台通道白名单——「未归属」出网的唯一例外面。
//
// 背景与取舍（与 egress.go 的 egressDecide 注释互为表里）：未携带任务身份的
// 出网请求原先按「放行但标记」处理，代价是 agent 可以靠「不带身份」绕过任务
// 范围校验。A-01 把默认处置翻成拒绝；但平台自身确有少量必须无任务出网的通道
// （如资产补全探测、浏览器 MCP 的出站回调）。这些通道必须**显式登记**才能
// 继续，未登记的一律拒绝。
//
// 白名单纪律（与 A-06 的降级开关同一原则）：
//   - 只能由 operator 在**启动期**配置（环境变量 ARTEX_PLATFORM_CHANNELS 指向
//     JSON 文件），不进 settings 热改面——绕过范围校验的口子不能靠点鼠标开；
//   - 文件不存在 = 空白名单 = 全部拒绝（不回落放行）；
//   - 解析失败 = 按空白名单处理并打错误日志（fail-closed 方向：坏配置只会
//     造成更多拒绝，不会造成更多放行）。

// PlatformChannel 是一条登记在册的无任务平台通道。
type PlatformChannel struct {
	// Name 是通道名（如 "enrich"、"browser-mcp"）。调用方经内部头声明自己的
	// 通道身份；名字必须与本表的某条目一致才有资格走白名单。
	Name string `json:"name"`
	// Hosts 是允许的目的地主机表：精确主机名（"api.example.com"）或后缀
	// （".example.com"，含自身与任意子域）。空表 = 该通道登记了也不放行任何
	// 目的地（显式写出来比「忘填=全放」安全）。
	Hosts []string `json:"hosts"`
}

// platformChannelHeader 是平台侧 HTTP 客户端声明自己通道身份的头。
//
// 为什么用头而不是环境变量：agent 侧的环境变量会被 Bash 子进程读到、也可以被
// 改写；这个头由平台自己的客户端代码在发请求时注入，不经 agent 之手。
//
// 【如实盲区】这个头**不是**防伪边界：agent 的 Bash 子进程与代理之间是明文
// HTTP 代理协议，子进程理论上可以自己拼一条带 X-Artex-Channel 的请求发给代理
// （curl -H）。真正的防线是**主机表**——冒用通道名只能到达该通道登记的主机，
// 攻击面被钉死在 operator 显式圈定的范围里。通道身份头只是「声明」，主机匹配
// 才是「裁决」。
const platformChannelHeader = "X-Artex-Channel"

// platformChannelDecidedHeader 是闸门→录制管线的内部传递头：egressGate 判定某请求
// 落在平台通道白名单内后，把通道名写到【同一个请求对象】上；Requestheaders 摘下
// 并据此把 exchange 记成 decision=channel（白名单放行的留痕）。它随 stripEgressIdentity
// 一并摘除，绝不转发给目标、也绝不接受伪造——agent 伪造它拿不到任何放行（放行
// 判定在闸门、只认白名单），至多影响已放行流量的标签。
const platformChannelDecidedHeader = "X-Artex-Channel-Decided"

// EnvPlatformChannels 是白名单文件路径的环境变量名。
const EnvPlatformChannels = "ARTEX_PLATFORM_CHANNELS"

// platformChannelSet 是进程内只读的白名单视图（启动期装配后不再变）。
type platformChannelSet struct {
	byName map[string]PlatformChannel
}

// emptyPlatformChannels 是空白名单：所有未归属请求都被拒。
func emptyPlatformChannels() *platformChannelSet {
	return &platformChannelSet{byName: map[string]PlatformChannel{}}
}

// Count 返回在册通道数（启动日志用）。
func (s *platformChannelSet) Count() int { return len(s.byName) }

// parsePlatformChannels 解析白名单 JSON。格式：
//
//	[{"name":"enrich","hosts":[".assets.example.com"]}]
//
// 重名条目以后者为准（启动期配置错误由 operator 看 JSON 自己负责，
// 不静默合并）。名字与主机都做归一化（小写、去空白）。
func parsePlatformChannels(raw []byte) (*platformChannelSet, error) {
	var chans []PlatformChannel
	if err := json.Unmarshal(raw, &chans); err != nil {
		return nil, fmt.Errorf("平台通道白名单解析失败: %w", err)
	}
	set := &platformChannelSet{byName: map[string]PlatformChannel{}}
	for _, ch := range chans {
		ch.Name = strings.ToLower(strings.TrimSpace(ch.Name))
		if ch.Name == "" {
			return nil, fmt.Errorf("平台通道白名单里有 name 为空的条目")
		}
		hosts := make([]string, 0, len(ch.Hosts))
		for _, h := range ch.Hosts {
			h = strings.ToLower(strings.TrimSpace(h))
			if h != "" {
				hosts = append(hosts, h)
			}
		}
		ch.Hosts = hosts
		set.byName[ch.Name] = ch
	}
	return set, nil
}

// LoadPlatformChannels 从文件加载白名单。文件不存在 = 空白名单（全部拒绝，
// 不回落放行）；解析失败也按空白名单处理，但把错误交还调用方打日志。
func LoadPlatformChannels(path string) (*platformChannelSet, error) {
	if strings.TrimSpace(path) == "" {
		return emptyPlatformChannels(), nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return emptyPlatformChannels(), nil
		}
		return emptyPlatformChannels(), fmt.Errorf("平台通道白名单文件不可读: %w", err)
	}
	set, err := parsePlatformChannels(raw)
	if err != nil {
		// 解析失败也按空白名单处理：坏配置只会造成更多拒绝，不会更多放行。
		return emptyPlatformChannels(), err
	}
	return set, nil
}

// platformChannelAllowed 判定一次未归属请求是否落在登记通道内。
//
// 两道条件缺一不可：调用方声明了在册通道名 **且** 目的地主机命中该通道的
// 主机表。返回命中的通道供审计留痕（放行的每一条都要能回答「走的哪条通道、
// 谁批准的」——名字写进判定理由）。
func (t *Traffic) platformChannelAllowed(req *http.Request) (PlatformChannel, bool) {
	set := t.platformChannels.Load()
	if set == nil || req == nil {
		return PlatformChannel{}, false
	}
	name := strings.ToLower(strings.TrimSpace(req.Header.Get(platformChannelHeader)))
	if name == "" {
		return PlatformChannel{}, false
	}
	ch, ok := set.byName[name]
	if !ok {
		return PlatformChannel{}, false
	}
	host := egressDestinationHost(req)
	if host == "" {
		return PlatformChannel{}, false
	}
	for _, h := range ch.Hosts {
		if matchPlatformHost(host, h) {
			return ch, true
		}
	}
	return PlatformChannel{}, false
}

// matchPlatformHost 报告 host 是否命中白名单条目：".example.com" 形式的后缀
// 条目命中自身与任意子域；其余按精确比对。
func matchPlatformHost(host, entry string) bool {
	if host == entry {
		return true
	}
	if strings.HasPrefix(entry, ".") {
		return strings.HasSuffix(host, entry) || host == strings.TrimPrefix(entry, ".")
	}
	return false
}

// SetPlatformChannels 注入启动期白名单（进程生命周期内只调一次；重复注入
// 以最后一次为准，仅供测试复位）。
func (t *Traffic) SetPlatformChannels(set *platformChannelSet) {
	if set == nil {
		set = emptyPlatformChannels()
	}
	t.platformChannels.Store(set)
}
