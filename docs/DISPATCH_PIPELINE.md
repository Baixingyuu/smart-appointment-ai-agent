# 派单三段流水线（Dispatch Pipeline）

> 定稿于 2026-09-24。本文档是 `FOCUS.md §一`（主线 1：派单）的执行设计。
> scope 层仍以 `FOCUS.md` 为准；本文档只讲"三段流水线怎么实现"，不重复"为什么是这个架构"。
> 指标解释以 `EVALUATION_PLAN.md` 为准。

## 0. 一句话与三段决策

**决策链**：先用不依赖 LLM 的规则+检索给出高分派；判定"弱"才升级到 LLM；LLM 也说不出或给出候选集外 ID，再升级到人工。

| 段 | 手段 | 目标占比 | 单位成本 | 输出 |
| --- | --- | --- | --- | --- |
| **Stage 1** | 归属解析 + kNN + 硬约束过滤 + 加权打分 | ~80% | 近零（< 1ms CPU） | 结构化 `Decision` |
| **Stage 2** | LLM function-calling，enum 只允许 Stage 1 的 top-N + `ESCALATE_HUMAN` | ~15% | ~几分钱、~1-3s | 结构化 `Decision` |
| **Stage 3** | 人工待认领池，无候选/LLM 拒绝/候选集外 ID 三种入口都汇到这里 | ~5% | 一次人审 | 派单 or 拒绝 |

Stage 1 内部**不是级联优先级**——归属、相似、可用度是**同一打分器的独立特征**，
这样才能力调（改权重看整体效果）而不是每段各调各的。硬约束过滤（不在职、满载、权限域外）
先于打分，与现有 `Assigner.score` 保持一致。

Stage 2 只在 Stage 1 判"弱"时启用。**判弱不是打分的连续值，是三类离散原因**（见 §2.5），
这样"为什么升级"这件事本身可评测、可归因，而不是"模型觉得不太像"。

---

## 1. Stage 1 五段详解

Stage 1 一次跑完五小段，每一段独立可测，任何一段退化只影响自己那一段的指标：

```
Ticket ──► 1.1 resolve ──► 1.2 candidates ──► 1.3 eligibility ──► 1.4 score ──► 1.5 weakness
           (服务归属解析)  (相似历史 kNN)     (硬约束过滤)      (加权打分)      (是否升级)
```

### 1.1 resolve：工单 → 服务节点

**目的**：把工单文本映射到一个或多个服务节点（`ServiceID`），拿到它们的 `OwnerID / BackupOwnerID / TeamID`。

**做法**：复用现有 `internal/rag.Retriever` 与同一套 BM25 打分（中文 bigram + 英文分词 + 归一化 + OOV 项剔除）。
语料 = 服务字典（`seed/service_catalog.go`），每条文档 = 服务名 + 别名 + 描述 + 关键组件关键词。
门控沿用 `Retriever` 的 6 桶 `Reason`；Stage 1 只关心两件事：
- `TopKServices`（默认 3）
- 最高分是否 ≥ `ServiceMatchFloor`（默认 0.40）

**为什么不 LLM**：这一段本质是"字符串集合匹配封闭枚举"，BM25 已经够；且服务字典是人工维护的枚举，
一旦上 LLM 抽取，"抽出一个不存在的服务 ID"就成了新型缺陷。这段留给 Stage 2 做（有 enum 约束）。

**接口**（`internal/assign/resolve.go`）：
```go
type ServiceResolver interface {
    Resolve(ctx context.Context, ticket domain.Ticket) ([]ServiceMatch, error)
}
type ServiceMatch struct {
    ServiceID int64
    Score     float64
    Reason    string // 复用 rag.Reason 值域
}
```

### 1.2 candidates：相似历史工单 kNN（可选，无数据时旁路）

**目的**：从过去已解决工单里找 top-K 相似，它们的 `resolver_id` 进候选。

**当前实现**：**接口先立起来，实现走"空返回"**。原因：种子数据里没有历史派单日志，
造出来的假历史会让评测过拟合到"造出来的分布"。等有真实工单再灌索引。

**接口**（`internal/assign/candidates.go`）：
```go
type SimilarTicketIndex interface {
    FindSimilar(ctx context.Context, ticket domain.Ticket, k int) ([]SimilarHit, error)
}
type SimilarHit struct {
    TicketID   int64
    ResolverID int64
    Score      float64
}
```

