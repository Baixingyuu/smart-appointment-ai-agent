# P3 Agent 组装：一句人话走到"有人负责"

复现：`make probe-p3`（先离线复现 id 撞车，再跑五个真机场景；
`.venv/bin/python probes/p3_live_smoke.py [short|intake|dedup|kb|kb_vague]`，只有 `kb` 带退出码断言）、
`make chat`（同一套接线的交互版）。
离线断言：`tests/test_agent.py` 25 条，全仓 81 条全过。

## 五个场景的实跑（真机 qwen3:8b temp=0 + bge-m3 dense）

| 场景 | 输入 | 模型调用 | input / output token | 落库 |
|---|---|---|---|---|
| short | 报障 → "确认" | 4~5 | 7348~9875 / 227~339 | 1 张单，assignee=101，missing=() |
| intake | 模糊报障 → 补信息 → "确认" | 7 | 12576 / 307 | 1 张单，assignee=106，追问以中文口语问出口（"是哪个系统/服务出的问题？"） |
| dedup | 报障 → "确认" → 同一问题再报 | 5~7 | 9470~14729 / 329~555 | 第二句起了 park，没落成第二张单 |
| kb | 一句"查知识库"式提问 | 2 | 3482 / 98 | 0 张单，`search_knowledge` 命中 `kb-deploy-hardware`，答话基本逐字引它 |
| kb_vague | 同一件事的含糊说法 | 1 | 1643 / 35 | 0 张单，**零工具调用** —— 只反问了一句"您要的是哪方面的配置信息？" |

方案 P3 那条验收（描述问题→检索→查重→追问→待确认→确认→建单→派单）逐段对位：
检索 = kb 行；追问/待确认/建单/派单 = intake 行；查重 = `find_open_ticket` 在早一轮 intake
实跑里被调用过（答"没有相近的未关闭工单"），**最近这一轮直接跳过它去建单了**（见下面第二条如实记录）。
没有哪一轮把六段全走一遍 —— 单轮全绿的轨迹留给 P4 的 21 例轨迹集去硬断言。

一次确认覆盖整条链路：建单 + 定位服务 + 指派在同一条 reply 里跑完；
指派日志带 top-3 候选服务与置信度（例：`(2001, 0.634) (2013, 0.552) (2011, 0.523)`，conf=0.95）。
第一轮 1.6k input token 是**框架固定开销**（系统提示 + 6 个工具 schema + RAG 注入），
Go 侧没有这一项的对位。同一句模糊报障在加固提示词前后是 1595 → 1634（+39 token），
short 场景因为用户话术更长是 1606。

四条如实记录（都是实跑撞出来的，不是推的）：

- **temp=0 不等于轨迹可复现。** 同一个 short 场景两次跑：第一次 4 次模型调用，第二次 5 次
  ——首轮 `in=1606` 完全相同而 `out` 从 111 变成 149，因为 qwen3 第一次把 `confidence`
  漏了，改正重试了一次。Go 侧"temp=0 方差为 0"的结论**不能带过来**。
  P4 的翻转率协议不是形式主义，是必须做的。
- **查重步骤不总被遵守。** dedup 场景两次跑，一次第三句走了 `find_open_ticket`
  （答"没有相近的未关闭工单"，闸没命中），另一次直接调 `create_ticket`。
  提示词里写"建单前用 find_open_ticket 查重"不算数，轨迹轴要把它做成硬断言。
  兜住底线的是 ASK 闸（没确认就落不了单）和内核自己的去重闸，不是提示词。
- **`assign_ticket` 漏 `confidence` 是常态而非意外**（报障类场景反复出现，short 与 intake 各撞过）。
  以前它会**静默丢单**，现在它只是一次错误结果 + 一次改正重试（≈1.1k input token）。
  见证据 ④。
- **会不会去检索，取决于这句话怎么说。** 同一件事（私有化部署的硬件要求）：
  "帮我查一下知识库：……查了再回答" → 老老实实调 `search_knowledge` 并照结果答；
  "我们想私有化部署，机器配置有什么要求？" → 一次模型调用、**零工具调用**，反手问回来。
  而这次反问是**用普通话术问的，没走 `ask_user`** —— 追问计数器看不见它，额度也就没被扣。
  所以"这个 agent 会不会检索"不能靠一句测试话术回答（P4 轨迹轴要按话术分组统计），
  "追问最多两轮"这条闸也只管得住走 `ask_user` 的那部分。

## 代码形状（P3 新增/改动）

