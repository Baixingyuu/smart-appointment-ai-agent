# 交接文档 · 评测体系（2026-09-18）

> 这份文档面向**接手评测体系的人**（也包括下一次打开仓库的自己）。
> 它回答三件事：现在测到了什么、这些数字哪些能信哪些不能、下一步该动哪里。
> 详细方法论在 `docs/EVALUATION.md`，此处不重复，只标差异与状态。

---

## 1. 当前状态一览

| 轴 | 命令 | 当前数字 | 这个数字的含义 |
|---|---|---|---|
| 指派匹配 | `make eval` | Top-1 100%（150 样本） | **实现与规格一致**，不是真实准确率（金标由派单规则推导） |
| 意图路由 | `make eval-intent`（需密钥） | 真实模型 98.33% / 关键词基线 54.44%（`ARGS="-offline"`） | 真实模型显著优于规则基线；短路比例 45.6% |
| 检索召回 | `make eval-retrieval` | Recall@5 92.5%，门控后有证据 69/80，假命中率 10% | BM25 + 置信门控，全部数字可由该命令复现 |
| 执行轨迹 | `make eval-trajectory` | scripted 100% / **live 68.75%** | scripted 是链路自检；live 才测模型能力 |
| 成本与延迟 | 同上（报告内） | live：prompt 42,277 / total 51,527 token，P50 3.6s、P95 10.4s | 一次完整 live run 约 5 万 token |

**本次会话的全部改动仍未提交**（`git status` 7 个 M + 未跟踪 `docs/sum.md`、`docs/HANDOVER.md`）。
报告目录 `eval/reports/*.json` 被 `.gitignore` 忽略——是可再生产物，
要当回归基线长期保存需显式 `git add -f`。

---

## 2. 评测体系导览

### 2.1 为什么分五轴

RAG 只有一种错法（答得不对），agent 有四类各自独立的错法：
**该做没做 / 不该做却做了 / 做对了但绕远 / 做对了但顺序错**。
四类失败的改进方向完全不同，混成一个「准确率」就只能知道「变差了」，
不知道「该改哪里」。这是整个体系的立论，见 `EVALUATION.md` §1。

### 2.2 三层指标

```
汇总指标   PassRate / ToolAccuracy / OrderAccuracy / SafetyViolationRate /
           BudgetRejectRate / AvgRounds / Recall@K / MRR / Latency
   ↓
归因指标   rag.Reason（6 种）/ tooling.ErrorKind（7 种）/ 逐轮 token
   ↓
明细记录   ToolCallRecord / AssignmentLog / TicketProgress / failures[]
```

每层都要能回答「下一步改什么」。排查时直接查 `EVALUATION.md` §9 的现象↔指标对照表，
那是最实用的一节。

### 2.3 代码地图

| 位置 | 职责 |
|---|---|
| `internal/eval/{assignment,intent,retrieval,trajectory}.go` | 四份断言与报告，纯函数，不碰业务写路径 |
| `cmd/helpdesk-agent/eval_*.go` | 各轴的 CLI 入口、数据集装载、sabotage/模型注入 |
| `internal/evalrun/runner.go` | 把「回合观测」喂给断言的适配器（执行与判定分离） |
| `internal/agent/{agent,routing}.go` | 被测对象：工具循环 + 意图短路 |
| `internal/tooling/` | 工具治理：白名单 → 风险 → 预算 → 确认 |
| `eval/datasets/*.json` | 数据集；`eval/verify_datasets.py` 独立复核金标 |

### 2.4 五条红线（改动时不要破）

1. **评测是观察者**：业务不得 import `internal/eval`，评测不得写业务表。
2. **指标可复现**：不使用 LLM 作为评判者——工具序列是结构化的，可精确比对
   （论证见 `EVALUATION.md` §1 的「是否需要 LLM 评判」一行）。
3. **派单器是纯函数**：可测性的前提。
4. **依赖单向**：`domain` ← 其他，反向 import 一律拒绝。
5. **授权与流程分离**：确认中断管流程，工具治理管授权，两者不互相顶替。

### 2.5 三条使用纪律

1. **看报告先看 `mode`**。轨迹报告没有 `mode` 字段就等于没有结论——
   scripted 的 100% 与 live 的 100% 是两个完全不同的命题。
2. **改完跑 sabotage 自证**。`make eval-trajectory ARGS="-sabotage=always_write"`，
   对应指标必须下降；不降说明断言写空了。
3. **改数据集或配置后必须重测文档表格**。`EVALUATION.md` §7 的表本次就因此漂移过
   （移除 tj-011 的 `minRounds` 让 `always_write` 从 18.8% 变成 25.0%）。

---

## 3. 命令速查