有数据之后：`rag.NewBM25Embedder(resolvedTickets)` 索引一次，`FindSimilar` 就是 retrieve + 取 assigneeId。

### 1.3 eligibility：硬约束过滤

**目的**：把根本不该参与的候选（不在职 / 满载 / 权限域外）先踢掉，不进入打分。

**沿用现有规则**：`Assigner.score` 的 `!emp.Active → filtered` / `!emp.HasCapacity() → filtered`
是正确形态，Stage 1 直接把它前置。新增：
- **Level 门槛**：`Priority == P0` 时要求 `Level >= Senior`。这条规则是**分诊语义**：
  P0 出问题时宁可让 senior 半夜上来，不能让新人接了再升。规则写在 `eligibility.go`，不放 LLM。
- **OnCall 与值班窗口**：一期只加字段不加规则。真实值班表要接 HR / IM 才可信；
  加了假数据只会让评测学到"on-call 是硬约束"，而实际不是。

**接口**（`internal/assign/eligibility.go`）：
```go
func Filter(ticket domain.Ticket, employees []domain.EmployeeExtension) ([]EligibleCandidate, []FilteredCandidate)
```
过滤掉的**仍然要进 `Candidates` 明细**（现有 `AssignmentLog` 已经是这个形态），
否则"为什么没选他"没法复核。

### 1.4 score：加权打分（ownership-first）

**目的**：把 ownership / 相似 / 可用度合成一个总分，产出排序后的 top-N。

**特征与权重**（`internal/assign/score.go`）：

| 特征 | 记号 | 权重 | 取值 0..1 | 来源 |
| --- | --- | --- | --- | --- |
| Ownership | `s_own` | 0.40 | owner=1.0 / backup=0.3 / team=0.1 / 其他=0 | resolve 输出 |
| Similar-history | `s_sim` | 0.20 | kNN 命中的最高分 | candidates 输出 |
| Availability | `s_avail` | 0.20 | `1 - loadRatio` | Employee 现有字段 |
| Seniority | `s_senior` | 0.10 | P0/P1 时 junior=0，其他情况=0.5；senior=1.0 | Level 新字段 |
| Recency | `s_recent` | 0.10 | Employee.Recency | 现有字段 |
| Skill（弱信号） | `s_skill` | 0.00 | Jaccard（保留接口） | 现有 SkillSet |

`s_skill` 权重归零但保留接口，是**诚实边界**：现有 120+30 派单数据集是围绕技能打的，
零权重之后它们就没意义了。因此现有 `Assigner` 不删——**降级为"排序段单元回归"**，
只测 Jaccard + 排序 + tiebreak 是否稳定，不再声明"端到端派单准确率"。

**接口不新造**：继续用 `domain.CandidateScore` 结构，新增分项字段 `OwnScore / SimScore /
SeniorityScore`。旧的 `SkillScore / LoadScore / RecencyScore / Total` 全保留，
`Total = Σ weight_i × score_i`；`Skills` 零权重时等价于 `Total = 0.4 own + 0.2 sim + 0.2 avail + 0.1 senior + 0.1 recent`。

**确定性 tiebreak**：沿用 `Assigner.sortCandidates` 的规则——总分 → 负载 → 响应 → ID 升序。
这是现有派单器最重要的性质之一，不能因为换打分器丢掉。

**重标定记录（2026-09-24，由 v2 评测驱动）**：`ownershipScore` 从 `1.0/0.6/0.3` 改为
`1.0/0.3/0.1`。原因：旧 backup 档位（0.6）让"mid 的 owner vs senior 的 backup"分差只有
`0.4×(1.0-0.6)=0.16`，再被 seniority 反号（`0.1×(1.0-0.5)=0.05`）与 recency 差抵消后，
margin 落到 ≈0.125 < `MarginThreshold(0.15)` —— 结构上把本应直派的强命中全判成 `low_margin`。
压低 backup/team 两档后 owner 重新压得住噪声。v2 评测前后：判弱轴 55.6%→84.4%、
排序轴 57.5%→85.0%、漏斗 Stage1 直出 24→35（共 45）。**诚实边界**：这组数字只证明
"实现 == 我们宣称的规则"，不证明外部准确率；剩余失败是 BM25 多召回第二条过阈值服务
（ownership-leak）与真实抽取歧义两类，见 `docs/FOCUS.md`。

