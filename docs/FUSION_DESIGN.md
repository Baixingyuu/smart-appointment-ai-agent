# helpdesk-agent — 融合设计

> **命名说明**：本文中的 `agent-desk` 指的是**上游参考仓库**
> （`github.com/huabeitech/agent-desk`，位于工作区 `../agent-desk`），
> 本项目名为 **`helpdesk-agent`**。文档写作时本项目尚未定名，
> 因此文中保留 `agent-desk` 作为「参考实现」的称呼——凡涉及
> 「参考实现怎么做/踩过什么坑」均指该仓库，而非本项目自身。

> 本文档定义如何以 `agent-desk` 为骨架、借鉴 `smart-appointment-ai-agent` 的能力，构建一个以**评测驱动**的客服 Agent 系统。
> 目标读者：项目作者本人（用于实施 + 面试准备）。
> 状态：**动工前的设计草案**（本文写作时尚未实施；如今主体已落地，因此全文都是历史快照）。
> 具体未实现项：ADR-1 的 `Trace`/`Step` 类型块只是 sketches——`Step.Type` 的取值 `skill`
> 随技能层于 2026-09-24 整体删除且从未上线；该块的 `ErrorKind` 取值名（`policy_denied` /
> `arg_too_large` / `need_confirm` / `exec_error`）与实际实现的 `tooling.ErrorKind`
> （`unknown_tool` / `not_allowed` / `budget_exceeded` / `args_too_large` / `invalid_args` /
> `needs_confirm` / `exec_failed`）不一致；`Surface` / `InterruptType` / `DecisionAction`
> 三个字段从未实现。现状以 `HANDOVER.md` §2（代码地图）与 §11（09-24 增补）、
> `EVALUATION.md` §4~§5 为准。

---

## 0. 结论摘要

- **骨架**：`agent-desk`（Go）。理由是它已有真实的多步 Agent Loop 与工具治理，这是四个简历点里最难自建的、且最需要工程可信度的部分。
- **代价**：必须从 ~15 万行砍到 **~2400 行 Go + ~800 行 Python**，砍掉 98%。本项目的工程量主要在"减法"。
- **不可避免的混合**：微调与 RAG 忠实度评测无 Go 生态，必须外挂 Python。采用**子进程 + JSON 契约**，不是双服务常驻。
- **主线**：以「评测」为第一公民，而非事后补的第四个点。评测同时是开发工具（驱动 A/B 对照）与简历产出（量化数字）。
- **顺序**：评测地基 → Loop 收敛 → Agentic RAG → 微调侧车。每期结束都有可写入简历的数字。

---

## 1. 目标与非目标

### 1.1 目标（对应四个简历点）

| # | 简历点 | 本项目的可交付物 | 可量化产出 |
|---|---|---|---|
| 1 | Agent loop 设计 | 自写 bounded loop + 统一 trace + 工具治理 | 工具选择准确率、平均步数、超限率 |
| 2 | Agentic RAG | 真正的检索决策循环（含证据门槛与改写重检索） | 检索召回、忠实度、平均检索轮次 |
| 3 | 微调（信息提取） | LoRA 抽取模型 + schema 强校验 | 字段 F1、schema 合规率、延迟与成本降幅 |
| 4 | Agent 系统评测 | 四轴评测框架 + 双基线 + 成本模型 | 一套可复现的对照实验结果 |

### 1.2 非目标（明确不做，写进 README 作为主动取舍）

- 不做多租户 / RBAC / OIDC / 组织管理
- 不做客服工作台前端、不做工单 CRUD 后台
- 不做可视化工作流编辑器（flowgram）
- 不做多渠道接入（企业微信 / Telegram / Discord / Zalo）
- 不做 Docker Compose 全家桶编排
- 不做多 Agent 之间的相互编排（演示规模下，Agent 间交接成本大于收益）

> 面试价值：以上每一条都能回答"为什么不做"。「我知道什么不该做」比「我什么都做了」更难被追问倒。

