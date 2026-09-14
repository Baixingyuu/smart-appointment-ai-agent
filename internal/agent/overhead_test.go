package agent

import (
	"encoding/json"
	"testing"

	"github.com/mac/agentdesk/internal/llm"
)

// TestFixedPromptOverheadBudget 把固定 prompt 开销锁进测试。
//
// 固定开销（系统提示词 + 工具 schema）是每一轮都要付的成本，
// 也是真实模型实测里单轮约 1,400 token 的主要来源。
// 用测试锁住它，避免后续新增工具或修改提示词时悄悄推高成本。
func TestFixedPromptOverheadBudget(t *testing.T) {
	h := newHarness(t, []*llm.Response{finalReply("好")})
	h.runTurn(t, 1, "测试")

	req := h.model.lastRequest()
	systemTokens := estimateTokens(req.System)

	var schemaTokens int
	for _, schema := range req.Tools {
		params, err := json.Marshal(schema.Parameters)
		if err != nil {
			t.Fatalf("序列化工具 schema 失败: %v", err)
		}
		schemaTokens += estimateTokens(string(params))
	}
	total := systemTokens + schemaTokens

	t.Logf("系统提示词 ≈%d token，工具 schema ≈%d token（%d 个工具），固定开销 ≈%d token",
		systemTokens, schemaTokens, len(req.Tools), total)

	// 上限取当前实测值留少量余量；超出说明有新增开销需要评估。
	const budget = 900
	if total > budget {
		t.Errorf("固定开销 %d token 超出预算 %d —— 新增工具或提示词时需评估成本影响",
			total, budget)
	}
}

// TestToolCountIsMinimal 锁住工具数量。
//
// 工具数量直接决定 schema 开销。新增工具前应先确认它无法被现有工具覆盖——
// 实测 ticket_create_draft 与 ticket_create_confirm 有 4 个完全相同的参数，
// 属于设计冗余，已移除。
func TestToolCountIsMinimal(t *testing.T) {
	h := newHarness(t, []*llm.Response{finalReply("好")})
	h.runTurn(t, 1, "测试")

	const maxTools = 3
	got := len(h.model.lastRequest().Tools)
	if got > maxTools {
		t.Errorf("工具数 %d 超出上限 %d，请先评估是否与现有工具重叠", got, maxTools)
	}
	t.Logf("当前工具数 %d", got)
}

// estimateTokens 粗略估算 token 数。
//
// 中文约 1.5 字符/token、英文约 4 字符/token，混合文本取 2 字符/token
// 是足够接近的近似。这里只需相对准确的量级，用于成本回归而非精确计费。
func estimateTokens(text string) int {
	return len([]rune(text)) / 2
}
