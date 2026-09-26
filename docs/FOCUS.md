# 三条主线（Focus）

> 定稿于 2026-09-23。本文档只锁 **scope 与推进顺序**，不重复方法论。
> 优先级：scope 层以本文件为准；指标定义与解读边界仍以 `EVALUATION_PLAN.md` 为准；
> 分期规划的长期方向以 `PHASE_ROADMAP.md` 为准。三者冲突时先看是不是把 scope 和 roadmap 混了。

## 0. 为什么需要这份文档

`PHASE_ROADMAP.md` 已经把一/二/三期规划到很细，但一旦开新对话就容易被"业界成熟做法"
拉着走（多路 hybrid、cross-encoder rerank、Bandit、GraphRAG、LTR、置信度校准……）。
**本项目的立论不是"用最强的技术"，是"能度量 + 能归因"**——所以架构规模必须服务于此。

三条主线**互不重叠**：
1. **派单**：工单落成之后怎么给人
2. **建单交互**：从用户开口到工单落库
3. **评测**：把前两条接进可回归的度量里

任何一条上出现的第四种东西，先问"能不能推到 §六 的触发条件表里"。

---

## 一、主线 1：派单（工单 → 员工）

> **执行设计已落到 `DISPATCH_PIPELINE.md`（2026-09-24）**：三段流水线（Stage 1 规则+检索 / Stage 2 LLM+enum 契约 / Stage 3 人工）
> 的接口、阈值与集成点都在那里。本节只讲 scope 决策；具体架构与代码组织以 `DISPATCH_PIPELINE.md` 为准。

### 唯一要先决定的

**中间显式建模"服务节点"这一层**：
- **B 主**（工单 → 服务节点 → owner）—— CMDB 天然存在，可解释、可审计
- **C 兜**（工单 → 相似历史工单 → resolver）—— 归属未命中时用
- **不做 A**（工单文本 → 直接 embedding 打员工）—— 端到端一旦失败没法定位是哪段错

**这不是效率选择是评测选择**：只有 B/C 这种带中间产物的架构，每一段才能独立加轴。

### 现状与差距（2026-09-24 更新，见 `DISPATCH_PIPELINE.md`）

| 项 | 一期落地 | 与原计划的偏差 |
| --- | --- | --- |
| `domain.Service / Team / OwnershipKind / Level` | ✅ `domain/service.go` | — |
| `Employee.TeamID / Level / Profile / OwnedServices` | ✅ 独立记录 `EmployeeExtension` | **不改 Employee 结构**：组织属性（级别/归属/画像）来自人员目录，负载（在职/并发/响应）每次派单由工单表实时重算，两者写入方与更新节奏都不同；混进一个结构就说不清该由谁负责刷新 |
| `Ticket.SuspectedServiceID` | ❌ 未加字段 | resolve 输出只活在 `Decision.Services` 单次调用中，不进 domain。理由：服务解析是流水线中间产物，不是工单属性 |
| 归属解析（工单 → 服务节点） | ✅ `assign/resolve.go` + BM25 实现 | — |
| 相似历史工单 kNN | ⚠️ 接口 + Noop + Static 桩 | **实现留空**：造出来的假历史会让 s_sim 段过拟合到造出来的分布；有真实工单再灌索引 |
| 派单打分器 | ✅ `assign/score.go` 五特征加权 | **原计划的"Assigner 不删、降级为排序段回归目标"已被推翻**：零权重特征 + 只被旧数据集引用的实体层，效果是让人误以为技能匹配还在被评测。2026-09-24 连实体、特征、120+30 数据集、旧派单器一起删除，pipeline 是唯一派单后端 |
| `AssignmentLog.Path` | ⚠️ 落在 `Decision.Path` 而不是 `AssignmentLog.Path` | 保持 domain 稳定；观测 pipeline 走的是哪一段读 Decision 或未来另建 `pipeline_run` 表 |
| Stage 2 LLM + enum 契约 | ✅ `llm_chooser.go` + `ScriptedChooser` | — |
| Stage 3 人工兜底 | ✅ 通过 `OutcomeFallbackPool + Path{stage3_human,stage3_blocked,stage2_invalid}` 三种入口 | — |
| Sabotage 三件套 | ✅ `WithoutOwnershipBoost / WithoutLLMEnumGuard / MarginThreshold=0` | — |

### 评测怎么接（v2 数据集）

`eval/datasets/assignment_v2.json` 已落地 45 条，与当初的设想有四处不同，都记在这里：

