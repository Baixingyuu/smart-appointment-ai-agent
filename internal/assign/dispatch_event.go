// 派单流水线的过程外显机制。
//
// 派单原本是纯函数（Pipeline.Run → Decision），不产生任何中间事件。为了让前端
// （如 Chainlit Step）能看到"服务解析 → 候选排序 → LLM 决策"的过程，这里通过
// context 注入一个 DispatchObserver：Pipeline 在关键阶段发事件，HTTP SSE 层消费。
//
// 与 agent.StreamSink 分工相同：离线评测 / 无观察者时零开销，不改变任何行为。
package assign

import (
	"context"
	"fmt"
	"strconv"
)

// DispatchEvent 派单流水线的一次中间事件。
type DispatchEvent struct {
	Stage  string `json:"stage"`  // resolve / candidates / stage2 / done
	Detail string `json:"detail"` // 人话摘要，直接可展示
	// Services 是 resolve 阶段的 top-K 服务命中（含服务名），供前端列明细。
	Services []DispatchServiceHit `json:"services,omitempty"`
	// Top 是 candidates 阶段的 top-N 候选打分明细，供前端列表格。
	Top []DispatchTopCandidate `json:"top,omitempty"`
}

// DispatchServiceHit 一条服务解析命中。
type DispatchServiceHit struct {
	ServiceID   int64   `json:"serviceId"`
	ServiceName string  `json:"serviceName"`
	Score       float64 `json:"score"`
}

// DispatchTopCandidate 一名候选的打分明细。
type DispatchTopCandidate struct {
	EmployeeID int64   `json:"employeeId"`
	Name       string  `json:"name"`
	Ownership  string  `json:"ownership"` // owner / backup / team / none
	Own        float64 `json:"own"`
	Sim        float64 `json:"sim"`
	Avail      float64 `json:"avail"`
	Senior     float64 `json:"senior"`
	Recent     float64 `json:"recent"`
	Total      float64 `json:"total"`
}

// buildServiceHits 把服务命中转成可展示的结构（补上服务名）。
func buildServiceHits(matches []ServiceMatch, dir Directory) []DispatchServiceHit {
	if len(matches) == 0 {
		return nil
	}
	out := make([]DispatchServiceHit, 0, len(matches))
	for _, m := range matches {
		name := strconv.FormatInt(m.ServiceID, 10)
		if svc, ok := dir.ServiceOf(m.ServiceID); ok && svc.Name != "" {
			name = svc.Name
		}
		out = append(out, DispatchServiceHit{ServiceID: m.ServiceID, ServiceName: name, Score: m.Score})
	}
	return out
}

// buildTopCandidates 取未被过滤候选的前 n 名（按 Total 降序），转成打分明细。
func buildTopCandidates(candidates []Candidate, n int) []DispatchTopCandidate {
	active := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.Active() {
			active = append(active, c)
		}
	}
	sortCandidatesStable(active)
	if n > 0 && len(active) > n {
		active = active[:n]
	}
	out := make([]DispatchTopCandidate, 0, len(active))
	for _, c := range active {
		out = append(out, DispatchTopCandidate{
			EmployeeID: c.EmployeeID,
			Name:       c.Name,
			Ownership:  ownershipLabel(c.OwnershipHit),
			Own:        c.OwnScore,
			Sim:        c.SimScore,
			Avail:      c.AvailScore,
			Senior:     c.SeniorityScore,
			Recent:     c.RecentScore,
			Total:      c.Total,
		})
	}
	return out
}

// DispatchObserver 派单事件观察者。
type DispatchObserver func(DispatchEvent)

type dispatchSinkKey struct{}

// WithDispatchObserver 把派单事件观察者塞进 context。
func WithDispatchObserver(ctx context.Context, fn DispatchObserver) context.Context {
	return context.WithValue(ctx, dispatchSinkKey{}, fn)
}

func dispatchObserverFrom(ctx context.Context) DispatchObserver {
	fn, _ := ctx.Value(dispatchSinkKey{}).(DispatchObserver)
	return fn
}

func (p *Pipeline) emitDispatch(ctx context.Context, e DispatchEvent) {
	if fn := dispatchObserverFrom(ctx); fn != nil {
		fn(e)
	}
}

// EmitDispatchDone 在流水线结束后发出"派单完成"事件（若观察者存在）。
//
// 由 ticket 层在拿到 Decision 后调用：pipeline.Run 有多个 return 分支，
// 与其在每个分支重复 emit，不如在唯一的收口处发一次，顺带把 assignee 名字补齐。
func EmitDispatchDone(ctx context.Context, d Decision) {
	fn := dispatchObserverFrom(ctx)
	if fn == nil {
		return
	}
	name := ""
	for _, c := range d.Candidates {
		if c.EmployeeID == d.AssigneeID {
			name = c.Name
			break
		}
	}
	fn(DispatchEvent{Stage: "done", Detail: fmt.Sprintf("派单完成：%s(%d)，走 %s", name, d.AssigneeID, d.Path)})
}