```bash
make check                    # fmt + vet + test + verify-data（不含评测报告）
make verify-data              # 用精确有理数独立复核金标
make eval                     # 指派（基础集 + 对抗集）
make eval-intent ARGS="-offline"              # 意图：关键词基线（零成本）
make eval-retrieval                           # 检索：Recall@K / MRR / 假命中率
make eval-trajectory                          # 轨迹：离线脚本，链路自检
make eval-trajectory ARGS="-sabotage=always_write"   # 评测自证（仅脚本模式）

# 轨迹 live 模式（本次新增）：给密钥就用真实模型测能力，零代码改动
LLM_API_KEY=sk-xxx make eval-trajectory ARGS="-json eval/reports/trajectory_live.json"
# 也可显式指定服务与模型
LLM_API_KEY=sk-xxx make eval-trajectory ARGS="-base-url=... -model=..."
```

默认模型 `deepseek-flash`，默认 `base-url` 为 DeepSeek 官方 `/v1`；
`-api-key` 缺省时读 `LLM_API_KEY`。`-sabotage` 与 `-api-key` 互斥
（脚本级缺陷注入对真实模型无意义），同时给出会直接报错。

---

## 4. 本次改动：轨迹评测的 live 开关

### 4.1 为什么改

审计发现结构性缺陷：`buildReplayRunner` 的脚本化模型是**从数据集的 `Expect` 反推行为**的，
所以 scripted 100% 是同义反复——它证明断言代码没写错，不证明模型做对了。
轨迹轴此前**从未对真实模型测量过**，这是评测体系里最大的一个空洞。

### 4.2 怎么做的

| 文件 | 改动 |
|---|---|
| `cmd/helpdesk-agent/eval_trajectory.go` | 新增 `-api-key/-base-url/-model`；`executeCase` 拆成重试壳 + `executeCaseOnce`；live 时注入 `classify.New(model)` 分类器；每条消息 180s 超时（`liveTurnTimeout`）；逐用例 stderr 进度 |
| `internal/eval/trajectory.go` | 报告新增 `Mode / Model / Sabotage` 元数据，`Text()` 明确标注「链路自检」还是「真实能力」 |
| `eval/datasets/trajectory.json` | 移除 tj-011 的 `minRounds: 1`，并在描述里写清「轮次 = 工具循环内决策轮数，短路回合为 0」 |
| `docs/EVALUATION.md` | §3.4 双模式表、§8 边界②给出 live 命令、新增 §8.6 首测失败模式、§7 sabotage 表按当前数据集重测、§10 状态更新；§3.5 标题层级由 H2 降为 H3（与其他 3.x 一致） |
| `README.md` / `Makefile` / `main.go usage()` | live 模式入口与说明同步 |

四个设计选择值得记住：

- **执行与判定分离**：`executeCase` 只产出 `[]eval.TurnObservation`，断言仍走同一套
  `eval.Runner`。所以 live 不是另一套评测，是同一个判据换驱动源。
- **live 注入分类器**：与 `serve.go` 生产路径一致，测的是「部署形态的系统」，
  不是裸模型。否则测出来的数字接不上线上行为。
- **tj-011 的改动不是迁就测试**：短路回合 `Rounds == 0` 是**正确行为**，
  原 `minRounds: 1` 会把正确行为判失败。理由写在数据集描述里，不静默改期望。
- **失败重试从紧**：只做**用例级一次**重试，且每次新建 store ⇒ 无副作用累积。

---

## 5. 首份 live 报告怎么读

`eval/reports/trajectory_live.json`（model=deepseek-flash）：
PassRate **68.75%**（11/16）、ToolAccuracy 55.56%、OrderAccuracy 50%、
**SafetyViolationRate 0%**、AvgRounds 1.69、prompt 42,277、P50 3.6s / P95 10.4s。

5 条失败全部是**漏做**，且全集中在建单链路的最后一跳：

| 用例 | 现象 | 归因 |
|---|---|---|
| tj-004 | 只检索未建单，后续「确认」无中断可恢复 | 检索无果后未发起建单确认 |
| tj-005/006 | 「帮我建个工单」零工具调用 | 模型选择追问信息，而非带 `MissingInfo` 起草 |
| tj-007 | 检索 + 查重都做了，独缺建单确认 | 同 tj-004 |
| tj-010 | 检索两次后作答，忽略同一句里「顺便登记」 | 混合意图只处理主诉求 |

两点解读纪律：

1. **0% 越界是好消息**。16 个场景里模型从未「不该建单却建单」，错误全是「该建没建」。
   对写操作这是正确的失败分布——漏建可由追问补回，越权建单是脏数据。
2. **tj-005/006 是金标口径问题，不是模型缺陷**。「不因信息不全而拒绝建单」是**系统**设计
   （`MissingInfo` 机制），模型天然倾向先追问。这两条在人工裁决前应记为「待定」。
   裁决结果无论哪边，都要同时改提示词或期望，不能只改一边。

---

## 6. 客观性与有效性：审计结论

**成立的部分**（都由执行验证，不是读文档得出的）：
分轴设计对应四类独立失败；sabotage 四种缺陷确实命中**不同**指标（不是只有 PassRate 动）；
金标由 `verify_datasets.py` 用 `Fraction` 独立复算，与生成脚本不共用代码，避免了循环论证；
评测不写业务表；成本有逐轮归因且固定开销被测试上界锁住（766 token / 3 工具）。

