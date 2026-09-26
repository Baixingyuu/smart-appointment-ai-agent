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
这样才能力调（改权重看整体效果）而不是每段各调各的。硬约束过滤（不在职、满载、P0 非 senior）
先于打分：不该参与的人不进 `Candidates` 打分，只进明细并带过滤原因。

Stage 2 只在 Stage 1 判"弱"时启用。**判弱不是打分的连续值，是三类离散原因**（见 §1.5），
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
- 最高分是否 ≥ `ServiceMatchFloor`（默认 0.25，实测依据见 `pipeline.go` 的 `DefaultPipelineConfig`）

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

**目的**：把根本不该参与的候选（不在职 / 满载 / P0 未达 senior 门槛）先踢掉，不进入打分。

**基本判据**（`eligibility.go`）：`!emp.Active → filtered`（不在职）、`!emp.HasCapacity() → filtered`
（未完成工单数已达 `MaxConcurrent`）。负载刻意不采信 `Employee.CurrentLoad` 那个静态快照：
候选由 `ticket.Service` 在每次派单时用 `FindOpenTicketsByAssignee` 重算，否则满载的人会被一直派中。
新增：
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
| Availability | `s_avail` | 0.20 | `1 - loadRatio` | 实时未完成工单数 |
| Seniority | `s_senior` | 0.10 | P0/P1 时 junior=0，其他情况=0.5；senior=1.0 | Level 新字段 |
| Recency | `s_recent` | 0.10 | Employee.Recency | 现有字段 |

**技能特征已整体删除**（2026-09-24）。原计划是"`s_skill` 权重归零但保留 Jaccard 接口"，
配 `Employee.Skills` / `Ticket.RequiredSkill` 与 120+30 条技能数据集一起留着。实际后果是
一条**看起来在工作、权重却是 0.00** 的特征，加上一个只被旧数据集引用、没有任何端到端指标
依赖它的实体层——保留它的唯一效果是让人误以为"技能匹配还在被评测"。
因此实体、打分特征、数据集、派单器一并删除，派单的第一因只有一个：**谁负责这个业务**。

**明细结构**：`domain.CandidateScore` 每项带 `OwnScore / SimScore / AvailScore /
SeniorityScore / RecentScore / Total` 与 `Filtered / FilterReason`，
`Total = Σ weight_i × score_i`，即 `0.4·own + 0.2·sim + 0.2·avail + 0.1·senior + 0.1·recent`。

**确定性 tiebreak**：总分 → 负载 → 响应 → ID 升序，总分比较用 epsilon 容差
（`1/3` 之类权重在二进制下无法精确表示，`==` 会把"实际相同"误判成"更优"）。
换打分器不能丢掉这个性质，否则同一批数据两次跑出不同指派，评测就无从复现。

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

**margin 阈值 0.15 怎么来的**：**目前是拍的**。校准需要"人类认可这个派单"的标注数据，
而人标环节尚未落地（生成空白标注表与一致率比对脚本曾存在，随技能层一起删除——它们的列
全部围绕技能编号）。当前只有 45 条规则自标注的 v2 样本，用它画"margin vs 派错率"曲线
等于把规则金标当人类判断，校准不出来只会被自己印证。在拿到人标数据前，把它当配置项暴露，
评测里对每个阈值单独跑一遍。这条不假装是"科学设定"，写清楚。

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
| **端到端 Top-1 准确率** | 排序轴 `winner == expectedAssigneeId`；**有效分母 40 而非 45**：gold outcome 为兜底池的样本没有期望处理人 | 见 §4.1 实测 |
| **成本 / 单** | Stage 2 调用数 × 平均 token × 单价 / total | 目标 < 0.02 元 |

**300 单日单量成本预估**（qwen3:8b 走本地 ollama 时近零；若接外部服务，按 0.006 元/1k token × 平均 3k token/次）：
- Stage 1 全 CPU、无外部调用 → 0
- Stage 2 15% × 300 = 45 次 × 0.018 元 ≈ 0.8 元/日
- Stage 3 5% × 300 = 15 次人审，按 3 分钟/单 ≈ 45 分钟人力/日

**这个成本模型必须写在这里**：Stage 2 值不值这个钱，取决于人工时薪与业务复杂度。
一期用 qwen3:8b（本地免费）能验证正确性；换成外部服务的成本要重新算。

### 4.1 实测（2026-09-24，`make eval` 离线 45 条）

| 轴 | 结果 | 有效分母 |
| --- | --- | --- |
| 抽取轴 service top-1 | 42/45 = 93.33% | 45（每条都有期望服务或明确的"无匹配"） |
| 判弱轴 原因一致 | 38/45 = 84.44% | 45 |
| 排序轴 winner 一致 | 34/40 = 85.00% | **40**，不是 45 |
| 漏斗 | stage1=35 / stage2=0 / stage3=10 | 45 |

三条口径限制，不能被一个总通过率抹平：