---

## 2. 基线盘点（决策依据）

### 2.1 `agent-desk` 实测规模

| 部分 | LOC | 处置 |
|---|---|---|
| `internal/ai` | 13,447 | 只保留 loop / tooling / rag 骨架 → **~1,800** |
| `internal/services` | 21,909 | 绝大多数是后台 CRUD → **~300** |
| `internal/models` | 1,157 | 保留 4 个模型 → **~150** |
| `internal/repositories` | 6,178 | 只保留 trace 落库 → **~200** |
| `internal/handlers` | 6,708 | 只保留 5 个端点 → **~200** |
| `internal/bootstrap` | 966 | 精简装配 → **~150** |
| `web/`（Next.js） | 59,961 | **全删** |
| `flowgram-editor/` | 12,624 | **全删** |
| 死代码 `runtime/tools` + `runtime/registry` | 1,122 | **全删** |

### 2.2 `agent-desk` 的真实缺陷（本项目要修的）

这些是实施时必须处理、也是面试可讲的判断依据：

1. **`internal/ai/runtime/tools/` 1122 行是死代码** —— 全仓无导入者，`NewRuntimeStaticTool` 只被自己的测试调用。
2. **`Answerability Gate` 名不副实** —— `evaluateAgentLoopResponsePolicy` 只判断检索上下文是否为空字符串，没有语义充分性判定。
3. **工具 token 记账不完整** —— `eino_agent_loop.go` 只取最后一次 model 调用的 `ResponseMeta.Usage`；多步工具调用下中间步的 token 未累加，成本被系统性低估。
4. **`writeToolCalls` 恒为 0** —— 评测只统计两个 `graph/` 写工具，而这两个恰好不在 `agentLoopSafeBuiltinCodes()` 里，模型无法调用。
5. **评测污染生产指标** —— 评测传 `Debug: true`，但 `RecordAgentLoopRun` 无 Debug 门，每次评测都往 `t_agent_run` 写行；而 `GetMetrics` 取"最近 5000 条 run"算 KPI。
6. **`requiresConfirmation` 断言过宽** —— 只检查 `summary.Interrupted`，不区分中断类型，workflow `human_confirm`、MCP `tool_confirmation` 都能让它通过。
7. **轨迹无步级结构** —— 所有工具调用挂在唯一一个 root step 上；派生 step 的 `DurationMS` 是整轮时长（假值）。

### 2.3 `smart-appointment-ai-agent` 实测规模与缺陷

仅 5,790 行 Python，但**没有 Agent Loop**：`agents/tools.py` 的 `@tool` 只是静态 schema 占位，全仓无 `AgentExecutor` / `bind_tools` / `create_react`，实际是硬编码路由。

可借鉴的：

- `eval/` 目录结构成熟：`metrics.py` + `run_*.py` + `datasets/*.json`
- `db/models.py` 的 `CustomerActivity` 事件表（`action_type` / `action_data` / `ticket_id` / `session_id`）——行为分析的干净底座
- `services/customer_service.py` 的规则健康度评分：**确定性算法算分 + LLM 只生成文案**，这个分工是对的，直接沿用
- `InputParser` 的抽取字段定义（`title` / `description` / `category` / `priority` / `engineer_name` / `info_complete` / `missing_info`）——即微调任务的 label schema

必须修的缺陷：

- `top_k_accuracy` **实现错误** —— 只做 `zip(matched, expected)` 逐元素比较，完全忽略 `k`，名字与行为不符
- 数据集过小：意图 40 条、抽取 20 条，**统计上无意义**（20 条上一个样本 = 5% 波动）
- **全仓无 token / 延迟计量** —— 而这两项正是四轴评测的两轴，必须新加
- 抽取靠 prompt 内写"只输出纯JSON"，`run_extraction_eval.py` 里要手写 `priority[:2]` 截断 LLM 自由输出 —— 脆弱性即微调任务的动机

---

## 3. 架构总览

### 3.1 分层（严格单向）

