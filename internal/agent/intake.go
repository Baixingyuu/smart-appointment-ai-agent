// 建单交互的"进展驱动追问"编排。
//
// 三条支柱在这里合流：阻塞槽位（ticket 包）+ 可追溯判定（traceability.go）+
// 进展终止 / 安全阀 / 主动建单出口（本文件）。核心决策 decideIntake 是纯函数，
// 不碰模型也不碰存储，便于把"问到没进展就建单"这套规则单测打透。
//
// 跨轮进度放在 Agent 内存 map（按会话）：与一期"仅内存持久化"的现状一致，
// 不新增 Store 接口；建单成功即清除，避免把上一件事的追问算到下一件事头上。
package agent

import (
	"context"
	"strings"

	"github.com/mac/helpdesk-agent/internal/domain"
	"github.com/mac/helpdesk-agent/internal/ticket"
	"github.com/mac/helpdesk-agent/internal/tooling"
)

// DefaultIntakeMaxAskRounds 是单会话追问回合的安全阀上限。
//
// 正常路径靠"字段进展"终止（某槽位问过一次、用户补了仍空 → 不再追问），
// 摸不到这个上限。它只在"判空逻辑出 bug 导致一直追问"时兜底（TICKET_INTAKE §2.3）。
const DefaultIntakeMaxAskRounds = 10

// MissingInfo 里由建单交互追加的固定标记。
const (
	missingFidelityUnverified = "fidelity_unverified" // 描述里有追溯不到的声明（只标注不拦单）
	missingUserForcedCommit   = "user_forced_commit"  // 用户主动要求直接建单
	missingSafetyValve        = "safety_valve_reached"
)

// slotMissingTag 把一个放弃追问的阻塞槽位记成一条 MissingInfo。
func slotMissingTag(slot string) string { return "missing_slot:" + slot }

// intakeAction 一次追问决策的动作。
type intakeAction int

const (
	intakeCreate intakeAction = iota // 建单（信息齐、或放弃追问、或安全阀）
	intakeAsk                        // 先向用户追问还缺的阻塞槽位
)

// intakeDecision 是 decideIntake 的纯输出。
type intakeDecision struct {
	Action       intakeAction
	AskSlots     []string // 本轮一次性列全、向用户追问的"新空"槽位
	AbandonSlots []string // 已问过仍空 → 放弃追问、记 MissingInfo 的槽位
	SafetyValve  bool
	// Progress 是推进后的跨轮状态，调用方写回会话。
	Progress domain.IntakeProgress
}

// decideIntake 是进展驱动追问的纯决策（无副作用、无 I/O）。
//
// emptySlots：本轮仍空的阻塞槽位（已由 checker 认定"没有可追溯证据"）。
// progress：该会话此前的追问进度。maxRounds：安全阀。
//
// 规则（TICKET_INTAKE §2）：
//   - 没有空槽 → 建单。
//   - 触顶安全阀 → 无条件建单，所有空槽都算放弃。
//   - 存在"从没问过"的空槽 → 追问这些（一次列全），并把它们记为已问、轮次 +1。
//   - 空槽全都问过了 → 放弃追问、建单，把它们记进 MissingInfo。
func decideIntake(emptySlots []string, progress domain.IntakeProgress, maxRounds int) intakeDecision {
	empty := dedupeKeepOrder(emptySlots)
	if len(empty) == 0 {
		return intakeDecision{Action: intakeCreate, Progress: progress}
	}
	if maxRounds > 0 && progress.AskRounds >= maxRounds {
		return intakeDecision{Action: intakeCreate, AbandonSlots: empty, SafetyValve: true, Progress: progress}
	}
	var fresh []string
	for _, slot := range empty {
		if !progress.Asked(slot) {
			fresh = append(fresh, slot)
		}
	}
	if len(fresh) > 0 {
		next := progress
		for _, slot := range fresh {
			next = next.MarkAsked(slot)
		}
		next.AskRounds++
		return intakeDecision{Action: intakeAsk, AskSlots: fresh, Progress: next}
	}
	return intakeDecision{Action: intakeCreate, AbandonSlots: empty, Progress: progress}
}

