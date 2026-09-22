package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// InterruptStatus 确认中断的状态。
type InterruptStatus string

const (
	InterruptPending   InterruptStatus = "pending"   // 等待用户确认
	InterruptResolved  InterruptStatus = "resolved"  // 用户确认，动作已执行
	InterruptCancelled InterruptStatus = "cancelled" // 用户取消
	InterruptExpired   InterruptStatus = "expired"   // 超时未回应
)

// InterruptKind 中断类型。
//
// 一期只有建单确认；二期 MCP 写操作复用同一状态机。
type InterruptKind string

const (
	InterruptTicketCreation InterruptKind = "ticket_creation_confirmation"
)

// ConfirmationDecision 用户对确认请求的答复。
type ConfirmationDecision string

const (
	DecisionConfirm ConfirmationDecision = "confirm"
	DecisionCancel  ConfirmationDecision = "cancel"
	DecisionUnknown ConfirmationDecision = "unknown"
	// DecisionHasNewDemand 表示这条消息在提出新的诉求（典型形态：
	// 「还是没弄好，帮我建个单跟进」），而不是在答复当前待确认的草案。
	//
	// 它必须与 DecisionConfirm 区分开：待确认草案是上一轮定下的，
	// 而用户这时要登记的往往是另一件事（或同一件事的新进展），
	// 直接拿旧 Payload 建单会建错单，同时把新诉求丢掉。
	DecisionHasNewDemand ConfirmationDecision = "has_new_demand"
)

// 确认与取消关键词。
//
// 两条刻意的约束，都是实测踩坑后加的：
//
//  1. 不含单字「是」。它会被「这是」「但是」「不是」大量命中，
//     实测「这是什么意思」被判成了确认——在写操作场景下这是危险的误判。
//     需要表达确认时用「是的」「确认」这类更长的词。
//  2. 英文词按整词匹配，中文按子串匹配。中文没有词边界概念，
//     子串匹配符合表达习惯；而英文若用子串，"yes" 会命中 "yesterday"、
//     "no" 会命中 "notice"。
var (
	confirmWords = []string{"确认", "确定", "是的", "好的", "可以", "同意", "继续", "没问题", "yes", "ok", "confirm", "approve"}
	cancelWords  = []string{"取消", "不用", "不需要", "不要", "算了", "放弃", "cancel", "abort"}
)

// nonApprovalPhrases 命中时，即便句子里有确认词也不算批准。
//
// 三类的共同点：「确认」在中文口语里经常不表示"我同意这份草案"——
//   - 否定式：「不确定」「无法确认」。「确认」是「不确定」的子串，
//     纯子串匹配会把「这个我还不确定」判成批准并直接建单（实测）；
//   - 自述核实：「确认一下」「核实一下」主语是用户自己，
//     意思是"我还要去查"，不是"我同意了"；
//   - 延后表态：「再说」「稍等」「先不」明确还没给答复。
var nonApprovalPhrases = []string{
	"不确定", "不能确认", "无法确认", "没法确认",
	"确认一下", "我再确认", "核实一下", "核对一下",
	"再说", "稍等", "待会", "先不",
}

// newDemandPhrases 明确的新一轮建单/登记诉求。
//
// 词面取自实测样本与评测集（「帮我建个单」「提个工单」「帮我登记个工单」），
// 刻意保持具体：只用「建」或「工单」单字会命中大量无关表述。
var newDemandPhrases = []string{
	"建个单", "建工单", "建个工单", "提个工单", "帮我建", "帮我登记",
}

// ParseConfirmationDecision 解析用户的确认答复。
//
// 判定阶梯：新建单诉求 → 取消 → 确认 → 语义不明。
//
//   - 新建单诉求排最前：这句话要登记的事与上一轮定下的草案不一定是同一件，
//     交给带上下文的模型判断，比用词表替它决定更接近真实语义。
//     它本身不产生任何副作用，因此放在最前不会带来风险。
//   - 取消优先于确认：理由仍是风险不对称——误判为确认会真的建单，
//     误判为取消只是让用户重说一次。故对犹豫表述（"不用了，确认吧"）
//     一律取保守解释。
//   - 确认必须未被否决短语命中（见 nonApprovalPhrases）。
//
// 除 DecisionConfirm 外没有任何判定能触达建单，
// 这条不变式由 TestOnlyConfirmGrantsApproval 与编排层共同守住。
//
// 两侧都未命中时返回 unknown，由编排层带上下文澄清，而不是猜测。
func ParseConfirmationDecision(text string) ConfirmationDecision {
	value := strings.ToLower(strings.TrimSpace(text))
	if value == "" {
		return DecisionUnknown
	}
	if matchesAnyKeyword(value, newDemandPhrases) {
		return DecisionHasNewDemand
	}
	if matchesAnyKeyword(value, cancelWords) {
		return DecisionCancel
	}
	if matchesAnyKeyword(value, confirmWords) && !matchesAnyKeyword(value, nonApprovalPhrases) {
		return DecisionConfirm
	}
	return DecisionUnknown
}