```
cmd/server            # 装配 + 路由
  ↓
internal/api          # 5 个 HTTP 端点，只做参数解析与响应
  ↓
internal/agent        # ★ 核心：bounded loop / 工具注册 / 工具治理 / trace
  ↓
internal/rag          # ★ 检索决策循环（agentic 部分）
  ↓
internal/ai           # LLM 客户端封装（OpenAI-compatible）
  ↓
internal/store        # 模型 + 落库（trace / 会话 / 工单）
  ↓
internal/eval         # ★ 四轴评测（消费 trace，不被业务依赖）
```

关键约束：**`internal/eval` 只读业务产出，业务层不依赖 eval**。评测是观察者，不是调用方。

### 3.2 进程与边界

```
┌──────────────────────────────────────────┐
│  Go 主进程                                │
│  server ── agent loop ── rag ── store    │
│              │                            │
│              │ exec.Command + JSON stdin/stdout
│              ▼                            │
│  ┌────────────────────────────────────┐  │
│  │ Python 侧车（按需拉起，非常驻）      │  │
│  │  train_extractor.py   LoRA 训练     │  │
│  │  infer_extractor.py   批量推理      │  │
│  │  rag_judge.py         忠实度评测    │  │
│  └────────────────────────────────────┘  │
└──────────────────────────────────────────┘
```

**为什么用子进程而不是常驻服务**：三者都是批处理/离线任务（训练、批量评测），无需长连接；子进程让契约必须是显式的 JSON schema，天然防止"隐式共享内存"式耦合；部署形态保持单进程主导。若将来推理需在线化，再把 `infer_extractor.py` 换成 FastAPI，契约不变。

---

## 4. 关键技术与决策

### ADR-1：Agent Loop 自写，不用框架编排器

**决策**：不引入 Eino（Go）或 LangChain AgentExecutor（Python），自己写约 250 行的 bounded loop。

**理由**：
- agent-desk 用 Eino 的代价已实测：token 记账不完整（缺陷 3）、trace 落库与 loop 内部状态脱节、评测断言拿不到 suppress 前的决策。框架把关键信号藏在内部。
- 简历点 1 是"loop 设计"，用框架等于放弃这个点。
- 自写 loop 让 trace 成为**一等输出**，评测直接消费，无需二次埋点。

**保留 agent-desk 做对了的部分**（这是骨架选择的核心价值）：

| 要素 | 来源 | 说明 |
|---|---|---|
| 工具分级 `read` / `write` | `internal/ai/tooling/registry.go` | write 必走确认 |
| 三重预算 | `agentLoopToolPolicy` | `maxTotalCalls` / `maxArgumentBytes` / `allowedRiskLevels` |
| 幂等边界 | `models.AgentToolInvocation` | `(ConversationID, ToolCode, IdempotencyKey)` 唯一索引 |
| trace 三表结构 | `AgentRun` / `AgentStep` / `AgentToolCall` | schema 直接沿用 |
| 确认中断与恢复 | `ConversationInterrupt` | 状态机 `pending/resolved/cancelled/expired` |

**Loop 契约**：

```
输入:  system_prompt, user_message, tools[], budgets
循环:  for step in 0..max_steps:
         决策 = LLM(system, messages, tools)        # 一次调用，不是 ReAct 多轮文本
         if 决策 是 终态:  写入 trace, 返回
         if 决策 是 工具调用:
            授权(工具分级 / 白名单 / 预算 / 参数大小)   ← 拒绝也要落 trace
            if 需要确认:  写 interrupt, 返回
            结果 = 执行(工具)
            写 trace(step, tool_code, status, duration_ms, args_preview, result_preview)
返回:  reply_text, trace_id, tokens{prompt, completion}, total_duration_ms
```

**修复缺陷 3**：每一步 model 调用的 `usage` 累加进 run 级 token，而非只取最后一次。

**统一 trace schema**（合并现有三表 + 新增字段）：

