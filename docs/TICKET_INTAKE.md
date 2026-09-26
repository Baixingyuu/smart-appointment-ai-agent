# 建单交互（Agent + 用户 → 工单落库）

> 定稿于 2026-09-24。本文档是 `FOCUS.md §二`（主线 2：建单交互）的执行设计，与派单侧的
> `DISPATCH_PIPELINE.md` 同级。指标定义仍以 `EVALUATION_PLAN.md` 为准。
> 本文只讲"从用户开口到工单落库这段交互怎么设计"，不重复"为什么是三条主线"。

## 0. 一句话与三支柱

**目标**：既不因为"追问太烦"丢单，也不因为"一步建单"给处理人一张空工单。

三根支柱，全部建立在一条既有公理上 —— **"宁建 MissingInfo 工单，也不冒用户跑掉不建单的风险"**（`FOCUS.md §二`）：

| 支柱 | 一句话 | 取代的现状 |
| --- | --- | --- |
| **进展驱动追问** | 只在缺"阻塞性槽位"时追问，问到没进展就建单；**不设轮次上限**，只留一个防死循环的安全阀 | 现在 prompt 反向要求"缺信息也建单"，等于**根本不追问** |
| **可追溯性判定** | 模型声称"这个字段填了""描述里这条是真的"，必须能在用户原话里追溯 | 现在 `missingInfo` 与 `description` 全靠模型自报，代码零校验 |
| **永远可达的建单出口** | 用户任何时候说"直接建单"都能立刻落库，不被追问卡住 | 现在只有 `ticket_create_confirm` 一条路径，无主动出口 |

**这三者共用一个底层组件 `TraceabilityChecker`**（§3），不是三套逻辑。

---

## 1. 现状诊断（精确到行）

| 项 | 事实 | 位置 |
| --- | --- | --- |
| 回合执行 | `Agent.Run(ctx, TurnInput)`；有界工具循环 `MaxToolRounds=5`（4 轮够用 + 1 备用） | `agent.go:279`, `:328`, `:70` |
| 追问 | **未实现**。系统提示 rule 5 明确"信息不全也建单，别因缺次要信息拒绝" | `agent.go:826-829` |
| MissingInfo | 100% 模型决定：`ticket_create_confirm` 的 `missingInfo` 数组参数 | `tools.go:165-169` → `agent.go:678` |
| 轮次计数 | `Interrupt.ResumeCount` 只记录、从不参与决策，注释自认"用于诊断" | `interrupt.go:209-210` |
| 起草忠实度 | `description` 从模型 args 原样透传，无任何回查 | `agent.go:663-703`（`description` 读取在 `:668`） |
| 主动建单出口 | 无；唯一路径是 `ticket_create_confirm`（RiskWrite + 必确认，Handler 恒 error） | `tools.go:142-143`, `:176` |
| 确认阶梯 | `newDemand → cancel → confirm → unknown`，只有 confirm 触达建单 | `interrupt.go:105-120` |
| 关键词表 | `confirmWords/cancelWords/newDemandPhrases/nonApprovalPhrases` 四张表已成型 | `interrupt.go:62-87` |
| 意图短路 | ✅ chit-chat / handoff / out_of_scope 一次 Chat、零工具 | `routing.go:21-49` |
| 中断-恢复 | ✅ D1/D2/D3 已修（见 `HANDOVER §10`） | — |
| 跨轮历史 | ✅ 6 条 / 单条 220 / 合计 900 rune，**从 store 读**（`MessagesByConversation`） | `history.go:36-39` |
| 已知开放缺陷 | 描述可含用户没说过的内容（脚本 B 编了「已尝试联系财务确认政策」） | 无轴可见，本文支柱二处理 |

**结论**：需要动手的只有四件事 —— 追问、轮次安全阀、可追溯性、主动建单出口。其余是回归保护。

---

## 2. 支柱一：进展驱动的追问（不设轮次上限）

### 2.1 区分阻塞性缺失与次要缺失

只在缺"阻塞性槽位"时追问。每个 `Category` 定义 blocking 槽位，**1 个为主、至多 2 个**（多了是设计者贪心，用户答不完）：

