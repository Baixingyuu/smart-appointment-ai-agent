// Stage 1.5：判弱。
//
// 判弱必须是离散原因，不是连续置信度：见 docs/DISPATCH_PIPELINE.md §1.5。
// 三类原因对应三种不同的修复方向：
//   - low_margin      → 排序缺证据，需要 LLM 或人补一条
//   - no_service_match → 抽取段没解出来，需要改进 resolve 或让 LLM 直接抽取
//   - empty_candidates → 负载模型或员工配置有问题，Stage 2 也救不了
package assign

// judgeWeakness 在过滤 + 打分完成后判定 Stage 1 是否可信。
//
// 判据顺序（短路）：
//  1. 过滤后无候选人 → EmptyCandidates（Stage 3 阻塞）
//  2. 服务解析最高分未达 ServiceMatchFloor → NoServiceMatch（Stage 2）
//  3. top1 与 top2 总分差未达 MarginThreshold → LowMargin（Stage 2）
//  4. 否则 None
//
// 只有 1 名 active 候选人时不做 margin 判定：没有次位可比，margin 视为 ∞。
// 单人团队是常见场景，此时"唯一人选是不是他"这件事 LLM 也无从判断，
// 直派比升级到 Stage 2 更合理。
func (p *Pipeline) judgeWeakness(candidates []Candidate, matches []ServiceMatch) WeaknessReason {
	// 复用 topCandidates 的确定性排序，取按总分降序的前两名。
	// 手写"遍历时记 top1/top2"是常见 bug 源（把"第一个 active"当成"最好的"），
	// 借已排序的辅助函数一次锁定语义。
	top := topCandidates(candidates, 2)
	if len(top) == 0 {
		return WeaknessEmptyCandidates
	}

	// 服务解析：matches 空或最高分未达下限 → 归属段没给出可靠答案。
	// 注意：matches 为空但 top1 已因 ownership 命中，仍视为 WeaknessNoServiceMatch。
	// 这不是自相矛盾 —— 没有服务字典时 s_own 一定为 0，
	// 若 s_own 非 0 又 matches 为空，说明配置错误，判弱是保护。
	if len(matches) == 0 || matches[0].Score < p.config.ServiceMatchFloor {
		return WeaknessNoServiceMatch
	}

	if len(top) >= 2 {
		margin := top[0].Total - top[1].Total
		if margin < p.config.MarginThreshold {
			return WeaknessLowMargin
		}
	}
	return WeaknessNone
}

// String 便于日志与评测输出。
func (w WeaknessReason) String() string {
	if w == WeaknessNone {
		return "none"
	}
	return string(w)
}

// IsStage2Eligible 判定该 weakness 是否允许进入 Stage 2。
// EmptyCandidates 不算：Stage 2 也没候选人可选。
func (w WeaknessReason) IsStage2Eligible() bool {
	return w == WeaknessLowMargin || w == WeaknessNoServiceMatch
}