### 1.5 weakness：是否升级到 Stage 2

**目的**：**判弱是离散原因，不是连续置信度**。这样"为什么升级"可枚举、可评测、可回归。

三类原因（`internal/assign/weakness.go`）：

| 原因 | 判据（默认阈值） | 为什么算弱 |
| --- | --- | --- |
| `WeaknessLowMargin` | `top1.Total - top2.Total < MarginThreshold (0.15)` | 排序无区分度，LLM 或人加一条证据就能定案 |
| `WeaknessNoServiceMatch` | `max(resolve.score) < ServiceMatchFloor (0.25)` | 服务归属都没解析出来，八成是描述太散 / 新服务 |
| `WeaknessEmptyCandidates` | 过滤后 `len(eligible) == 0` | 全员不可用（罕见），Stage 2 也救不了，直接 Stage 3 |

Stage 1 判定流：
```
if eligible == 0            → WeaknessEmptyCandidates → Stage 3
else if serviceFloor 未达   → WeaknessNoServiceMatch  → Stage 2
else if margin 未达         → WeaknessLowMargin       → Stage 2
else                        → 直接派单，记 PathStage1Xxx
```

**margin 阈值 0.15 怎么来的**：**目前是拍的**。等有第一批人标数据（`assignment_annotation_blank.json` 150 条），
画"margin vs 派错率"曲线校准一次；没数据前把它当配置项暴露，评测里对每个阈值单独跑一遍。
这条不假装是"科学设定"，写清楚。

---

## 2. Stage 2：LLM chooser（慢通道，有 enum 契约）

### 2.1 输入契约

`internal/assign/llm_chooser.go`：只喂 5 样东西，其他一概不给。

1. 工单：`Title + Description + Category + Priority`
2. 服务解析结果：top-3 `ServiceMatch` + 各自 score
3. 候选员工：Stage 1 top-N（默认 5）的 profile 摘要
4. 判弱原因：`WeaknessReason` 之一（让模型知道自己在补哪一段的缺口）
5. 强制约束：**只能从这 5 个 ID + `ESCALATE_HUMAN` 中选**（function calling schema 里的 enum）

### 2.2 输出契约

```json
{
  "assignee_id":  "EMP_101" | "EMP_205" | ... | "ESCALATE_HUMAN",
  "rationale":    "引用具体 profile/past_ticket/ownership 字段的字符串",
  "confidence":   0.0 - 1.0
}
```

三条硬约束：
1. **enum 阻断幻觉 ID**：`assignee_id` 只允许 Stage 1 输出的 top-N ID + `ESCALATE_HUMAN`。
   违反 → 不重试，直接走 Stage 3。理由：一次幻觉说明这个 prompt 对当前模型超出能力，
   重试只会烧钱。把"幻觉率"作为独立指标（`PathStage2Invalid / Stage2Total`），
   而不是把它藏在"派单准确率"里。
2. **rationale 必须引用可核查字段**：不是"因为张三看起来合适"，是
   "张伟（101）ownership 明确包含核心下单接口，且近 30 天处理过 3 起同类 500 故障"。
   一期用规则断言（rationale 至少包含候选 profile 里出现过的一个字符串）粗判，
   不做质量分——这条与主线 2 的"起草忠实度"是同一类问题，共用机制。
3. **confidence 只上报不参与决策**：一期不做校准，`confidence` 是"给运营看的透明度信号"，
   不是控制流。校准的触发条件见 §六。

### 2.3 与现有 `llm.ChatModel` 的关系

复用 `llm.ChatModel.ChatWithTools` 与 `ToolSchema`，不新造 LLM 客户端。
关键设计：把"选择 assignee"注册成一个函数，参数 schema 里 `assignee_id` 是 `enum`。
这样 OpenAI-compatible 服务（含 qwen3:8b @ ollama）都会强制输出 enum 值域内的字符串。

### 2.4 ScriptedChooser（评测用）

`internal/assign/scripted_chooser.go`：假实现，从 fixture 里按 ticket_id 查表返回固定 JSON。
这样：
- `make check` 不需要网络 / 不需要 API Key
- sabotage 时能精确构造"LLM 输出候选集外 ID"这条路径
- live 评测才走 `OpenAIModel`

沿用现有评测的 **scripted / live 双模**约定（`HANDOVER.md §10`）。

---

## 3. Stage 3：人工兜底