| 模块 | 行数 | 里面有什么 |
|---|---|---|
| `tools.py` | 291 | 5 个业务工具 + 权限分级（读=ALLOW、建单=ASK）+ 显式 schema |
| `runtime/confirm_bridge.py` | 159 | 自然语言 → `UserConfirmResultEvent` / `UserInterruptEvent` / 重开一轮 |
| `runtime/intake.py` | 118 | 追问进度（`middle_context` 存 + `on_system_prompt` 注入）与"追问必须说出口"闸 |
| `runtime/agent_factory.py` | 96 | `Agent` 组装：RAG/中间件/`ReActConfig`/`ContextConfig` + 业务提示词 |
| `runtime/traceability.py` | 78 | 引文核验漏斗（子串 → 字元覆盖率 ≥0.70），零模型调用 |
| `runtime/toolcall_ids.py` | 58 | **计划外新增**：tool_call id 唯一性，见证据 ④ |
| `app.py` | 84 | 会话循环 + 框架 `ConsoleRenderer` |
| `eval/fakes.py` `eval/scripted_model.py` | 50 + 148 | 离线替身；`Turn(ollama_ids=True)` 复刻框架 id 规则 |

计划 §4 估的三件自研：确认桥 ≈160（实际 159）、追问计数 ≈50（实际 118，因为多了 ⑧ 那条闸）、
可追溯 ≈60（实际 78）。`toolcall_ids.py` 完全在计划之外，是跑真机才暴露的。

总量对位（口径与 Go 的"非测试 12.9k 行"一致）：`src/` 2494 行（含 `src/helpdesk/eval/` 的脚本模型与替身）
+ 仓库根 `eval/` 391 行（人工标注表与校验器）= **2885 行 ≈ 4.5×**。
方案估的是 2.2k / 5.9×，**这条估算偏乐观约 700 行**，而且 P4 的评测轴还要再往分子上加代码
（加完只会掉到 4.0~4.5× 这一带，不会涨回 5.9×）。差额来源点名的有两笔计划外：⑧ 那条"追问必须说出口"的闸、
以及 `toolcall_ids.py`。`tests/` 726 行、`probes/` 820 行不计入 —— Go 的那个 12.9k 也是去掉测试后的数。

## 接缝证据：九条框架实测，每条都对应留下的代码或改掉的假设

① **`Agent` 不收集 middleware 提供的工具。** 只有托管路径
`app/_service/_toolkit.py:233` 会 `tools.extend(await mw.list_tools())`；直接用 `Agent(...)`
时 `RAGMiddleware` 的 `search_knowledge` 根本不在 toolkit 里（toolkit 那条测试就是这么测出来的）。
→ `make_agent` 改成 async，自己 `await rag.list_tools()` 并进 `Toolkit`。
真机对位：kb 场景确实调到了 `search_knowledge`（返回 `[1] (source: kb-deploy-hardware)`，
2 次调用 / 3482 in / 98 out），这条缝不止在离线 schema 断言里成立。

② **`InjectionConfig.extra_fields` 不保证每轮新鲜。** `_agent.py:1606` 的分支是
`if injections:`，框架自己的注释写着"user defined fields, which don't trigger an injection
by themselves" —— extra_fields 只在**已经有别的注入触发**时才被附上。时间注入默认 0.5 小时
一次，所以进度在多数轮次里根本不会进上下文。方案 v2 里"追问进度走 extra_fields"这条是错的。
→ 换 `on_system_prompt`（transformer 钩子，`_prepare_model_input` 每次模型调用前都跑），
进度为 0 时一个字都不注入。

③ **`is_state_injected=True` 的工具不能用注解生成 schema。** `tool/_utils.py:113` 把所有参数
（含注入用的 `_agent_state`）建成 pydantic field，而 pydantic 拒绝下划线开头的字段名
（`NameError: Fields must not use names with leading underscores`）。→ 这三个工具显式传 `input_schema=`。

④ **Ollama 适配器合成的 tool_call id 跨轮会撞车，后果是写操作静默丢失。** 三段机制：
`model/_ollama/_model.py:324` 用 `f"{idx}_{name}"`（只在一次响应内唯一）；
`state/_state.py:310-316` 把同一 reply 的所有块塞进**同一条** assistant 消息；
`state/_state.py:398-403` 与 `agent/_agent.py:3499-3507` 又拿"本条消息里已有的 tool_result id"
判定"这条调用执行过了"。于是同一 reply 里第二次调同一个工具不报错、不执行、连 tool_result 都没有。

真机现象：`assign_ticket` 漏填 required 的 `confidence` → 框架按 `message/_block.py:163`
的迁移置 finished 并回校验错误 → 模型改正再调一次 → **工单停在未指派**。

修法在 `on_model_call` 上给每次模型调用的 id 加单调前缀（`m0_0_assign_ticket`、
`m1_0_assign_ticket`），不去 subclass 模型：id 是适配器的事，重写它等于把 ollama 的
流式累加再实现一遍。`probes/p3_tool_call_id_collision.py` 把这条关进离线：修复前
`calls=2 results=1 assignee=None`（退出码 1），修复后 `calls=2 results=2 assignee=101`。
脚本模型默认 id 跨轮唯一，所以这个 bug 在此之前**不可能**被那 62 条离线断言发现 ——
这也是 `Turn(ollama_ids=True)` 存在的理由。真机随后连续两次给出同一形状的
error→retry→success 轨迹，指派都落了地。