```go
type Trace struct {
    RunID        string
    Status       string   // completed | failed | interrupted
    Steps        []Step
    PromptTokens int      // ← 全步累加（修缺陷 3）
    CompletionTokens int
    DurationMS   int
    // 新增，用于评测
    DecisionAction string // ← suppress 前的原始决策
    InterruptType  string // ← 区分 ticket_creation_confirmation / human_confirm / tool_confirmation（修缺陷 6）
    Surface        string // ← 模型是用 tool_search 包装还是直连别名
}

type Step struct {
    Type      string // model | tool | skill | knowledge | policy | resume
    Code      string
    Status    string
    DurationMS int   // ← 真实耗时（修缺陷 7：不再用整轮时长充当步时长）
    ArgsPreview string
    ResultPreview string
    ErrorKind string  // policy_denied | budget_exceeded | arg_too_large | need_confirm | exec_error
}
```

### ADR-2：Agentic RAG 是自己写决策循环，不是调框架

**决策**：保留 `agent-desk` 的检索底座，**替换掉那个判空的伪 gate**，加入真正的充分性判定 + 查询改写重检索。

**参照 `agent-desk` 的检索参数**（`retrieve_context.go` / `knowledge_retriever.go`）：

```
TopK             默认 8
ScoreThreshold   默认 0.3
ContextMaxTokens 默认 4000
MaxContextItems  默认 5
```

**真正的 agentic 循环**（约 150 行）：

```
for round in 0..max_rounds(2):
    hits = retrieve(query)
    sufficient, reason = evidence_gate(hits)     # ← 核心：不是判空
    if sufficient: break
    query = rewrite(query, hits, reason)         # ← 用已有 hit 反推缺什么
if 始终不充分:
    降级策略 = fallback_mode (suggest_retry | handoff | 明确说明知识不足)
```

**`evidence_gate` 的判定维度**（这是与 agent-desk 的差异点，也是面试可讲的改进）：

| 维度 | 判据 | 可测性 |
|---|---|---|
| 召回存在性 | `len(hits) > 0` | agent-desk 只做到这里 |
| 置信充分性 | `top_score >= ScoreThreshold` | 新增 |
| 覆盖充分性 | 命中覆盖 query 的关键实体/术语比例 | 新增 |
| 冗余抑制 | 单文档 ≤2 条、同章节去重 | 移植 `documentUsage` 逻辑 |
| 兜底归因 | 记录 `reason`，区分 `no_hit` / `low_score` / `low_coverage` | 新增，评测可按原因分桶 |

**为什么这是个真改进**：`reason` 让评测能区分"知识库没覆盖"和"检索没召回"，这是两条完全不同的改进方向。agent-desk 的判空 gate 产生不了这个区分。

### ADR-3：微调走 Python 侧车

**决策**：Go 主进程通过子进程调用 Python 训练/推理。**Go 侧不引入任何 ML 依赖。**

**任务定义**（沿用 `smart-appointment` 的字段，修正为强 schema）：

```json
{
  "title":       "≤20字，必填",
  "description": "问题现象+影响范围，必填",
  "category":    "incident | consultation | request | change",     // 封闭枚举
  "priority":    "P0 | P1 | P2 | P3",                              // 封闭枚举
  "engineer_name": "string | null",
  "info_complete": "bool",
  "missing_info": ["description"]                                   // 封闭字段名
}
```

**关键设计：用 schema 校验替代 prompt 里写"只输出纯JSON"。** 现在的脆弱性（`priority[:2]` 手工截断）就是动机。Go 侧收到模型输出后**必须过 schema 校验**，违规即计入 `schema_violation` 指标——这个指标本身就是微调收益的证明。

**数据构造**（融合两项目的关键一步）：

| 来源 | 用途 |
|---|---|
| `agent-desk` 运行中的客服会话 | 输入分布（真实中文技术支持语料） |
| `agent-desk` 产出的工单（`Ticket`） | 弱标签初稿 |
| **人工修正后的工单** | 金标 |
| `smart-appointment` 抽取字段定义 | label schema |
| 合成 + 人工校对 | 补足长尾类别 |

