package agent

import "github.com/Autumn-27/artex/internal/scopepolicy"

// ScopePolicy 是 scope 策略常量的【薄别名】，不是第二份定义。
//
// 唯一真相源在 internal/scopepolicy：扩权上限与「scope 内」判定读的是同一个值。
// 这里只为了让 agent 包内能用 agent.ScopePolicy 这个名字；值本身不在这里定义。
var ScopePolicy = scopepolicy.Policy
