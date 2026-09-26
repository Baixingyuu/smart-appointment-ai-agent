> **历史快照，非维护文档**（本文是早期一次会话的收尾总结，写成于评测文档还很薄的阶段）。
> 三处已不再成立：① `docs/EVALUATION.md` 早已远超 274 行（现 599 行）；
> ② 下方「当前缺口」表前两行（检索轴 Recall@k、路由/意图分类轴）已交付，
> 对应 `make eval-retrieval` / `make eval-intent`；
> ③ "补齐新轴不需要改动 `eval.Runner`，只需加数据集和断言"**是错的**——
> 派单 v2 轴新增了 `internal/eval/pipeline_v2.go` 与其报告结构；端到端交互轴至今仍被
> 抽象卡住，因为 `evalrun.AgentRunner.RunTurn` 只返回 `eval.TurnObservation`，
> 喂不进 `TicketSnapshot + AssignmentLog`（见 `FOCUS.md §三`、`HANDOVER.md §10.4`）。
> 现状以 `HANDOVER.md §11`（2026-09-24）为准。

---

文档已落地：`docs/EVALUATION.md`（274 行），提交 `2a382b6`。

## 关于你的 RAG 项目

我看了 `MODULAR-RAG-MCP-SERVER`，你的评测体系已经很完整：

| 你的实现 | 指标 |
|---|---|
| `CustomEvaluator` | `hit_rate`、`mrr`（确定性，无需 LLM） |
| `RagasEvaluator` | `faithfulness`、`answer_relevancy`、`context_precision` |
| 还有 | `composite_evaluator`、`eval_runner`、观测面板 |

**所以我没有在 agent 框架里重复实现检索与生成质量**，而是在文档第 6 节明确写了衔接方式：agent 侧只消费 `rag.Reason` 的归因做分桶，文本层指标复用你的实现。这样避免两套评测各说各话。

## 文档的核心论点

我刻意把第 1 节写成**「为什么 agent 评测与 RAG 评测不是一回事」**，因为这是整份文档的立论：

> RAG 只有一种错法（答得不对），agent 有四类各自独立的错法：
> **该做没做** / **不该做却做了** / **做对了但绕远** / **做对了但顺序错**
>
> 这四类失败的改进方向完全不同，因此必须分轴统计。混成一个「准确率」，就只能知道「变差了」，无法知道「该改哪里」。

## 指标三层结构

```
第一层 汇总指标    Top1Accuracy / PassRate / ToolAccuracy / SafetyViolationRate / AvgRounds / Latency...
     ↓
第二层 归因指标    rag.Reason（6 种）/ tooling.ErrorKind（7 种）/ 逐轮 token
     ↓
第三层 明细记录    ToolCallRecord / AssignmentLog（含候选明细）/ TicketProgress
```

设计原则是**每一层都要能回答「下一步该改什么」**。

## 我认为最实用的部分：第 9 节

一张「指标 ↔ 失败模式」对照表，排查时直接查：

| 现象 | 查哪个指标 | 根因方向 |
|---|---|---|
| 知识库能答却建了单 | `SafetyViolationRate` | 置信门槛过低 |
| 出现重复工单 | `OrderAccuracy` | 模型跳过查重 |
| 成本突然翻倍 | 逐轮 `RoundRecords` | 反复检索 → 检索质量退化 |
| 声称做了实际没做 | `ToolAccuracy` 正常但 `PassRate` 低 | **模型在回复里假装调用工具** |
| 检索似乎失效 | `rag.Reason` 分桶 | `no_hit` 补知识 vs `low_score` 改检索 |

## 核对中发现并修正的一个错误

文档里 `extra_rounds` 的 AvgRounds 我写了 **4.56**，实测是 **5.56**。原因是我把 `MaxToolRounds` 从 3 调到 5 之后，这个 sabotage 能跑更多轮了，而我沿用了旧数字。

顺带补了一句说明：**这类指标的含义依赖配置**，报告时必须同时记录配置——否则跨版本对比会得出错误结论。这也是文档里所有数字都要能由命令复现的原因。

## 关于 RAG 复用的一个判断

你说"可以直接复用或借鉴"——我的结论是**借鉴评测方法论，不复用代码**。理由：

- 你的 RAG 评测是 Python，agent 是 Go，跨语言复用的成本高于各写一遍
- 真正值得借鉴的是**指标口径**（`hit_rate`/`mrr` 的确定性设计、Ragas 的三项该放在哪一层），这些我已经引用进文档
- 二期补「检索轴独立报告」时，数据契约对齐你的格式即可，实现各写各的

## 当前缺口（文档第 10 节）

| 缺口 | 期次 |
|---|---|
| 检索轴独立报告（Recall@k） | 二期 |
| 路由准确性（意图分类） | 二期 |
| 抽取字段级 F1 | 三期 |
| 忠实度（复用你的 Ragas 实现） | 三期 |
| 意图漂移（JS 散度） | 三期 |

补齐前两轴后四轴变六轴，而 `eval.Runner` 接口与分轴报告结构**不需要改动**，只需加数据集和断言。

---

顺带说明：**HTTP 服务还在跑**（`localhost:8080`，真实模型 + BM25），你可以继续体验。需要我停掉，还是继续做二期（bounded loop + 意图分类层 + 检索轴报告）？