> 规模要求：**≥400 条**。现有 20 条出任何准确率都没有意义（一个样本 = 5%）。
> 划分：train / dev / test = 6:2:2，**test 集冻结，只在最终报告用**。

**三向对照实验**（简历数字的来源）：

| 方案 | 字段 F1 | schema 合规率 | P50 延迟 | 单次成本 |
|---|---|---|---|---|
| A. prompt-only（现状） | 基线 | 基线（低） | 基线 | 基线 |
| B. 大模型 + schema 校验 | ↑ | ↑ | 不变 | 不变 |
| C. LoRA 微调 + schema 校验 | ↑↑ | ↑↑ | **↓ 显著** | **↓ 显著** |

成本用静态价目表换算（agent-desk 的 `AIConfig` 无价格字段，本项目补一张 `model_price` 表，按 `model_name` 计价）。

### ADR-4：行为分析作为独立只读模块

**决策**：沿用 `smart-appointment` 的正确分工——**确定性特征工程 + LLM 只做文案**。

**为什么不把行为分析交给 LLM**：健康度分数若由 LLM 生成则不可复现、不可回归测试、无法解释给业务方。规则算分 + LLM 组织语言，两层各自可测。

**特征层**（`CustomerActivity` 事件表 → 特征向量，约 150 行）：

| 特征 | 定义 | 用途 |
|---|---|---|
| 活动频次 | 近 30 天 `ticket_created` / `consultation` 计数 | 活跃度 |
| 活跃间隔 | `days_since_last_activity` | 流失预警 |
| 工单积压 | 未关闭工单数 / 平均滞留时长 | 满意度风险 |
| **意图漂移** | 近 30 天意图分布 vs 前 30 天的 JS 散度 | ★ 行为分析亮点 |
| 重复求助 | 同 `category` 在 7 天内重复次数 | 未真正解决 |
| 升级行为 | 咨询 → 工单的转化率 | 自助失败率 |

**意图漂移**是本项目独有的：因为 loop 每轮都落 `DecisionAction` 和路由意图进 trace，所以意图序列是**免费的副产品**。这是"四个点互相咬合"的体现，也是简历上能讲出深度的点。

**评测**：特征层用确定性单测（给定事件序列断言分数）；文案层用人工评分或 LLM-judge，二者分离报告。

### ADR-5：`services` 层只留业务不变量

**决策**：`agent-desk` 的 21,909 行 `internal/services` 里，只保留**业务不变量与事务边界**，删掉全部后台 CRUD 编排。

保留：
- `CreateTicket`（事务：工单 + 进度 + 事件）
- `ConversationInterrupt` 状态机
- 工具幂等声明（`Claim`）
- trace 落库

删除：权限、组织、排班、渠道 outbox、知识库目录树、资产、通知、看板聚合……（约 95%）

### 为什么骨架选 Go 是成立的（回应取舍）

选 Go 而非 Python，**不是因为 Go 在 ML 上合适，而是因为本项目的重心是"评测驱动的系统"而不是"模型本身"**：

- 评测需要**稳定的 trace schema、确定性的状态机、并发实验调度**——Go 的强类型与显式错误处理让 trace 契约不会悄悄漂移
- 微调只需**离线批处理**，Python 侧车完全够用，且天然隔离了 ML 依赖的版本地狱
- 若反过来以 Python 为骨架，Loop 与工具治理会用动态类型写，trace schema 容易腐化——而 trace 正是四个点的公共地基

**代价必须承认**：任何时候要改微调，都要跨越进程边界；本地开发需要同时装 Go 与 Python 环境。这是自觉接受的成本。

---

## 5. 目标代码结构