// intakeGate 一次建单前置检查的结果，交给 createInterrupt / 工具循环消费。
type intakeGate struct {
	ask           bool     // true：本轮先追问，不建草案
	askSlots      []string // 追问话术里列出的缺失槽位
	notes         []string // 追加进 MissingInfo 的标注
	untracedDescs []string // 追溯不到的描述声明，展示给用户核对
	progress      domain.IntakeProgress
}

// evaluateIntake 对一次 ticket_create_confirm 调用做可追溯性与追问判定。
//
// 仅在注入了 checker 时被调用（默认关闭 → 既有链路零改动）。
// 关键约束：只有当模型确实给了 extractedSlots 证据时才"追问拦单"——
// 若模型压根没用槽位协议（如仅凭标题建单），退回既有"信息不全不阻塞建单"语义，
// 只追加忠实度标注，绝不因为没走新协议就改变旧行为。
func (a *Agent) evaluateIntake(ctx context.Context, conversationID int64, args map[string]any) intakeGate {
	corpus := a.customerCorpus(conversationID)
	progress := a.intakeState(conversationID)
	gate := intakeGate{progress: progress}

	category := domain.Category(tooling.StringArg(args, "category"))
	if !category.Valid() {
		category = domain.CategoryIncident
	}
	required := ticket.BlockingSlots(category)
	extracted := ticket.ParseExtractedSlots(extractSlotsArg(args))

	// 1) 阻塞槽位证据可追溯性 → filled 集合。门控（追问 vs 建单）只在
	//    intakeGating 开启、且模型确实走了槽位协议时才生效。
	empty := required
	if a.intakeGating && len(extracted) > 0 && len(required) > 0 {
		claims := make([]Claim, 0, len(extracted))
		for _, slot := range extracted {
			if slot.Quote == "" {
				continue
			}
			claims = append(claims, Claim{ID: slot.Name, Text: slot.Quote})
		}
		filled := map[string]bool{}
		if len(claims) > 0 {
			for _, verdict := range a.checkClaims(ctx, claims, corpus) {
				if verdict.Verdict != VerdictUnverified {
					filled[verdict.ID] = true
				}
			}
		}
		empty = ticket.MissingBlockingSlots(required, filled)

		// 2) 只有走了槽位协议才做追问门。
		decision := decideIntake(empty, progress, a.intakeMaxRounds)
		gate.progress = decision.Progress
		if decision.Action == intakeAsk {
			gate.ask = true
			gate.askSlots = decision.AskSlots
			a.storeIntakeState(conversationID, decision.Progress)
			return gate
		}
		for _, slot := range decision.AbandonSlots {
			gate.notes = append(gate.notes, slotMissingTag(slot))
		}
		if decision.SafetyValve {
			gate.notes = append(gate.notes, missingSafetyValve)
		}
	}

	// 3) 描述忠实度：只标注不拦单。
	if desc := strings.TrimSpace(tooling.StringArg(args, "description")); desc != "" && a.traceability != nil {
		claims := splitClaims(desc)
		if len(claims) > 0 {
			for _, verdict := range a.checkClaims(ctx, claims, corpus) {
				if verdict.Verdict == VerdictUnverified {
					if text := claimText(claims, verdict.ID); text != "" {
						gate.untracedDescs = append(gate.untracedDescs, text)
					}
				}
			}
			if len(gate.untracedDescs) > 0 {
				gate.notes = append(gate.notes, missingFidelityUnverified)
			}
		}
	}
	return gate
}