| Category | blocking 槽位 | 为什么阻塞 |
| --- | --- | --- |
| `incident` | `affected_system` | 不知道哪个系统出问题，处理人无从查起，也接不上派单的服务归属 |
| `consultation` | `asked_topic` | 不知道在问什么，无法组织回答 |
| `request` | `requested_action` | 不知道要执行什么操作，工单没有可执行内容 |
| `change` | `target_service` + `planned_window` | 变更：改哪 + 什么时候，缺任一都无法评估风险 |

其余信息（复现步骤、影响人数、报错截图…）**一律次要** → 建单后记进 `MissingInfo` 让处理人补，不追问。

规则表放 `internal/ticket/blocking_slots.go`（一期手写常量，不推导）。

### 2.2 终止靠"字段进展"，不靠计数

- 每个 blocking 槽位在会话内记 `askedEmptyTwice`（这个字段问过一次、用户答了但仍空）。
- **某字段问了一次、用户补充后该字段仍为空 → 这个字段不再追问**，建单 + 记 MissingInfo。
- 只要用户每轮都在把"某个还空着的槽位"填上，就一直追问，**不设总次数上限**。

关键区别：数的是"同一字段第几次仍空"，不是"这条会话一共追问了几轮"。

### 2.3 安全阀（防 bug，不是业务规则）

单会话累计追问回合 **> 10** → 无条件建单 + MissingInfo 注明 `safety_valve_reached`。

- 它只在"某字段判空逻辑出 bug、导致一直追问"时兜底；正常路径靠 §2.2 的进展终止，**摸不到 10**。
- 与 `MaxToolRounds=5` 无关：后者限的是"一条用户消息内模型连续调几次工具"，前者限的是"跨消息来回追问几轮"，两回事。
- 计数落在会话上（不是 Interrupt 上）：一次会话可能因换需求产生多个建单草案，安全阀要防的是"整条会话对用户的总打扰"。

### 2.4 追问话术：一次列全，不挤牙膏

- 第 1 次触发追问：把**所有**还空的 blocking 槽位一次列全问用户（"为了建单还需确认：① 是哪个系统？② ……"）。
- 不逐字段问 —— 逐字段把一次能完成的事摊成多轮，是打扰的主要来源。

---

## 3. 支柱二：TraceabilityChecker（可追溯性判定）

三处需求其实是同一个问题——"某段文本能不能追溯到用户说过的话"：

1. `extracted_slots` 的 quote 是否真出自用户原话（§4）
2. `description` 里每条事实声明是否有用户依据（§1 的已知缺陷）
3. quote 的部分匹配容错（小模型给不出逐字 quote）

**一个组件，三个入口**，且三处口径天然一致。

### 3.1 契约

```go
// internal/agent/traceability.go
type Verdict string
const (
    VerdictTraced     Verdict = "traced"     // 字面可追溯
    VerdictEntailed   Verdict = "entailed"   // 语义可推出
    VerdictUnverified Verdict = "unverified" // 拿不准，保守标注
)

type Claim struct{ ID, Text string }
type ClaimVerdict struct{ ID string; Verdict Verdict; Reason string }

// TraceabilityChecker 批量判断一批 claim 能否追溯到 userCorpus（本会话用户原话集合）。
// 批量而非逐条：一次建单里 description 拆出的多条声明与多个槽位 quote 一起判定，
// 给实现留出"合并成一次模型调用"的优化空间（ID 用于把批量结果对齐回输入顺序）。
type TraceabilityChecker interface {
    CheckAll(ctx context.Context, claims []Claim, userCorpus []string) ([]ClaimVerdict, error)
}

// 三个构造函数（scripted / rule / 两级）都在 traceability.go，不再拆单独文件。
func NewScriptedTraceabilityChecker() TraceabilityChecker              // 规则级，离线/评测默认
func NewRuleTraceabilityChecker(cfg TraceabilityConfig) TraceabilityChecker // 可调阈值的规则级
func NewTwoLevelTraceabilityChecker(model llm.ChatModel, cfg TraceabilityConfig) TraceabilityChecker // 规则 + 语义
```

`userCorpus` = `store.MessagesByConversation(convID)` 过滤 `Sender == SenderCustomer`。已可达，无需新依赖。AI 自己的话不能反过来"证明"自己说过的事实，否则编造会自证。

### 3.2 两级判定，便宜的先筛

与派单三段同一漏斗思想：不为先进，为省。