| 设想 | 实际 | 影响 |
| --- | --- | --- |
| `title` + 200-500 字 `body` | `title` + `description`，中位 29 字、最长 84 字 | BM25 可用的信号比设想少一截，判弱/召回类失败与这个直接相关；补长描述是廉价改进，但会同时改变金标难度 |
| 员工 `profile` 文本 + `systems_owned / tech_stack / recent_30d` | `EmployeeExtension.Profile` 文本 + `OwnedServices` 结构化；无 `recent_30d` | Stage 2 喂给模型的画像因此比设想薄 |
| 金标**只有** `expectedAssigneeId` + 人写 `rationale_by_human` | 金标是四元组 `serviceTop1 / weakness / outcome / assigneeId`，`goldSource = formula-derived` | **没有人写理由**。三轴数字的口径是"实现 == 宣称的规则"，不是准确率 |
| 不预填 `requiredSkillIds` / `skillIds` | ✅ 成立，且技能实体层已整体删除 | 派单的第一因只有服务归属 |

派单轴同样按设想/实际拆开看，别把没做的算进成绩：

| 段 | 指标 | 状态 | 实测（2026-09-24 离线） |
| --- | --- | --- | --- |
| 服务解析 | Service-ID Top-1 | ✅ 抽取轴 | 42/45 = 93.33% |
| 判弱 | 原因类别一致 | ✅ 判弱轴 | 38/45 = 84.44% |
| 排序 | winner 一致 | ✅ 排序轴（有效分母 40） | 34/40 = 85.00% |
| Stage 漏斗 | stage1/2/3 分布 | ✅ | 35 / 0 / 45 |
| kNN resolver | Top-K 命中真正解决过的人 | ❌ Noop 桩 | 无历史语料，故意不测 |
| 端到端（相对**人标**） | Top-1 / @3 / 兜底率 | ❌ 未做 | 没有人标数据，做不了 |

**旧 120+30 技能数据集与 legacy Assigner 已删除**（2026-09-24）。原先"不删、降级为排序段
单元回归"的方案被推翻：一个零权重特征加一套只被自己引用的实体，唯一效果是让读者以为
技能匹配还在受测。数字口径见 `DISPATCH_PIPELINE.md §4.1` 与 README「评测设计」。

---

## 二、主线 2：Agent 与用户交互完成建单

> **执行设计已落到 `TICKET_INTAKE.md`（2026-09-24，已实现）**：进展驱动追问（不设轮次上限，靠"同一字段问一次仍空即放弃"+ 安全阀终止）、
> 可追溯性判定（`TraceabilityChecker`，忠实度只标注不拦单）、forceCommit 主动建单出口三支柱，行为与回归覆盖在 `internal/agent/intake_test.go`。
> 本节只讲 scope 决策；具体架构与代码组织以 `TICKET_INTAKE.md` 为准。
> **仍待办**：专门的评测轴命令 `make eval-e2e`（样本 + 追问放弃率 / 忠实度检出率指标）——逻辑已被测，但样本驱动的 runner 尚未建。

### 决定：终止靠"字段进展"，不靠轮次计数

早期草稿设想一个"第 1/2/3 轮"硬上限：
```
第 1 轮：缺哪些一次列全 → 一并问（不逐字段挤牙膏）
第 2 轮：仍缺 → 只追问影响后续处理的那 1-2 个字段
第 3 轮：仍缺 → 直接建单 + MissingInfo
```
**这个"数总轮次"的思路已被否掉**（见 `TICKET_INTAKE.md §2`）：真实用户补全的速度不一，按会话总轮次设上限会误伤"每轮都在填新字段"的正常会话。落地规则改为**进展驱动**：

- 数的是"同一字段第几次仍为空"——某字段问过一次、用户补了仍空 → 该字段不再追问，建单 + 记 `MissingInfo`。
- 不设轮次上限；只要用户每轮都在填某个空槽就一直问。
- 只保留一个 `>10` 轮安全阀，**防判空逻辑出 bug 导致无限追问**（工程兜底，非业务规则）。

公理不变：宁可建"信息不全的工单"，也不冒"用户跑了没建单"的风险。

### 现状与差距

