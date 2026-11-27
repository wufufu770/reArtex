package agent

import "github.com/Autumn-27/artex/db"

// GateTaskCompletion 在收尾时决定终态。
// 没有未达成目标时，调用方想记的状态原样返回。
// 还有未达成目标、调用方却想记 done：不许记 done，改记 timeout。
// 想记的本来就是 timeout，或是 failed / paused 等其它状态：不改写。
// 不读库、不调模型。未达成清单由调用方先查好。
func GateTaskCompletion(unmet []db.UnmetGoal, wantTerminal string) (string, []db.UnmetGoal) {
	if len(unmet) == 0 {
		return wantTerminal, nil
	}
	if wantTerminal == "done" {
		return "timeout", unmet
	}
	return wantTerminal, unmet
}