**第一级 · 规则（免费、快）**
- 归一化 claim 与语料（去标点/空格、全半角统一、大小写统一）。
- 判 `VerdictTraced`：归一化后子串命中，**或** token 重叠率 ≥ `0.70`。阈值是 `TraceabilityConfig.TokenOverlapThreshold` 的默认值（一期为结构体字段，不外置到 env；0.70 靠人肉抽检标定，见 §10）。
- 这一级同时消化："登不上" vs "无法登录" 的字面交集，以及 §4 里 quote 的部分匹配容错。
- 命中 → 返回 `VerdictTraced`，**不再走第二级**。

**第二级 · 语义（只处理第一级没过的小集合）**
- 本地 `qwen3:8b`，`temperature=0`。premise = 用户全部消息拼接，hypothesis = 可疑 claim。
- 判定提示：能推出 → `VerdictEntailed`；否则（矛盾 / 无信号 / 调用失败）→ `VerdictUnverified`。方向刻意保守。
- **一期逐条调用**：接口虽是 `CheckAll`（批量），但两级实现对每条没过规则的 claim 各问一次模型。把整批合并进一个 prompt 减少调用次数，是已识别的**二期**成本优化——一期用本地模型验正确性，不承诺成本数字（见 §9）。
- 无模型（离线评测 scripted 模式 / `NewScriptedTraceabilityChecker`）时：只有第一级，规则没过即 `VerdictUnverified`，退化不报错——与现有 scripted/live 双模约定一致。

### 3.3 只标注，不拦单

`Unverified` 的 description 声明 → `MissingInfo += "fidelity_unverified"`，并在确认提示里显式列出：

> "以下几处我在您的描述里没直接看到，请核对是否属实：① 已尝试联系财务……"

理由：语义判定也不是 100% 准，拦单会把假阳变成丢单，违反 §0 的公理。**忠实度一期定位是"提高透明度"，不是"阻断"。**

---

## 4. 支柱二落地：extracted_slots 带 quote（重版）

`ticket_create_confirm` 参数新增一个数组。模型对**每个它声称已填的 blocking 槽位**，逐字摘一段用户原话当证据：

```json
{
  "title": "...",
  "description": "...",
  "extractedSlots": [
    { "name": "affected_system", "quote": "核心下单接口" }
  ]
}
```

判定（复用 §3 的 checker，不另写逻辑）：
- quote 过 `TraceabilityChecker` → 认可该槽位已填。
- quote 没过（模型编的），或该槽位压根没给 quote → 判"未填" → 进 §2 的追问流程。

**为什么是重版**：quote 同时喂给 §2（哪些字段真填了）和 §3（描述有没有依据）一套可信证据。轻版（只让模型给 `slotFilled: bool`）省一点 token，但退化成模型自评——那么追问判定就建在沙上。多出的 token 成本约 +3~5%，换取整条链路可核查，值。

**对小模型的容错内建在 §3 第一级里**（部分匹配），不额外处理。

---

## 5. 支柱三：forceCommit 主动建单出口

在 `ParseConfirmationDecision` 阶梯里插一支 `DecisionForceCommit`，命中即用当前待确认草案立即建单、跳过追问：

```go
var forceCommitPhrases = []string{"直接建单", "直接建", "先建单", "直接提交", "提交吧", "别问了"}
// 命中 → DecisionForceCommit：立即建单，MissingInfo 记 "user_forced_commit"。
```

沿用四张既有关键词表同一实现（`matchesAnyKeyword`，中文子串、英文整词）。

**两处对原词表的刻意收敛**（否则会破坏既有回归）：
- 不含「就这样」「行了就这样」。既有确认用例已把「OK 就这样」等锁定为 `DecisionConfirm`；收进 forceCommit 会把这些用例改判。forceCommit 只收明确祈使句。
- 不含「不用再问」。它与取消词「不用」同前缀，而取消排在 forceCommit 之前，收进来也永远命中不到，反而误导；用「别问了」表达"停止追问"这一意图。

**最终阶梯顺序**：`newDemand → cancel → forceCommit → confirm → unknown`。

