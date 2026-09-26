# P0 探针结论（三个脚本跑出来的，不是读文档读出来的）

复现：`make probe`（或逐个 `.venv/bin/python probes/p0_*.py`）

## ① embedding + 向量库（`probes/p0_vector_store.py`）

MilvusLite + bge-m3 在 macOS arm64 **直接跑通**，不需要自实现 `VectorStoreBase` 的退路。

| 观测 | 冷跑（禁缓存） | 暖缓存 |
|---|---|---|
| 索引 21 条（14 服务 + 7 知识） | 2.7s（0.13s/条） | 1.6s（0.08s/条） |
| 45 条查询 | 2.2s（50ms/条） | 0.1s（3ms/条） |

`FileEmbeddingCache` 会把 embedding 延迟几乎全部吃掉（50ms → 3ms），所以成本数字必须写明缓存状态；
`HELPDESK_EMBED_CACHE=off` 可以关掉。

**服务抽取 top-1（金标 = 人工写的 `serviceTop1`，45 条）：36/45 = 80.0%**（判空 3 条一律算错时的数）。

- 正确命中分数区间 0.539–0.758，错误命中 0.485–0.654 → **两个区间重叠，单一全局阈值切不开**。
- 3 条 `serviceTop1=0` 的 top-1 分数是 0.536 / 0.569 / 0.610。要全挡住需要 t>0.610。

> **2026-09-26 更正（我自己算错的一条对比）**：本节原写"阈值扫描最优 t=0.58 → 37/45=82.2%，
> 相比 Go 的 42/45=93.3% 净赔 5 条 / 11.1pt"。那个扫描把闸同时套在了**具名金标**案例上
> （top-1 分数低于阈值的正确命中也被判错），而 Go 的规则（`pipeline_v2.go:229`）里阈值
> **只对 `gold=0` 生效**，具名金标只看 top-1 id 是否相等 —— 两个口径混用了。
> 按 Go 原规则重算：同一份 dense 结果是 **40/45 = 88.9%（最优 t=0.61）**，
> 相比 Go BM25 的 42/45 = 93.3%，**净赔 2 条 / 4.4pt**，不是 11.1pt。
> 详细分布与扫描表见 `docs/P2_KERNEL.md`。
> "阈值买不回被删掉的置信门"这条结论仍然成立（判空三例的分数与正确命中区间重叠），
> 只是代价被我算大了。

另一个测量敏感（记下来免得以后对着数字发懵）：探针把工单拼成 `标题 空格 描述`，内核用
`Ticket.text = 标题。描述`。整份 45 条里只有 1 条的 top-1 因这个标点而翻转 ——
`dp-v2-023`：空格拼接给 2014（score 0.4853），「。」拼接给 2010（score 0.4847，且这才是金标）。
两次分数只差 0.0006，属于刀口上的样本。所以抽取轴的报表必须钉死文本构造方式，
`eval/reports/extract.json` 就是按 `Ticket.text` 跑的。

**架构结论（改了层 1 的职责）**：dense 只给分数、给不出"什么都不像"这个判定，
所以**判空不能留在层 1**。层 1 只报 top-3 服务 + 分数，`无服务可归` 由层 2 的结构化输出
（枚举里带 `ESCALATE_HUMAN`）来落。

**一条框架坑（P0 记的诊断，P3 换了修法）**：`KnowledgeBase.ensure_collection` 只 memoize
"这个实例建过集合没有"。P0 看到的后果是同一进程内 `delete_collection` 之后拿旧句柄
`search` 抛 `MilvusException: collection 'services' does not exist`（而不是静默返回空），
当时下的结论是"每次重建必须换新句柄"——**这条 workaround 已被 P3 作废**。
P3 用 `--keep-index` 复跑撞到更疼的一种：**新进程打开一个已存在的集合时同样不会
`load_collection`**，于是 `search` 抛 `Collection 'knowledge' is in state 'released'`，
换句柄也救不了（memoize 是真的，集合没 load 也是真的）。
现在的规则是 `HelpdeskIndex.ensure_ready()`：拿 `store.create_collection` 当幂等闸
（`MilvusLiteStore.create_collection` 对已存在的集合就是 `load_collection`），
`__aenter__` 里跑一次，重建后（delete 过）再跑一次。
pymilvus 即使异常被捕获也会往 stderr 打整段 traceback，探针里把它的 logger 压到 CRITICAL。