**入口三种**：Stage 1 判 `WeaknessEmptyCandidates` / Stage 2 输出 `ESCALATE_HUMAN` /
Stage 2 输出候选集外 ID 触发 `PathStage2Invalid`。

**输出**：现有 `OutcomeFallbackPool` 语义完全兼容，改一个字段：
`AssignmentLog.Outcome = fallback_pool` 时 `Reason` 必须写清是哪一种入口。
这样"待认领池"这一指标能拆开看：
- **无人可用** 类 → 说明负载模型或员工配置有问题
- **模型放弃** 类 → 说明 Stage 1 特征不够，或 Stage 2 prompt 需要重写
- **模型乱猜** 类 → 说明 enum 之外的字段（evidence 引用）需要收紧

一期不做**采样审计**（`SampleRate` 只当配置项预留，不启用）。触发条件见 §六。

---

## 4. 指标与漏斗

| 指标 | 定义 | 一期目标 |
| --- | --- | --- |
| **Stage 1 命中率** | `Outcome=matched && Path starts stage1 / total` | 60-80% |
| **Stage 2 命中率** | `Outcome=matched && Path starts stage2 / total` | 10-20% |
| **Stage 3 兜底率** | `Outcome=fallback_pool / total` | ≤ 10% |
| **Stage 2 幻觉率** | `PathStage2Invalid / Stage2 调用总数` | ≤ 2% |
| **端到端 Top-1 准确率** | 人标数据集上 `assigneeId == expectedAssigneeId` | 待 v2 数据集就位 |
| **成本 / 单** | Stage 2 调用数 × 平均 token × 单价 / total | 目标 < 0.02 元 |

**300 单日单量成本预估**（qwen3:8b 走本地 ollama 时近零；若接外部服务，按 0.006 元/1k token × 平均 3k token/次）：
- Stage 1 全 CPU、无外部调用 → 0
- Stage 2 15% × 300 = 45 次 × 0.018 元 ≈ 0.8 元/日
- Stage 3 5% × 300 = 15 次人审，按 3 分钟/单 ≈ 45 分钟人力/日

**这个成本模型必须写在这里**：Stage 2 值不值这个钱，取决于人工时薪与业务复杂度。
一期用 qwen3:8b（本地免费）能验证正确性；换成外部服务的成本要重新算。

---

## 5. 代码集成点

**只加不改**（另有一个 session 在同仓库改动；保持 diff 小）：

```
新增文件：
  internal/domain/service.go             // Service / Team / EmployeeExtension / Level
  internal/assign/pipeline.go            // 三段编排 + Decision/Path/WeaknessReason 契约
  internal/assign/resolve.go             // Stage 1.1
  internal/assign/candidates.go          // Stage 1.2（当前返回空，接口先立）
  internal/assign/eligibility.go         // Stage 1.3
  internal/assign/score.go               // Stage 1.4
  internal/assign/weakness.go            // Stage 1.5
  internal/assign/llm_chooser.go         // Stage 2 LLM 版本
  internal/assign/scripted_chooser.go    // Stage 2 假实现（评测/单测用）
  internal/seed/service_catalog.go       // 15-20 服务节点 + 员工 ownership 映射

不改动：
  internal/assign/assigner.go            // 现有 Assigner 保留，降级为"排序段回归"目标
  internal/domain/domain.go              // 现有 Employee / Ticket 字段不变
  internal/rag/*                         // 通过 rag.Retriever 接口调用，不改内部
  internal/llm/*                         // 通过 ChatModel 接口调用，不改内部

Pipeline 与现有 Assigner 的关系：
  pipeline 自己算打分与排序，不调用 Assigner.Assign。原因：
    · 新特征（own/sim/senior/…）与 Jaccard-based SkillScore 不匹配，硬塞会引入"虚拟技能"这类权宜 hack；
    · Assigner 的 120+30 派单数据集是围绕 SkillScore 打的，保持它是"排序段回归"目标最省事。
  但从 Assigner 借三件确定性资产：sortCandidates 的 tiebreak 顺序、nearlyEqual 的误差容忍、
  Filtered+FilterReason 的候选人明细形态。这三件在 pipeline 中重实现一份（约 20 行），
  不合并到公共文件——避免与并发 session 的改动冲突。
```