- newDemand 最前：「帮我建个单」既像 forceCommit 又像新登记另一件事，交回模型带上下文判断比词表更准，且它无副作用。
- **cancel 在 forceCommit 之前**：仍是风险不对称——误判 forceCommit 会真建单，误判 cancel 只是让用户重说一次。故对犹豫表述（"算了，就这样吧"）一律取无副作用的取消解释。
- 只有 `GrantsApproval()` 为真（confirm / forceCommit）的判定能触达建单，由 `TestGrantsApprovalInvariant` 与编排层共同守住。

**成本极低、价值极高**：这是"用户目标永远可达"的 UX 红线。真实客服最贵的失败是"用户不耐烦跑了、单都没建"。

---

## 6. 判定流总览

```
用户消息
  │
  ├─ forceCommit 短语？ ─是→ 立即建单（MissingInfo: user_forced_commit），结束
  │
  ├─ 模型抽字段 + 对每个已填 blocking slot 给 quote
  │
  ├─ 每个 quote 过 TraceabilityChecker
  │     · 未过 → 该 slot 视作空 → 计入"该字段第几次仍空"
  │     · 空且首次 → 一次列全追问（本轮不建单）
  │     · 空且已问过一次仍空 → 停止追问该字段，建单 + MissingInfo
  │
  ├─ description 拆原子 claim，逐条过 TraceabilityChecker
  │     · Unverified → MissingInfo += fidelity_unverified（确认提示里列出，不拦单）
  │
  ├─ 会话累计追问 > 10（安全阀）→ 无条件建单 + safety_valve_reached
  │
  └─ 所有 blocking slot 已填（或已放弃追问）→ 走 ticket_create_confirm → 用户确认 → 落库
```

---

## 7. 代码集成点（仅新增，不动 agent 主循环）

```
新增：
  internal/ticket/blocking_slots.go    // Category → blocking 槽位规则表（含 ParseExtractedSlots）
  internal/agent/traceability.go       // TraceabilityChecker 接口 + 规则级(scripted) + 两级实现（不再拆 scripted 单文件）
  internal/agent/intake.go             // 追问编排：decideIntake 纯决策 + evaluateIntake 接线 + 安全阀
  internal/domain/intake.go            // IntakeProgress（跨轮追问进度值类型：AskAsked/AskRounds）

改动（小、局部）：
  internal/domain/interrupt.go         // ParseConfirmationDecision 增 DecisionForceCommit 一支 + GrantsApproval 不变式
  internal/agent/tools.go              // ticket_create_confirm 增 extractedSlots —— 仅当 a.traceability != nil 才声明，
                                       //   默认（未启用）部署不为用不到的字段付固定 prompt 开销
  internal/agent/agent.go              // 两处小钩子：executeTool 的 RequireConfirmation 分支前置 intake 门；
                                       //   resume 增 DecisionForceCommit 建单分支；再加一个按会话的内存 map intakeStates
                                       //   （不新增 Store 接口；建单成功即清除）
  internal/domain/domain.go            // TicketInput 增 Intake 字段，把跨轮进度随草案落库供审计

不碰：
  有界工具循环、四道闸治理、确认中断-恢复状态机主体、跨轮历史窗口、意图短路 —— 均已完成且有回归覆盖
```

**接线复用**：`userCorpus` 与追问判定都用现成的 `store.MessagesByConversation` + `SenderCustomer` 过滤，与派单侧 `Directory` 一样保持"构造注入、无隐藏 I/O"。

---

## 8. 评测怎么接（端到端交互轴）

> **已落地（2026-09-24）**：下表 8 例与三条 sabotage 已在 `internal/agent/intake_test.go` 实现，
> 全部经共享测试装置走 `conversation.Service.Send`（装置顺带记下完整 `TurnResult`，
> 好让"一次列全"的追问观察可被断言）。纯决策 `decideIntake` 另有一组单测打透终止/安全阀规则。

`FOCUS.md §三` 定的"端到端交互轴"落地时，**runner 必须走 `conversation.Service.Send` 而非 `ag.Run`** —— 这样跨轮历史窗口才有内容（修掉 D1 结构盲区，`HANDOVER §10`）。

最小 case 集 **8 条**，每条覆盖一种失败模式：