## ② 轮次记账（`probes/p0_round_accounting.py`）

**`max_iters` 数的是模型轮次，不是工具调用次数。** 一轮里发 3 个 tool_call →
模型被叫 1 次、`cur_iter` 只 +1、3 条工具结果照发。

后果：Go 侧「一次回复最多 N 次工具调用」那道闸**不能直接写成 `max_iters`**，
`max_iters=50` 在最坏情况下允许约 150 次工具调用。调用计数得自己挂在
`ToolResultEndEvent` 上（这也是"调用计数与归因"中间件唯一的存续理由）。

其余观测：

- `is_concurrency_safe=False` 的多个调用**串行且保持模型给出的顺序** → 自研排序器可以删。
- 同一轮既给文本又给 tool_call 时，Acting 优先，文本不会提前收尾。
- park 的那一轮 **`cur_iter` 不 +1**（`_agent.py:1262`），等确认不消耗轮次预算。
- `cur_iter == max_iters` 时框架**额外叫一次模型**逼收尾，`tool_choice=ToolChoice(mode="none")`；
  实测 `max_iters=2` → 模型被叫 3 次、发出 1 条 `ExceedMaxItersEvent`、
  `ReplyEnd.finished_reason=exceed_max_iters`。成本记账按 `max_iters + 1` 次算。
- 叫一个未注册的工具名**不抛异常**，改成 1 条 `state=error` 的工具结果回喂模型
  → 评测归因必须读 `ToolResultEndEvent.state`，不能只数事件条数。
- 脚本模型必须尊重 `tool_choice=none`（否则会把"真实 API 不可能出现的输出"喂给框架，
  测出来的行为是假的）。这条是探针②跑出来的，已写进 `ScriptedChatModel`。

## ③ park / 唤醒契约（`probes/p0_park_resume.py`）

写工具声明 `permission=PermissionDecision(behavior=ASK)` 即可被拦，**不需要自研状态机**。

| 场景 | 框架行为 |
|---|---|
| 命中 ASK | 发 `RequireUserConfirmEvent(reply_id, tool_calls[])`，**不发 `ReplyEndEvent`** |
| park 期间塞普通 `UserMsg` | `ValueError: Agent is waiting for 1 tool calls ... but received no event.` |
| `UserConfirmResultEvent(confirmed=True)` | 工具执行，同一 reply 内继续推理到收尾 |
| `UserConfirmResultEvent(confirmed=False)` | 一条 `state=denied` 的工具结果回喂，模型接着想，工具没执行 |
| 重复/过期决策 | agent 层**抛 ValueError**（幂等只在 app 层 `resume_after_decision` 里，它返回 False） |
| `UserInterruptEvent` | 待确认调用被 `state=interrupted` 关闭，`ReplyEnd=interrupted`，**不进推理循环** |

三条对确认桥的硬约束，都是这里定下来的：

1. **park 时不发 `ReplyEndEvent`** → SSE/前端不能靠它判断"这一轮结束了"，
   只能靠 `RequireUserConfirmEvent` 或那条固定的 `"I'm waiting for your permission..."` 兜底消息。
2. **裸用户消息在 park 期间是异常而不是丢弃** → Go 的 D2（消息被静默丢掉）结构上不可能复发，
   但代价是必须有人把自然语言翻成事件。**这就是确认桥存续的证据**：框架给的是
   bool + tool_call_id 的机制和词表为 `{"allow","approve"}` 的**卡片按钮回调**，
   用户说"嗯嗯先这样吧""我还想提别的"没人翻。
3. **agent 层不幂等** → 桥必须先从 `state.get_awaiting_tool_calls()` 读 ASKING 集合，
   再决定发不发事件；不能拿前端回传的 tool_call 直接发。
4. **"新诉求"= 先 `UserInterruptEvent` 再重开一轮**，reply_id 会换。
   不需要 Go 那套 superseded + TTL 的补丁。