**为什么 pipeline 复用 Assigner 的 tiebreak 而不是 SkillSet 编码**：现有 Assigner 的确定性排序、
`nearlyEqual` 误差处理、"被过滤者也要出现在 Candidates 明细"这三条是**已经验证过的资产**，
必须保留。但 ownership 特征不是"技能集合命中与否"的问题（owner/backup/team 是三档不同权重），
把它硬编码成虚拟技能会引入 Jaccard 分母膨胀的问题。因此打分独立实现、tiebreak 复制一份。

### 5.1 如何在生产入口启用

Pipeline 通过 `ticket.Dispatcher` 接口接入 `ticket.Service`，不改任何现有调用点：

```
internal/ticket/dispatch.go     // Dispatcher 抽象 + legacyAssigner / pipelineDispatcher 两个实现
  · ticket.New(st, *Assigner)   旧签名不变，内部包一层 legacyAssigner
  · ticket.NewWith(st, Dispatcher, WithDirectoryProvider(p))  新路径
  · pipelineDispatcher 在 Reason 前加 "[path|weakness]" 前缀，让落库日志能回溯走的是哪一段
```

生产入口 `cmd/helpdesk-agent/dispatch.go` 的 `newTicketService` 按环境开关装配：

```
DISPATCH_PIPELINE=on    切到三段流水线；否则默认 legacy Assigner
LLM_API_KEY / -api-key  提供模型时 Stage 2 用 OpenAIChooser；无模型（demo/offline）时不注入 chooser，判弱样本直落人工池
```

启动时把当前模式打到 stderr —— 派单结果差异背后就是这个开关，不打印会出现
"以为在跑 pipeline 其实没跑"这类无法回溯的误判。`serve` / `chat` / `demo` 三个入口共用这套装配。

一期默认仍走 legacy：pipeline 只在有限样本上验证过，`make eval-assign`（120+30）测的是旧 Assigner。
`ServiceMatchFloor` 默认 0.25 是在 14 条服务字典上按 BM25 分数分布定的，需用 v2 数据集重新校准。

**未接线事项**：`ticket.Service.AssignToBest` 用 `context.Background()` 驱动 Stage 2 ——
store 层尚未透传 ctx，所以请求取消时进行中的 LLM 调用不会中断。一期不影响正确性，接生产需补 ctx 贯穿。

---

## 6. 明确不做（触发条件表）

沿用 `FOCUS.md §六`，此处只列**本流水线范围内**的新拒项：

| 组件 | 触发加回来的条件 |
| --- | --- |
| BM25 服务解析 → cross-encoder rerank | 服务字典 > 200 条，或 resolve Top-3 准确率 < 85% |
| 加权打分 → LambdaMART | 错派样本累积 ≥ 200 且每周新增 ≥ 20 |
| LLM chooser 前置一层 LLM 抽取 → 独立抽取服务 | 服务解析准确率 < 70%，或需要多语言 |
| confidence 校准（isotonic / Platt / conformal） | ≥ 2k 条"派完 + 反馈"闭环数据 |
| 采样审计（`SampleRate > 0`） | 日单量 ≥ 5k，运营看不过来 |
| Stage 2 之上再加 LLM rerank 层 | Stage 2 命中率 < 60%（说明 5 个候选经常不够） |
| OnCall 硬约束 | 接入真实值班表（HR 系统 / IM bot） |
| 匈牙利 batch 匹配 | 单批待派 ≥ 50 且负载不均衡已是首要投诉 |

---

## 7. 与其他文档的关系

| 文档 | 关系 |
| --- | --- |
| `FOCUS.md §一` | 本文档的 scope 上级；本文档是 §一 的展开 |
| `PHASE_ROADMAP.md` | 长期规划；本文档只覆盖下一层（一期能落地的最小版本） |
| `dispatch-matching-best-practices.md`（`/Users/mac/dev_projects/cv/`） | 外部参考系；本文档是**取舍后的实现**，比参考系少 hybrid/Bandit/GraphRAG/LTR 四件套 |
| `EVALUATION_PLAN.md` | 指标定义权威；本文档 §4 只是漏斗视角的复述 |
| 现有派单轴（`eval/datasets/assignment.json` + `assignment_hard.json`） | 降级为"排序段单元回归"，README 明写不测端到端准确率 |

**验证节奏**：Task #2 → #3 → #4 → #6 → #5 → #7 → #8（`make check`）。
每一步都保持 `make check` 绿；不出现"改到一半"的状态。
