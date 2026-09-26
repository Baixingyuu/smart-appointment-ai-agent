// 建单交互"进展驱动追问"的跨轮状态。
//
// 它随 Interrupt.Payload 一起落库（见 domain.TicketInput.Intake），因此刻意不含
// 指针/时间等复杂字段——保持可 JSON 序列化，进程重启后仍能续上追问进度。
//
// 为什么挂在草案上而不是会话全局：待确认中断按"最新的 pending"取，旧草案会被取代；
// 追问进度理应"跟着一份草案走"。换需求 → 新草案 → 进度自然重置，
// 不会把上一件事问过的槽位算到这一件事头上。
package domain

// IntakeProgress 记录一份建单草案在追问过程中的进展。
type IntakeProgress struct {
	// AskedSlots 是已经就其向用户追问过的阻塞槽位集合。
	// 某槽位再次为空且已在其中 → 不再追问该槽位，建单并记 MissingInfo（见 TICKET_INTAKE §2.2）。
	AskedSlots []string `json:"asked_slots,omitempty"`
	// AskRounds 是这份草案累计向用户追问的轮次，仅用于防死循环的安全阀。
	AskRounds int `json:"ask_rounds,omitempty"`
}

// Asked 报告某槽位是否已追问过。
func (p IntakeProgress) Asked(slot string) bool {
	for _, s := range p.AskedSlots {
		if s == slot {
			return true
		}
	}
	return false
}

// MarkAsked 幂等地把一个槽位记为"已追问"，返回新副本（不改收值，避免共享底层数组）。
func (p IntakeProgress) MarkAsked(slot string) IntakeProgress {
	if p.Asked(slot) {
		return p
	}
	next := make([]string, len(p.AskedSlots), len(p.AskedSlots)+1)
	copy(next, p.AskedSlots)
	next = append(next, slot)
	p.AskedSlots = next
	return p
}