| 项 | 现状 | 差距 |
| --- | --- | --- |
| 三个模型侧工具 + 治理四道闸 | ✅ | — |
| 确认中断-恢复状态机 | ✅ 已修 D1/D2/D3 | 见 `HANDOVER §10` |
| 意图分类短路 | ✅ | — |
| 跨轮历史窗口（6 条 / 900 rune） | ✅ 已修 D1 | 轨迹轴 runner 仍看不到，见 §三 |
| MissingInfo 机制 | ✅ | — |
| **追问（进展驱动）** | ✅ `intake.go`：阻塞槽位缺→一次列全追问；同字段问一次仍空→放弃建单 + MissingInfo；`>10` 安全阀 | 逻辑已测（`intake_test.go`）；`make eval-e2e` runner 待建 |
| **起草忠实度**（模型编内容） | ✅ `TraceabilityChecker`：描述里追溯不到的声明→ `MissingInfo += fidelity_unverified` 并在确认文案点名，**只标注不拦单** | 假阳率待 `>10` 抽检标定（见 `TICKET_INTAKE §10`） |
| 跨会话上下文（同用户偏好） | ❌ | 短期不做，无数据 |

### 评测怎么接（端到端交互轴）

新轴 `make eval-e2e`：
```
输入：一段用户消息脚本（含追问、干扰、语义不明、换需求）
判据：
  · 落库工单 (title, category, priority, missing_info, assigneeId)
  · 中间回合 (interrupts[], promptTokens, rounds, path)
```
**runner 必须经 `conversation.Service.Send`**，不走 `ag.Run`——
这样才能顺手把 §三 里"轨迹轴看不到跨轮历史"的结构盲区一起修掉。

---

## 三、主线 3：评测（把前两条串起来）

### 唯一要先决定的

**评测先行，不是功能先行**。理由：
- 派单轴已按 v2 重跑（42/45、38/45、34/40 + 漏斗 35/0/10），**依然不能对外当准确率讲**，
  但原因换了：不再是"100% 太假"，而是金标 `formula-derived`——它测的是实现对规则的实现率
- 建单链路的三条 P0 缺陷（D1/D2/D3）都是人肉开 `chat-local` REPL 才发现的，
  说明**现有轴有大片盲区**
- 起草忠实度缺陷已知存在，规则版断言已实现（`TraceabilityChecker`），
  但**没有比率型指标轴**，所以"检出率多少"仍然排不出优先级

**评测先行不是追求完美，是没有反馈回路时后面每一刀都在盲切。**

### 现状与差距

框架层面基本够用（五轴 + 缺陷注入 + scripted/live 双模 + 变异实验），
**但"独立金标"这一条已经不成立**：v1 时代那份用 `Fraction` 精确复算、与生成脚本不共用代码的
交叉校验（`verify_datasets.py`）随技能层删除了，现在派单轴只有一套实现自己标的金标
+ `pipeline_v2_test.go` 的三条弱一点的防线。

三条新轴：**一条已交付、一条做了一半、一条没动**。

| 新轴 | 覆盖 | 修的是哪个盲区 | 状态 |
| --- | --- | --- | --- |
| **端到端交互轴** | 用户脚本 → 落库工单 | 轨迹轴看不到跨轮历史（D1 结构盲区） | ❌ 未做：8 条用例是 `go test`，runner 仍走 `ag.Run` |
| **派单 v2 三段轴** | 长工单 + profile → 人标 assignee | 派单轴只测排序段 | ⚠️ 三段轴已交付；**"→ 人标 assignee"那一半没做**（人工金标仍缺） |
| **忠实度断言**（先规则版） | 起草字段必须能在对话历史里 grep 到 | 模型编内容看不见 | ✅ 规则版 + 两级版已实现（默认关闭，未接 `serve.go`） |

### 与其他两条主线的耦合

三条新轴**共用一份 `internal/evalrun` 抽象**（现在只喂 `TurnObservation`，
扩到喂 `TicketSnapshot + AssignmentLog`）—— 不是三套 runner。

**这条抽象扩展至今没做**（`AgentRunner.RunTurn` 仍只返回 `eval.TurnObservation`，2026-09-24 核对）。
它就是端到端交互轴唯一的前置工作，也是 D1 结构盲区一直关不掉的原因：
runner 不落库消息 → 历史窗口恒空 → 跨轮行为在这一轴上测不到。

---

## 四、跨主线耦合

```
主线 3（评测）              主线 1（派单）              主线 2（建单交互）
─────────────              ──────────────              ──────────────
evalrun 抽象扩展     ──驱动──►   v2 数据集先跑           ──驱动──►  round counter
                                 ↓                                    ↓
                                 倒逼"服务归属抽取"必须先实现        倒逼对话落库
                                 ↓                                    ↓
                                 assign/resolve.go           端到端交互轴 runner
                                                                       ↓
                                                          忠实度规则断言（先规则版）
```