// matchesAnyKeyword 判断文本是否命中任一关键词。
//
// 含中文的关键词按子串匹配，纯 ASCII 关键词按整词匹配。
// 见上方注释中关于 "yes"/"yesterday" 的说明。
func matchesAnyKeyword(text string, keywords []string) bool {
	for _, keyword := range keywords {
		if isASCIIWord(keyword) {
			if containsASCIIWord(text, keyword) {
				return true
			}
			continue
		}
		if strings.Contains(text, keyword) {
			return true
		}
	}
	return false
}

// isASCIIWord 判断关键词是否只由 ASCII 字母数字组成。
func isASCIIWord(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r > unicode.MaxASCII {
			return false
		}
	}
	return true
}

// containsASCIIWord 按整词匹配 ASCII 关键词。
//
// 词边界定义为「非字母数字」字符：因此 "ok!"、"ok,"
// 都能命中，而 "yesterday" 不会命中 "yes"。
func containsASCIIWord(text, word string) bool {
	index := 0
	for {
		found := strings.Index(text[index:], word)
		if found < 0 {
			return false
		}
		start := index + found
		end := start + len(word)

		leftOK := start == 0 || !isWordRune(rune(text[start-1]))
		rightOK := end >= len(text) || !isWordRune(rune(text[end]))
		if leftOK && rightOK {
			return true
		}
		index = start + 1
		if index >= len(text) {
			return false
		}
	}
}

// isWordRune 判断字符是否属于「词内字符」。
func isWordRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	default:
		return false
	}
}

// Interrupt 一次待恢复的确认中断。
//
// 采用中断-恢复而非同步阻塞：真实客服场景中用户可能思考很久才回复，
// 同步阻塞会因超时而丢单，且占用连接。落库后由用户的下一条消息触发恢复。
type Interrupt struct {
	ID             int64
	ConversationID int64
	Kind           InterruptKind
	Status         InterruptStatus
	// CheckPointID 恢复时用于定位该中断，对用户不可见。
	CheckPointID string
	// Prompt 展示给用户的确认文案。
	Prompt string
	// Payload 恢复时执行动作所需的全部数据。
	//
	// 必须完整落库：恢复可能发生在进程重启之后，不能依赖内存状态。
	Payload TicketInput
	// ResultTicketID 恢复成功后生成的工单 ID。
	ResultTicketID int64
	// ResumeCount 已尝试恢复的次数，用于诊断反复确认不上的情况。
	ResumeCount int
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ExpiresAt   time.Time
}

// Validate 校验中断数据。
func (i Interrupt) Validate() error {
	if i.ConversationID <= 0 {
		return errors.New("中断必须关联会话")
	}
	if strings.TrimSpace(i.CheckPointID) == "" {
		return errors.New("中断必须有 CheckPointID")
	}
	if i.Kind == "" {
		return errors.New("中断必须有类型")
	}
	return nil
}

// Expired 判断中断是否已过期。
func (i Interrupt) Expired(now time.Time) bool {
	if i.ExpiresAt.IsZero() {
		return false
	}
	return now.After(i.ExpiresAt)
}

// CanResume 判断该中断当前是否可被恢复。
//
// 只有 pending 可恢复：已 resolved/cancelled 的中断若再次恢复，
// 会重复执行写操作（例如重复建单）。
func (i Interrupt) CanResume(now time.Time) error {
	if i.Status != InterruptPending {
		return fmt.Errorf("%w: 中断状态为 %s，不可恢复", ErrInvalidTransition, i.Status)
	}
	if i.Expired(now) {
		return fmt.Errorf("%w: 中断已于 %s 过期", ErrInvalidTransition, i.ExpiresAt.Format(time.RFC3339))
	}
	return nil
}

// ErrInterruptNotFound 表示未找到待恢复的中断。
var ErrInterruptNotFound = errors.New("未找到待恢复的确认中断")