```
helpdesk-agent/                   ~2,400 行 Go
├── cmd/server/main.go            ~80
├── internal/
│   ├── api/                      ~200   5 个端点
│   │   ├── chat.go               POST /api/chat
│   │   ├── eval.go               POST /api/eval/run
│   │   ├── metrics.go            GET  /api/metrics
│   │   ├── trace.go              GET  /api/trace/:id
│   │   └── health.go             GET  /api/health
│   ├── agent/                    ~700   ★ 简历点 1
│   │   ├── loop.go               bounded loop
│   │   ├── tools.go              工具注册 + risk 分级
│   │   ├── guard.go              授权：白名单/预算/参数大小/确认
│   │   ├── interrupt.go          确认中断与恢复
│   │   └── trace.go              trace 记录（统一 schema）
│   ├── rag/                      ~300   ★ 简历点 2
│   │   ├── retrieve.go           FAISS/Qdrant 适配
│   │   ├── gate.go               证据充分性判定
│   │   └── rewrite.go            查询改写
│   ├── insight/                  ~180   行为分析
│   │   ├── features.go           确定性特征工程
│   │   └── narrate.go            LLM 文案
│   ├── ai/client.go              ~120   OpenAI-compatible 客户端
│   ├── store/                    ~400
│   │   ├── models.go             4 个模型 + model_price
│   │   └── trace_store.go
│   └── eval/                     ~400   ★ 简历点 4
│       ├── runner.go             调度
│       ├── metrics.go            四轴指标
│       ├── cost.go               成本换算
│       ├── baseline.go           规则基线（移植 agent-desk 关键词分类器）
│       └── report.go             JSON + CSV
└── sidecar/                      ~800 行 Python
    ├── train_extractor.py        LoRA 训练
    ├── infer_extractor.py        批量推理
    ├── rag_judge.py              忠实度评测
    └── contract.py               与 Go 共享的 JSON schema 定义

eval/datasets/
├── intent.json                   ≥200 条
├── extraction.json               ≥400 条（冻结 test 集）
├── rag_qa.json                   ≥100 条（带金标 chunk id）
└── trajectory.json               ≥50 条（带期望工具序列）
```

---

## 6. 四轴评测框架（简历点 4 的核心）

### 6.1 为什么评测要先做

四个点如果各自独立就是四个 demo。评测把它们串成一个故事：

```
评测暴露 prompt 抽取的脆弱性
  → 因此做微调替换
  → 用同一套评测量化收益
  → 同一套 trace 也评测 RAG 检索决策与 loop 工具调用质量
```

评测框架是唯一同时被四个点消费的组件。先建它，后面每一步都有即时反馈。

### 6.2 四轴定义

**轴 1：路由准确性**
- 指标：`Accuracy`、`Macro-F1`、**混淆矩阵按类别分桶**
- 双基线：LLM 分类器 vs **关键词规则基线**（移植 `agent-desk` 的 `analyze_conversation_graph.go` 中文关键词表）
- 价值：零 API 成本的确定性对照，"LLM 相比规则提升多少"是可讲的结论

**轴 2：抽取字段级质量**

| 指标 | 定义 | 注意 |
|---|---|---|
| 字段准确率 | 逐字段精确匹配 | 枚举字段（category/priority）用这个 |
| 字段 F1 | 逐字段 P/R/F1 | 自由文本字段（title）需要归一化或模糊匹配 |
| **schema 合规率** | 一次通过 schema 校验的比例 | ★ 微调收益的关键证据 |
| 缺失字段检出 | `missing_info` 的 P/R | 交互质量的代理指标 |

**轴 3：RAG 质量**
- 检索侧：`Recall@k`、`MRR`、**按 `gate` 的 reason 分桶**（`no_hit` / `low_score` / `low_coverage`）
- 生成侧：`Faithfulness`（答案是否被证据支撑）、`Answer Relevance`
- **必须修正** `smart-appointment` 的 `top_k_accuracy`：现实现忽略 `k`，改为真正的窗口命中判定
- 价值：`reason` 分桶能区分"知识库没覆盖"与"检索没召回"——这是改进方向的依据