三条主线**不是各自推**：主线 3 的每一条新轴都是主线 1 / 2 的推进前置条件。
反过来说——**主线 1 / 2 的每一步都必须带着新轴一起做**，不然就是没测过的功能。

---

## 五、三周推进表

| 周 | 主线 | 具体产出 | 实际 |
| --- | --- | --- | --- |
| **W1** | 3 | `internal/evalrun` 抽象扩展（喂 `TicketSnapshot + AssignmentLog`） | ❌ 未做 |
| **W1** | 2 | round counter 上线（3 轮上限）+ 端到端交互轴 5 条多轮用例 + fresh baseline | ⚠️ 三支柱与 8 条用例做了，但只是 `go test`；轴没成，baseline 无从谈起 |
| **W2** | 1 | `domain/service.go` + `assign/resolve.go`（ownership-first pipeline） | ✅ 已交付 |
| **W2** | 1+3 | `assignment_v2.json` 前 50 条 + 三段指标全跑通 | ✅ 45 条 + 三轴跑通；**人工金标那一半没做** |
| **W3** | 3 | 忠实度规则断言（描述实体必须在对话历史里出现） | ✅ 规则版 + 两级版，默认关闭 |
| **W3** | 1 | 派单 v2 三段指标进 `make check` 与 sabotage 集 | ✅ 下限进 `pipeline_v2_test.go`；注入是 Option 形态的 `TestSabotage_*`，非 CLI 开关 |

**没做成的那两格是同一件事**：`evalrun` 抽象不扩，端到端交互轴就立不起来，
D1 跨轮盲区、追问放弃率、描述忠实度检出率这三样会一直一起躺在待办里。
下一轮从这一格动手，不要再去动派单。

**每周结束必须能跑一次 `make check` 且指标可对外引用**，不能出现"改到一半"的状态。
（并发 caveat：另有一个 session 在同仓库改动，`git add` 前必看 diff。）

---

## 六、明确不做（触发条件表）

| 组件 | 触发加回来的条件 |
| --- | --- |
| 加权打分 → LambdaMART | 错派样本累积 ≥ 200 且每周新增 ≥ 20 |
| 单路 BM25 → hybrid + RRF | 员工库或历史工单库任一 > 5k 条，或 BM25 Recall@10 < 80% |
| 待认领池 → LLM rerank 层 | 待认领池每周 > 500 条，人看不过来 |
| margin 手调 → 概率校准 | 有 ≥ 2k 条"派完 + 反馈"闭环数据 |
| 人肉 review → 审计采样框架 | 日单量 ≥ 5k |
| `evalrun` 抽象 → 泛型 `Decision[T]` | ≥ 3 个子系统真出现同样接口形状（不是预感） |
| CMDB 服务归属 → GraphRAG | 服务依赖图节点 > 1000 且需跨 2-3 跳查询 |
| 派单反馈 → Bandit / 在线学习 | 反馈信号覆盖率 ≥ 70% |
| System Two 三级 → 二级 | 当前不做三级；一级 LLM 都还没上，讨论分层为时过早 |
| sqlite 持久化 + 租户隔离 | 见 `PHASE_ROADMAP`，属"能不能上线"范畴，不属本三条主线 |

**每一条都是规模驱动，不是技术先进驱动**。搜广推那套复杂度是从百万 QPS 里长出来的，
不是因为它高级。

---

## 七、与现有文档的关系

| 文档 | 状态 | 冲突时的优先 |
| --- | --- | --- |
| `EVALUATION_PLAN.md` | 指标定义权威（涉及技能的 C1/短期#6/待办#3 已按 09-24 二次修订） | 指标解释以此为准 |
| `HANDOVER.md` §11 | 最新事实（2026-09-24；§10 是 09-23） | 现状描述以此为准 |
| `DISPATCH_PIPELINE.md` §4.1 | 派单轴数字口径 | 派单数字以此为准 |
| `PHASE_ROADMAP.md` | 分期规划（§D 方案 A 已标注作废） | 长期方向以此为准；本 FOCUS 是**下一层**的执行 scope |
| `FUSION_DESIGN.md` §7-9 | 已被 PHASE_ROADMAP 取代 | 不作为依据 |
| `README.md` §下一步 | 2026-09-24 已重写为现状（三条真待办） | 与本文一致；细节仍以本 FOCUS 为准 |