// checkClaims 调判定器，吞掉实现层错误并退回"全部 unverified"。
//
// 判定失败不该让整条建单失败——最坏是"多标注几条待核对"，而不是丢用户一个诉求。
func (a *Agent) checkClaims(ctx context.Context, claims []Claim, corpus []string) []ClaimVerdict {
	if a.traceability == nil {
		verdicts := make([]ClaimVerdict, len(claims))
		for i, c := range claims {
			verdicts[i] = ClaimVerdict{ID: c.ID, Verdict: VerdictUnverified, Reason: "未配置判定器"}
		}
		return verdicts
	}
	verdicts, err := a.traceability.CheckAll(ctx, claims, corpus)
	if err != nil {
		fallback := make([]ClaimVerdict, len(claims))
		for i, c := range claims {
			fallback[i] = ClaimVerdict{ID: c.ID, Verdict: VerdictUnverified, Reason: "判定器出错：" + err.Error()}
		}
		return fallback
	}
	return verdicts
}

// clarifyObservation 把"要追问的缺失槽位"写回给模型，指示它当轮只问不建。
func clarifyObservation(slots []string) string {
	labels := make([]string, 0, len(slots))
	for _, slot := range slots {
		labels = append(labels, ticket.SlotLabel(slot))
	}
	return "登记工单还缺以下关键信息，请本轮直接向用户一次性列出追问，不要再调用建单工具：（" +
		strings.Join(labels, "；") + "）。用户补充后你再据其原话填写 extractedSlots 的 quote。"
}

// splitClaims 把描述拆成原子声明，用于逐条可追溯性判定。
//
// 一期只做中文/英文分隔符的粗切；够把"我重启过网关。已联系财务确认政策"这种
// 多事实句子切开，逐条核对。切不开的句子按整段判，宁可少切不可错切。
func splitClaims(description string) []Claim {
	raw := strings.FieldsFunc(description, func(r rune) bool {
		switch r {
		case '，', '。', '；', '！', '？', '、', '\n', '\r', ',', ';', '!', '?':
			return true
		}
		return false
	})
	claims := make([]Claim, 0, len(raw))
	for _, piece := range raw {
		text := strings.TrimSpace(piece)
		if len([]rune(text)) < 2 {
			continue
		}
		claims = append(claims, Claim{ID: "claim-" + itoa(len(claims)), Text: text})
	}
	return claims
}

func claimText(claims []Claim, id string) string {
	for _, c := range claims {
		if c.ID == id {
			return c.Text
		}
	}
	return ""
}

func extractSlotsArg(args map[string]any) []any {
	raw, ok := args["extractedSlots"].([]any)
	if !ok {
		return nil
	}
	return raw
}

func dedupeKeepOrder(items []string) []string {
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		if seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

// itoa 小整数转字符串，避免为几个 ID 引入 strconv。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// customerCorpus 取本会话内用户说过的话，作为可追溯性判定的语料。
//
// 只取 customer：AI 自己的话不能反过来"证明"自己说过的事实（否则会自证编造）。
func (a *Agent) customerCorpus(conversationID int64) []string {
	if a.store == nil || conversationID <= 0 {
		return nil
	}
	messages := a.store.MessagesByConversation(conversationID)
	corpus := make([]string, 0, len(messages))
	for _, msg := range messages {
		if msg.Sender != domain.SenderCustomer {
			continue
		}
		if text := strings.TrimSpace(msg.Content); text != "" {
			corpus = append(corpus, text)
		}
	}
	return corpus
}

// intakeState 读取某会话的追问进度（无记录即零值）。
func (a *Agent) intakeState(conversationID int64) domain.IntakeProgress {
	a.intakeMu.Lock()
	defer a.intakeMu.Unlock()
	return a.intakeStates[conversationID]
}

// storeIntakeState 写回某会话的追问进度。
func (a *Agent) storeIntakeState(conversationID int64, progress domain.IntakeProgress) {
	a.intakeMu.Lock()
	defer a.intakeMu.Unlock()
	if a.intakeStates == nil {
		a.intakeStates = make(map[int64]domain.IntakeProgress)
	}
	a.intakeStates[conversationID] = progress
}

// clearIntakeState 在建单成功后清除该会话的追问进度，
// 使同一会话的下一个诉求从"还没追问过"重新开始。
func (a *Agent) clearIntakeState(conversationID int64) {
	a.intakeMu.Lock()
	defer a.intakeMu.Unlock()
	delete(a.intakeStates, conversationID)
}