`confidence` 保持 required：代价是真机上偶尔多烧一次模型调用，换来指派决策一定带置信度，
评测轴要有这个数。

⑤ **本地工具返回只认 `ToolChunk`。** `tool/_adapters.py:179` 对其它返回一律 `str(result)`
兜底并写死 `state=RUNNING`。看着最像对的 `ToolResponse` 恰恰是错的（它是框架汇总之后的类型，
`_response.py:50`）：真返回 `ToolResponse` 时 ERROR 被抹平成 success、`metadata` 整包掉进字符串。
→ 工具全改成返回 `ToolChunk`，`metadata` 才回到 `ToolResultBlock.metadata`
（实测 `{'ticket_id': 1, 'assignee': 101, 'escalated': False, 'candidates': []}`），
"指派被在职闸拒绝"也才真的是 `state=error`
（`test_refused_assignment_surfaces_as_error` 钉住这两条）。

⑥ **集合没 load 就 search 会抛 `state 'released'`** —— P0 记的"必须换新 KnowledgeBase 句柄"
是误诊，P3 已换成 `HelpdeskIndex.ensure_ready()`，详见 `docs/P0_PROBES.md` 的更正小节。

⑦ **前端还是零自研代码，但没用 `launch_console`。** 它把待确认写成 y/n 提示
（`console/_console.py:75`），正好绕开确认桥 —— 桥要接的是聊天里的一句话。渲染仍用框架的
`ConsoleRenderer`。托管路径 `create_app` 要 `agentscope[service]`（apscheduler），本轮不装，
会话也就仍不跨进程持久化（业务事实是进程内 `TicketStore`，恢复 `AgentState` 会得到
"模型记得建过单、系统里没这张单"的错位）。

⑧ **`ask_user` 之后模型不会自己停，而框架的"禁工具"开关在 Ollama 上是空转的。**
真机实测：qwen3:8b 在一轮里连调 `ask_user` ×2 然后直接 `create_ticket` —— 用户一个字都没被
问到，下一句话被桥判成 `new_request`，park 掉的建单被取消（Go 的 D2 吞消息在这条上换了个形态复现）。
框架给的缝是 `ToolChoice(mode="none")`（`tool/_types.py:185`），但
`model/_ollama/_model.py:223` 打一句 `Ollama ignores tool_choice.mode` 就把 mode 丢了，
实跑里模型照旧建单。→ `AskOutLoudMiddleware` 在 `on_model_call` 上把**那一次**调用的
schema 列表抽成 `tools=[]`（provider 支不支持 tool_choice 都不再影响结果）。
改完实测：追问那一轮真的停在提问上，用户回答后下一轮才查重建单。
残留问题也已消掉：撤掉 schema 后 qwen3 头一次会把 `{"name": "ask_user", ...}` 当话术吐出来
（那轮 `in=688 out=18`），在系统提示第 2 步写明"那一次用中文口语问，不要输出 JSON、不要模仿
工具调用格式"之后，同一轮变成 `in=727 out=9`，用户看到的是"是哪个系统/服务出的问题？"。
便宜了一半以上是因为没有工具 schema；+39 token 的固定开销付在每一轮（见上）。
诚实边界：这条只靠提示词，没有后置校验兜底 —— 上面是一次观测，不是收敛证明。
真要钉死得加一层"那轮的回复里不许出现 `{`"的改写或重试，本轮没做，记在 P4 的轨迹轴候选断言里。

⑨ **归因表必须跨轮活着。** `ToolResultEndEvent` 只带 `tool_call_id` 不带工具名，
而 park 之前发出的调用它的 result 会在下一轮才到达 —— 第一版冒烟脚本按行重建 id→name 表，
park 的建单结果就打印成了 `result ?=success`。运行时不留调用计数副本（P0 探针②：
`max_iters` 数的是模型轮次不是调用数），但**评测侧归约器**要按这条形状写。

## 已知限制

- 桥的词表判定是自研小逻辑（`classify`），批准/拒绝/新诉求三类靠精确集 + cue 词首现位置，
  没有语义模型兜底。落到 `NEW_REQUEST` 的判定会让 park 的建单取消、按新诉求重开 —— 宁可不建，
  不会误建。真实用户话术的覆盖率本轮没测（要 P4 的 21 例轨迹集）。
- 五个场景的样本量是"每形一次到两次"，只够说明接线通了与故障真实存在，不够报成功率。
- **追问额度有旁路**：模型可以完全不调 `ask_user`、直接用普通话术反问（kb_vague 实测就是这样），
  计数器看不见，额度也就扣不到。本轮只记录不修 —— 修法要么每轮多付一次"这算不算提问"的模型调用，
  要么再写一条不保证被遵守的提示词。真出问题时它的后果是"聊不完"，不是"丢单"，所以排在 P4 之后。
- 单测覆盖的是接线，不是模型行为：脚本模型永远走满，真模型会不会走（检索、查重）
  由话术决定 —— 见上面第四条如实记录。两者结论不能互替。
