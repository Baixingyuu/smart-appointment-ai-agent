// ScriptedChooser 是 Stage 2 的确定性假实现，用于评测与单测。
//
// 与现有评测框架的 scripted / live 双模约定一致（见 HANDOVER.md §10）：
// make check 走这条路径，无需网络与 API Key；live 评测才换成 OpenAIChooser。
package assign

import (
	"context"
	"fmt"
	"sync"

	"github.com/mac/helpdesk-agent/internal/llm"
)

// ScriptedChoice 一条 fixture：按 ticketID 命中的返回内容。
//
// 用值类型而不是函数：让 fixture 能直接由 JSON 数据集载入，
// 评测集与代码解耦，改数据集不用改代码。
type ScriptedChoice struct {
	AssigneeID int64
	Rationale  string
	Confidence float64
	Err        error
}

// ScriptedChooser 按 ticket.ID 查表返回固定决策。
//
// 未命中时的行为由 Default 决定（例如默认全部 ESCALATE_HUMAN），
// 而不是静默返回 0 —— 0 会让 pipeline 把"缺 fixture"误报成"模型幻觉"。
// 命中顺序无副作用：所有并发调用读同一份只读表，用 RWMutex 保护记录调用序列。
type ScriptedChooser struct {
	mu       sync.Mutex
	ByTicket map[int64]ScriptedChoice
	Default  *ScriptedChoice
	// calls 记录每次 Choose 的入参摘要，供断言"Stage 2 被调用了几次 / 送进去的候选集对不对"。
	calls []Stage2Request
}

// NewScriptedChooser 构造。byTicket 允许为 nil（当作纯默认桩）。
func NewScriptedChooser(byTicket map[int64]ScriptedChoice) *ScriptedChooser {
	return &ScriptedChooser{ByTicket: byTicket}
}

// Choose 返回预置决策，并把 req 追加进内部记录。
func (s *ScriptedChooser) Choose(_ context.Context, req Stage2Request) (LLMChoice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req)

	choice, ok := s.ByTicket[req.Ticket.ID]
	if !ok {
		if s.Default == nil {
			return LLMChoice{}, fmt.Errorf("ScriptedChooser: 未预置 ticketID=%d 且无 Default", req.Ticket.ID)
		}
		choice = *s.Default
	}
	if choice.Err != nil {
		return LLMChoice{}, choice.Err
	}
	return LLMChoice{
		AssigneeID: choice.AssigneeID,
		Rationale:  choice.Rationale,
		Confidence: choice.Confidence,
		Usage:      llm.Usage{PromptTokens: 100, CompletionTokens: 40}, // 固定用量便于成本轴断言
	}, nil
}

// Calls 返回迄今记录的入参快照（拷贝），供测试断言。
func (s *ScriptedChooser) Calls() []Stage2Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Stage2Request(nil), s.calls...)
}

// CallCount 迄今被调用的次数。
func (s *ScriptedChooser) CallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// Reset 清空调用记录，不影响 fixture。用于在同一 ScriptedChooser 上跑多轮断言。
func (s *ScriptedChooser) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = nil
}

var _ LLMChooser = (*ScriptedChooser)(nil)