**轴 4：执行轨迹 + 成本**

| 指标 | 定义 |
|---|---|
| 工具选择准确率 | 实际工具序列 vs 期望序列（有序/无序两种） |
| 工具冗余率 | 同 run 内重复调用同工具次数 |
| 平均步数 | 与期望步数的偏差 |
| 预算超限率 | 触发 `maxTotalCalls` / `maxArgumentBytes` 的比例 |
| 越权尝试率 | 尝试调用未授权工具的次数（应被 guard 拒绝并落 trace） |
| **token 成本** | prompt/completion 全步累加 × 价目表 |
| **端到端延迟** | P50 / P95，且区分 model 耗时 vs 工具耗时 |

### 6.3 基线设计（保证结论可信）

| 对照组 | 含义 | 成本 |
|---|---|---|
| B0 规则基线 | 关键词分类 + 模板抽取 | 0 |
| B1 prompt-only | 现状实现 | API |
| B2 prompt + schema 校验 | 只加校验 | API |
| B3 LoRA 微调 | 完整方案 | API + 训练 |

只有 B0 与 B1 的差距能证明"LLM 有用"，B1/B2 与 B3 的差距能证明"微调有用"。**缺任何一组，数字都不可信。**

### 6.4 报告与可复现性

- 输出 `JSON`（机读，供回归对比）+ `CSV`（人读，`agent-desk` 已有此格式可沿用）
- 每次运行记录：数据集 hash、模型名、prompt 版本、代码 commit
- **一次运行一条 `eval_run` 记录**，与业务 trace 表物理隔离——修掉 agent-desk 的缺陷 5（评测污染生产 KPI）
- 支持 `--baseline <run_id>` 做回归对比，输出指标 delta

---

## 7. 实施里程碑

> ⚠️ **本节已被 `PHASE_ROADMAP.md` 取代。** 下文 M0–M6 是最初的试验性排期；分期口径（一期＝工单落地场景 / 二期＝多步决策 / 三期＝学习与闭环）以 `PHASE_ROADMAP.md` 为准，此处仅作历史记录保留。

每期结束都有可写入简历的产出。**严格按序**，因为顺序反了要返工。

### M0：清理与搭建（0.5 周）
- 新建项目骨架，从 `agent-desk` 只搬运 `internal/ai/tooling` 的设计（不搬代码）
- 删除决策落地：不复制 `web/`、`flowgram-editor/`、死代码
- 产出：可编译的空壳 + `go test ./...` 通过

### M1：评测地基（1 周）★ 先做
- `internal/eval/` 四轴指标 + 成本模型 + 报告
- 移植规则基线（关键词分类器 → Go）
- 从 `smart-appointment` 迁移并**扩容**数据集（意图 ≥200、抽取 ≥400）
- 修正 `top_k_accuracy`
- 产出：**基线数字**（B0 规则 / B1 prompt-only），这是后续所有对比的锚

### M2：Agent Loop（1.5 周）★ 简历点 1
- 自写 bounded loop + 工具治理 + trace
- 修掉 token 累加、步级耗时、决策记录
- 产出：**工具选择准确率、平均步数、token 成本、P50/P95 延迟**

### M3：Agentic RAG（1 周）★ 简历点 2
- 替换判空 gate 为真充分性判定 + 改写重检索
- 产出：**召回率、`gate` reason 分布、平均检索轮次、忠实度**

### M4：行为分析（0.5 周）
- 特征工程 + 意图漂移 + LLM 文案
- 产出：**特征层单测覆盖 + 意图漂移案例**

### M5：微调侧车（2 周）★ 简历点 3，最重
- 构造 ≥400 条金标数据集（含 agent-desk 真实语料 + 人工修正）
- LoRA 训练 + 推理 + schema 校验
- 产出：**三向对照表（B1/B2/B3）**，字段 F1 + schema 合规率 + 延迟 + 成本

### M6：收口（0.5 周）
- README 写清架构取舍（第 1.2 节内容）
- 全套评测跑一遍，产出最终报告