1. **Stage 2 未参与**：离线评测刻意不注入 chooser（派单里多一次 LLM 调用会污染轮次、延迟、
   token 三个轴）。所以 `stage2=0` 是装配选择，不是模型能力上限；`stage3=10`（22.2%）
   等于"判弱样本全部直落人工"，与 §4 表里 ≤10% 的目标**不同框**，不能据此判不达标。
   接线已经留好：`eval-assign-v2 -offline-stage2=false` 配上模型即可跑通 Stage 2。
   **Stage 2 命中率与幻觉率目前是"未测"，不是 0%**——这两个空档只能由真实模型跑出来。
2. **金标是规则自标注**：45 条 gold 由同一套服务字典与 ownership 规则生成，
   所以这组数字只证明"实现 == 我们宣称的规则"，不证明外部派单准确率。
3. **剩余 8 条不一致的主因是金标过期**：§1.4 记录的 `ownershipScore` 重标定只改了实现，
   gold 是标定前打的——4 条 `exp=none got=low_margin` 正是这个落差的直接后果，
   它们连带被判 0 分排序（`got=0` 因为离线无 Stage 2 接手）。
   另有 3 条抽取不一致（dp-v2-036 期望无匹配却召回 2010、dp-v2-041 期望 2005 得 2014、
   dp-v2-038 期望 2007 得 2003）属 BM25 过度召回与真实歧义两类。
   **刻意没有重打 gold 来把分数抬上去**：那等于用实现去定义正确答案，
   回归测试会立刻变成自证。当前基线由 `internal/eval/pipeline_v2_test.go`
   以下限形式锁定（42/45、38/45、34/40、stage1≥35、stage2==0），退化会红，
   改进需要显式改基线。

---

## 5. 代码集成点（终态）

```
internal/domain/service.go             // Service / Team / EmployeeExtension / Level
internal/assign/pipeline.go            // 三段编排 + Decision/Path/WeaknessReason 契约
internal/assign/resolve.go             // Stage 1.1
internal/assign/candidates.go          // Stage 1.2（当前返回空，接口先立）
internal/assign/eligibility.go         // Stage 1.3
internal/assign/score.go               // Stage 1.4
internal/assign/weakness.go            // Stage 1.5
internal/assign/llm_chooser.go         // Stage 2 LLM 版本
internal/assign/scripted_chooser.go    // Stage 2 假实现（评测/单测用）
internal/seed/service_catalog.go       // 14 服务节点 + 员工 ownership 映射
internal/ticket/dispatch.go            // DirectoryProvider 抽象 + runAssignment（落 AssignmentLog）
```

**通过 `rag.Retriever` 与 `llm.ChatModel` 接口调用，不改这两个包内部**——流水线对它们的
要求只有"能检索""能带 tools 对话"。

**从旧派单器继承的三件确定性资产**（`assigner.go` 已删除，这三件在 pipeline 内重实现）：
tiebreak 顺序（总分→负载→响应→ID 升序）、`nearlyEqual` 的 epsilon 容差、
"被过滤者也要出现在 `Candidates` 明细里并带 `FilterReason`"。
这三条是"同一批数据能复现同一指派"和"为什么没选他可复核"的地基，换打分器不能丢。

### 5.1 装配点与 Stage 2 开关

`ticket.New(st, pipeline, opts...)` 只接受 `*assign.Pipeline`，**pipeline 为 nil 时 panic**：
这里已没有可退回的"默认派单器"，静默兜底会造出"以为在跑流水线其实没跑"这类最难诊断的问题。

生产装配在 `cmd/helpdesk-agent/dispatch.go`：

```
newTicketService(st, chatModel)     serve / chat / demo 入口；chatModel 可为 nil
newDispatchService(st, chooser)     评测命令逐用例装配，不打 banner
```

Stage 2 是否启用只由"有没有模型"决定：

```
-api-key / LLM_API_KEY  有模型 → OpenAIChooser（enum 约束 top-5 + ESCALATE_HUMAN）
无模型（离线 / demo）    不注入 chooser → 判弱样本直落 Stage 3 人工池，而非退回 Stage 1 强派
```

`newTicketService` 启动时把 Stage 2 的实际状态打到 stderr。两种模式的派单结果差异很大，
不打印就只能事后猜。离线评测（`eval-trajectory` / `eval-realtickets`）刻意不注入 chooser：
派单里多一次 LLM 调用会污染轮次、延迟、token 三个轴，派单质量由 `eval-assign-v2` 单独测。

`AssignmentLog.Reason` 无条件带 `[path=...|weakness=...]` 前缀（`runAssignment` 添加），
所以派单历史里能直接 grep 出走的是哪一段。一期默认仍按此形态跑：`ServiceMatchFloor` 0.25
是在 14 条服务字典上按 BM25 分数分布定的，样本量决定了它需要随字典增长复标。

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
| 派单轴（`eval/datasets/assignment_v2.json` 45 条） | 唯一的派单评测：三轴 + 漏斗；`internal/eval/pipeline_v2_test.go` 以下限形式锁住 §4.1 的数字，退化即红 |

**验证节奏**：`make check`（fmt + vet + test）保证实现没有偏离宣称的规则；
`make eval`（`eval-assign-v2`）出三轴与漏斗。派单后端只有流水线一条路径，
所以"测试绿"与"线上跑的是同一套打分"这两件事现在是同一件事。