**弱点清单**（含本次已关闭项）：

| # | 弱点 | 状态 |
|---|---|---|
| 1 | 轨迹 scripted 模式循环论证，真实能力从未测过 | ✅ 本次关闭（live 模式） |
| 2 | 指派轴金标来自规则推导，100% 不等于真实准确率 | ⏳ 需要历史「人工实际指派」数据（三期） |
| 3 | 检索不可回答集缺同域近邻负例，FPR 区分力有限 | ⏳ 未动 |
| 4 | 报告不带「怎么生成」元数据。两个实例：`intent_baseline.json` 其实**是真实模型跑的**（prompt=38,627，关键词基线 token=0），命名与含义相反；`retrieval_baseline.json` 里的 70% 假命中率用当前代码复现不出来（`-threshold` 打 0.0001/0.05/0.1 三档均仍为 10%），它是门控阈值重定标之前的历史产物（其 58 与 `EVALUATION.md` §3.5 表里的 53/80 也对不上，无法归因到任何已记录配置） | ⏳ 未动，见 §7 |
| 5 | 无回归基线与新鲜度门禁：`make check` 不比对报告。实例——磁盘上的 `retrieval.json` 记的是 `decidedHitOnAnswerable=44`，而当前代码 fresh run 为 **69**（文档 §3.5 同样是 69/80），其余字段一字不差，说明它是门控重定标前的残留，且**没有任何机制会报警** | ⚠️ 本次已重新生成该报告，门禁机制仍未动 |
| 6 | live 指标单次运行，未测方差——68.75% 里有多少是噪声未知 | ⏳ 未动 |
| 7 | 无「回答质量」轴：只测做了什么，不测答得好不好 | ⏳ 三期；LLM-judge 只能在这一层、隔离接入，并先用人工标注元验证 |

关于 LLM-as-judge 的结论（第二次提问的答案）：**不是现在的第一优先级**。
先用确定性的 live 轨迹把「能力有没有」测准（本次已完成），
judge 只在「答得好不好」这一层补，且单独出一份报告，不与确定性指标混在同一个 PassRate 里。

---

## 7. 已知坑与注意事项

- **温度没有真正固定**：`internal/llm/client.go` 的 `applySampling` 只在 `Temperature > 0` 时才下发该字段，
  默认配置下走的是服务端默认值。live 报告的可复现性上限受此限制，想收紧就在 `llm.Config` 显式设值。
- **`eval/reports/*_baseline.json` 的命名不可信**，判断某份报告跑了什么，看 `promptTokens` 是否为 0
  （0 = 离线/关键词），或看轨迹报告的 `mode` 字段。
- **报告是 gitignore 的可再生产物，磁盘上的旧文件不会自己更新**。本次就发现 `retrieval.json` 里
  `decidedHitOnAnswerable=44` 是门控重定标前的残留，已用 fresh run 重新生成为 69（与 `EVALUATION.md` §3.5 一致）。
  引用任何报告数字前，先重跑一次对应命令。
- **sabotage 表与数据集强耦合**：改期望后要重跑四种注入，否则表里的数字会误导跨版本对比。
- **live run 有成本**：16 用例 25 条消息 ≈ 5 万 token、约 2 分钟。改提示词前的对照 run 值得留档。
- **`-sabotage` 只在脚本模式有效**，与 `-api-key` 同时给出会直接报错，这是设计而非 bug。

---

## 8. 建议的下一步（按性价比排序）

1. **修「建单最后一跳」**：tj-004/007/010 是同一个失败模式——提示词没有把
   「检索无果/信息不全也要发起建单确认」讲成硬要求。改 `agent` 系统提示词后跑一次 live 对照，
   预期 PassRate 从 68.75% 上到 80%+，成本一次 run 约 5 万 token。
2. **人工裁决 tj-005/006 的金标口径**，并同步改提示词或期望（见 §5）。裁决前不要把它们计入回归基线。
3. **live 跑 2–3 次记方差**。没有方差，任何阈值判断都是假的；确认稳定后才把 live 报告当回归基线。
4. **给 assign/intent/retrieval 报告补 `mode` + 生成命令元数据**，并纠正 `_baseline` 命名
   （轨迹轴的 `Mode/Model/Sabotage` 已经是一个可抄的样板）。
5. **检索轴补同域近邻负例**（形如「问的是知识库里没有的同类问题」），让假命中率真正有区分力。
6. **`make check` 加报告新鲜度门禁**：至少一个脚本比对报告里的数字与文档表格，不一致就失败。
7. **回答质量轴（三期）**：确定性指标覆盖不到的地方再引入 judge，隔离出报告、先做元验证。

---

## 9. 密钥处理

本次 live run 使用的 API Key 由对话提供，**只通过 `LLM_API_KEY=` 环境变量前缀在运行时传入**，
未写入代码、文档、报告或提交（仓库内已 grep 确认无残留）。
该 key 已出现在对话记录中，建议事后在服务商侧轮换。