**总计约 7 周**（按兼职投入估）。M1 和 M5 是风险最高的两期。

---

## 8. 简历映射（含反问与反例）

| 简历条目 | 建议表述 | 面试官会问 | 你的答案 |
|---|---|---|---|
| Agent loop 设计 | "自研 bounded agent loop，含工具分级授权、三重预算与幂等写入边界；支持确认中断与恢复" | 为什么不用 LangChain/Eino？ | 框架隐藏 token 记账与中间状态，导致评测拿不到关键信号；自研后 trace 成为一等输出 |
| Agentic RAG | "实现检索决策循环：证据充分性判定 + 查询改写重检索，按失败原因分桶归因" | 这和普通 RAG 有什么区别？ | 普通 RAG 是单轮检索拼 prompt；这里是"判断是否够用→不够则改写→仍不够则降级"，且能区分"知识库没覆盖"和"检索没召回" |
| 微调（信息提取） | "**用 LoRA 微调替代 prompt 抽取**，字段 F1 从 X→Y，schema 合规率 A→B，P50 延迟降 C 倍，单次成本降 D%" | 为什么不直接 prompt？ | 三向对照数据：prompt-only vs prompt+schema vs 微调。prompt 方案要靠 `priority[:2]` 手工截断输出，微调后 schema 一次通过率显著提升且可本地部署降本 |
| Agent 系统评测 | "搭建四轴评测框架（路由/抽取/RAG/轨迹），含规则基线与成本模型，支持跨版本回归对比" | 数据集多大？怎么保证不乐观偏差？ | 抽取 ≥400 条、意图 ≥200 条，test 集冻结且只在最终报告使用；四组对照（规则/prompt/prompt+schema/微调）隔离各因素贡献 |

**反例（不要这样写）**：
- ❌ "使用 LangChain 构建多 Agent 协作系统" —— 无差异化，且你没用
- ❌ "实现了 RAG 知识库问答" —— 人人都有
- ❌ "微调大模型提升效果" —— 无数字，必被追问"提升多少、成本多少、为什么不 prompt"

---

## 9. 风险与回滚触发条件

| 风险 | 触发条件（量化） | 回滚动作 |
|---|---|---|
| Go 骨架改造成本超预期 | M2 结束时 `internal/agent` > 1200 行仍未跑通 | 缩小范围：去掉中断恢复，只保留单轮 loop |
| 微调无收益 | M5 中 B3 相对 B2 的字段 F1 提升 < 3pt | 改为只报"schema 校验 + 小模型蒸馏"的结论，不写微调；**不伪造数字** |
| 数据集标注量不足 | M5 启动时金标 < 300 条 | 缩减任务范围到 4 个字段（去掉 `missing_info` / `engineer_name`），并明确标注"样本量限制" |
| 评测与业务耦合 | 任何业务代码 import `internal/eval` | 立即解耦，eval 只读 |
| Python 侧车环境地狱 | 本地无法在 30 分钟内重建训练环境 | 改用 API 微调（如 OpenAI fine-tuning），放弃本地 LoRA |

**红线**：任何情况下不编造指标数字。样本量不足就写样本量不足，这是面试里唯一会被真正否定的事。

---

## 10. 待决问题

1. **RAG 向量库选型**：沿用 `agent-desk` 的 Qdrant，还是回到 `smart-appointment` 的 FAISS？
   - Qdrant：工程化更好，需 Docker 常驻（与"简洁"有张力）
   - FAISS：零部署，但 Go 绑定不成熟，可能需侧车
2. **微调基座模型选型**：中文技术支持场景，候选 Qwen 系列 / Llama 系列；影响显存与训练时长。
3. **行为分析是否独立端点**：作为 `/api/metrics` 的一部分，还是独立 `/api/insight`？
4. **评测数据集来源合规**：`agent-desk` 的会话数据若来自真实业务，需脱敏；建议全部用种子语料 + 合成。