| # | 场景 | 断言 |
| --- | --- | --- |
| 1 | 一句话信息全 | 单轮建单，零追问 |
| 2 | 缺 affected_system | 追问 1 次，一次列全缺失 |
| 3 | 追问后补齐 | 第 2 轮建单，无 fidelity 问题 |
| 4 | 补充但仍缺同一字段 | 该字段停止追问，建单 + MissingInfo |
| 5 | 模型给不出可追溯 quote | 该 slot 判空，触发追问 |
| 6 | 用户中途"直接建单吧" | forceCommit 生效，无追问 |
| 7 | 描述含用户没说过的声明 | MissingInfo 出现 fidelity_unverified，**但仍建单** |
| 8 | 中途换需求 | 现有 newDemand 分支覆盖，作回归（不新增判据） |

**指标**：追问放弃率（触发追问后用户不再回复的会话占比）、忠实度检出率（注入编造样本 vs 检出比）、一次列全 vs 挤牙膏的对比（后者是 baseline）。

**sabotage 三条**（证明承重）：
- 把重叠阈值调到 1.0（几乎不判 Traced）→ 忠实度假阳率暴涨、追问次数暴涨。
- 去掉 forceCommit → case 6 卡在追问里建不了单。
- 关掉安全阀 + 故意让某字段判空 bug → 会话无限追问（检出）。

---

## 9. 成本

| 项 | 估算 | 说明 |
| --- | --- | --- |
| extractedSlots token | +3~5% / 次建单 | 多输出一个数组 |
| Traceability 第二级 | 每次建单 0~1 次额外调用 | 多数 claim 第一级 Traced；只对没过的批量走一次 |
| 本地 qwen3:8b | 近零边际成本 | `make check` / scripted 不调模型 |
| 外部服务替换第二级 | 与派单 Stage 2 合并算 | 见 `DISPATCH_PIPELINE.md §4` |

一期用本地模型验正确性，不承诺外部服务成本数字。

---

## 10. 明确不做（触发条件表）

| 组件 | 触发加回来的条件 |
| --- | --- |
| 规则+语义两级 → 全语义 LLM judge | 人肉抽检 30 条建单，规则级**假阳率 ≥ 20%** |
| blocking 槽位手写表 → 从服务字典推导 | 槽位种类 > 12 或每类维护不住；或服务字典 > 30 条 |
| 一次列全 → 按重要度分次问 | 首条追问放弃率 ≥ 30%（当前假设"一次列全更友好"） |
| forceCommit 词表 → 意图分类模型 | 词表误触率 > 5%（人审日志发现"我不想建单"被判成建单） |
| 安全阀 10 → 概率化/按 category 差异化 | 有 ≥ 500 条多轮真实对话且分桶显示某桶明显偏 |
| quote 部分匹配 → 严格全等 | 假阴率被证明由部分匹配引起（届时是收紧阈值，不是换架构） |
| 忠实度"只标注" → "拦单" | 假阳率降到 <5% 且业务明确愿意为此承担少量丢单 |

**每条都是规模/数据驱动，不是技术先进驱动。**

---

## 11. 与派单侧的接缝

- `incident` 的 blocking 槽位 `affected_system` 一旦填上，正好是派单 Stage 1.1 服务解析想要的强输入 —— 建单交互把"哪个系统"问清楚，等于给 `DISPATCH_PIPELINE.md §1.1` 的 BM25 解析喂了最干净的关键词。
- 但**一期不共享字段**：建单只把 `affected_system` 记进 `MissingInfo`/描述，不写 `Ticket.SuspectedServiceID`（domain 未加此字段，见 `DISPATCH_PIPELINE.md` 偏差记录）。真接线等派单 v2 数据集就位再评估。

---

## 12. 与其他文档的关系

| 文档 | 关系 |
| --- | --- |
| `FOCUS.md §二` | 本文是 §二 的展开；scope 决策仍以 FOCUS 为准 |
| `DISPATCH_PIPELINE.md` | 同级执行设计；§11 是两者接缝 |
| `HANDOVER.md §10` | D1/D2/D3 现状权威；本文不重述修复，只在其上增量 |
| `EVALUATION_PLAN.md` | 指标定义权威；§8 指标解释以此为准 |
| `PHASE_ROADMAP.md` | 长期方向；本文是一期能落地的最小版本 |

**实现顺序建议**：TraceabilityChecker（含 scripted）→ blocking_slots 规则表 + 进展终止 → forceCommit → extracted_slots 接线 → 端到端交互轴 8 例 → `make check`。每步保持全绿。
