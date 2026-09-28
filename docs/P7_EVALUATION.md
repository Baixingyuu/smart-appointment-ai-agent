# P7 评测层：量具从哪来、红线怎么变成退出码

复现：`make eval`（extract + retrieval + trajectory 离线 + sabotage 自检，一次模型都不叫，
四档全绿 exit=0）、`make eval-all`（再加读历史落库的运行时轴）、
`make eval-live`（21 条金标 × qwen3:8b，约 12 分钟）、`make eval-judge`（前 4 条再叠一层评委）。
测试面：`make test` 里 `tests/test_eval_gates.py` 31 条 + `tests/test_eval_runtime.py` 8 条 +
`tests/test_eval_judge.py` 7 条，全部离线（全套 263 条，2026-09-27 核）。

## 1. 框架给了什么，没给什么

安装包是 agentscope 2.0.8。三条容易踩错的事实，都实测过：

- **没有评测器**。`agentscope.evaluate` 不存在（wheel 的 RECORD 里没有这条模块，import 失败），
  顶层导出只有 `exception/logger/set_id_factory/set_timestamp_factory/setup_logger/warnings`。
  中文文档 `tutorial/task_eval.html` 讲的是 **1.x** 的 `Task/Metric/Evaluator`，还写着
  `agentscope.init(studio_url=..., tracing_url=...)` —— 2.0.8 里没有 `agentscope.init()`，那是文档漂移。
- **有 tracing**。`.venv/.../agentscope/middleware/_tracing/` 1438 行，暴露 `TracingMiddleware`，
  span 覆盖 reply / model-call / tool-execution，属性直接用的是
  `opentelemetry.semconv._incubating.attributes.gen_ai_attributes`
  （`gen_ai.usage.input_tokens/output_tokens`、cache token、model、temperature、finish reason）。
  `opentelemetry-{api,sdk,exporter-otlp,semantic-conventions}` 是**核心依赖**，不是可选的。
  早先我在这里写过一句"框架里没有 tracing"——那是错的，成因见 §7。
  但 span 只有在注册了真 `TracerProvider` 时才产出（`_check_tracing_enabled`），否则钩子廉价短路；
  `create_app()` 没有任何 telemetry 参数，唯一的缝是 agent 的 `middlewares=[...]`
  （我们的在 `src/helpdesk/service.py:95`，每会话在 `app/_service/_chat.py:946` 重组）。
  **这一层本次没有接**：接上要多起一个 OTLP collector，而 P50/P95 与 token 已经能从
  下面两个源拿到，不必先付那个运维面。缝的位置留在这里，要接就是 `middlewares()` 加一项。
- **自带 pass/fail 语义的只有 `GoalPipeline`，而它的 verifier 是模型判的** —— 不能当红线用。

所以判分被切成两处，这也是本次删掉的那条红线的去处：
**穿没穿闸用代码判（`eval/gates.py`），答得好不好用 py-openjudge 的 grader 判（`eval/judge.py`）。**

## 2. 四个测量源，各自能答什么

| 源 | 能答 | 答不了 |
|---|---|---|
| 事件流（每个事件带 `created_at`；`ModelCallEndEvent` 带 per-call token） | 分轮归因：哪一轮花了多少 token、工具执行了多少毫秒 | 跨会话的历史分布 |
| `Msg.usage` | 一次 reply 的总输入/输出 token | 分轮（只有合计） |
| 托管落库 `messages` 表（每个块带 `created_at/finished_at`） | 真机流量的延迟四分段、单次受理的 token | 哪一轮花的 |
| `TracingMiddleware` → OTLP | 跨进程的 span 树 | 本次未接 |

`messages` 表的块结构有个坑，值得单独写：**`tool_call` 块 50ms 就封板了**，那不是工具执行。
真正的执行窗口在 `tool_result` 块自己的 `created_at→finished_at`（`match_service` 1.62s、
`assign_ticket` 72ms、`create_ticket` 0.5ms），而确认类工具从"调用封板"到"结果出现"之间隔的是
**人读卡片的时间**。

## 3. 延迟必须拆四段，合起来报一个 P95 是错的

从 `data/service.db` 里那几条真实会话量出来的。其中一份以错误收尾的 reply（0 轮、0 token、
一行 hint）**照判照报，但不进分布** —— 把它算进 p50 就等于把真实会话稀释成"又快又省"：

```
5d6a6213#0  以错误收尾（type=upstream）→ 判软、不进分布
33cae606#0  rounds=4 wall=  82386.7 model= 18100.2 tool= 1692.6 wait=  62593.9 resid=0.0 tok=12823/272
e5537716#0  rounds=4 wall=  94818.2 model= 18998.5 tool= 1700.1 wait=  74119.6 resid=0.0 tok=12937/292
62929b6f#1  rounds=4 wall=  24776.6 model= 15585.6 tool= 2058.9 wait=   7132.1 resid=0.0 tok=13635/296
（另有两条 ask_user 单轮的：wall≈10.6~10.9s，tok≈4.1k/90）
wallMs p50=24776.6  modelMs p50=15585.6  toolMs p50=1692.6  waitMs p50=7132.1  residualMs p50≈0
```

同一条 `create_ticket`，tool_call→tool_result 隔了 62 秒，工具本身只花 0.5 毫秒。
把它记进 `toolMs` 就会得出"工具慢，去优化 embedding 往返"的假结论，而真相是人在读卡片。
所以 `Trace` 把 `wall = model + tool + wait + residual` 四项摊开，`residual` 显式算出来：
真实数据里它 ≈ 0（±0.1ms，`tests/test_eval_runtime.py` 钉住），说明没把没测的东西藏进总数。

## 4. 分位数用向上取整的最近秩

`percentile()` 取 `ceil(q*n)-1`，不是本仓 `metrics._pct` 的 `round(q*(n-1))`。
差别在 n=20 时才会咬人：后者把 P95 落在第 19 个样本上，等于报出一个不存在的分位数。
n<20 时 `runtime_summary` 会附 `p95Note`（"n=6 撑不起 P95，只能看形状"）—— 样本不够就明说，
不把 P95 当成一个可比的数。空输入返回 `None` 而不是 `0`，因为 `0` 会被读成"延迟为零"。

## 5. 红线 / 软阈值 / 退出码

`Check.fatal` 只留给"发生了不该发生的事"：写操作绕过确认闸、调了禁用工具、轮次失控、
整轮以错误收尾、该被闸拦住的没拦住。其余是可商量的软阈值（轮次预算、轨迹分、must_call、收尾空正文）。
**`exit_code()`：0 通过 / 1 软阈值未达 / 2 红线挂掉。**

这条改动的全部动机是：以前 `_main` 无论测出什么都 `return 0`，于是"不可回答的问题被放过"
这种致命项只是打印一行，CI 挂不住。现在检索轴在**生产同一档阈值**（`KNOWLEDGE_SCORE_THRESHOLD`，
0.55，评测不再自带一个会漂的常量）上判 leak，运行时轴读真机会话，轨迹轴读金标。

红线名单不来自提示词，来自权限表：`CONFIRM_TOOLS` 必须等于 `tools.py` 里 `permission=_ASK`
的那几个，`test_confirm_tools_match_permission_table` 用框架的公开访问器 `check_permissions()`
钉住它。加了新的写工具而评测还在按旧名单放行，就是这条测试先红。

还有一条判分器自己的坑被测试抓出来过：`must_not_call` 原来只看已落地的步，
于是"调了禁用写工具、但被确认闸拦住"反而判通过。现在 park 的调用也会记成一步
（`state="asking"`，事后被结果事件原地改写，不重复计），所以它响。

### 错误收尾要分归责，不然红灯永远亮

`reply_not_errored` 不再一律判红。框架在 `agentscope.types.ErrorType` 里给了分类，落库的
`payload["error"]["type"]` 直接带着它（实测那条是 `upstream`），所以按分类切：

| kind | 判 | 理由 |
|---|---|---|
| `upstream` / `connection` / `rate_limit` | 软（exit 1） | 当时那次部署的可用性问题。运行时轴读的是历史落库，重跑当前代码改不动它 |
| `internal` / `setup` / `authentication` / `invalid_request` | 红线（exit 2） | 本仓把自己跑崩了（装配失败、框架 bug、参数不合法） |
| 没给分类 | 红线 | 归不了责不当免罪理由 |

判软不等于放过：那条 reply 仍然 `ok=False`、仍然打印出来，只是不当成"闸穿了"。
`tests/test_eval_gates.py::test_reply_error_fatality_follows_attribution` 逐档钉住。

### 量具自检的退出码是反的

`sabotage_verdict` 是唯一反过来的一条：`--sabotage` 每条都注入了违规，注入的那条失败是
**设计好的失败**，所以该行的退出码只由"响没响"决定 —— 响了 exit 0，没响 exit 2。
修之前它按字面判红，`make eval` 就永远绿不了；而红灯永久亮等于没有红灯。

### 于是 make 目标这么分

- `make eval` = extract + retrieval + trajectory(离线) + sabotage。四档都不叫模型、
  都只判当前代码，所以"全绿 exit=0"是一个可达且有意义的风控点。
- `make eval-runtime` / `make eval-all` = 观测轴。它判 `data/service.db` 里的历史，
  现在那份数据给的是 exit 1（一条上游 5xx），不进门禁 —— 否则门禁永远打不开。
- `make eval-live` / `make eval-judge` 单独跑：花模型的钱，且延迟按定义会飘。

## 6. 轨迹 grader：先量量具，再量模型

`ToolCallStepSequenceMatchGrader`（openjudge，纯规则、不调模型）配成 `strict=False,
jaccard=False, metric="recall"`。实测出的语义是**逐步对齐**（参考第 i 步 对 实际第 i 步）：

| 轨迹 | 分 |
|---|---|
| 正序两环 | 1.0 |
| 乱序 | **0.0** |
| 3 环里漏掉中间一环 | 0.333（后面全被错位对掉） |
| 3 环里换掉一环 | 0.667 |
| 正序之后多调两环 | 1.0 |

"多调不扣分"是**必须**的：我们的运行时在建单之后还会 `match_service` 取推荐材料、
`propose_appointments` 拿可上门时段，而 Go 金标止于确认建单。开 jaccard 就变成按集合比，
乱序也会拿满分 —— 那 `orderedTools` 这条断言就成了假仪器。这几档都写成了 `tests/test_eval_judge.py` 的断言。

金标集 `eval/datasets/trajectory.json`（version 5，21 条）是从 Go 项目带过来的，只换了工具名：
`rag_search→search_knowledge`、`ticket_create_confirm→create_ticket`、
`ticket_find_open_by_topic→find_open_ticket`，别名表在 `run.py:TOOL_ALIASES`，报告里带出去。
version 4 是本仓自己加的口径：**21 条的 `forbiddenTools` 全部加上 `assign_ticket`**（业务口径改成
"助手只推荐工程师、指派由人在系统外做"，而这条禁令在 v3 里一处都没钉 —— 摘白名单之前它的
mustCall/forbidden 计数都是 0，改完不写断言就等于没改）。原本没有禁用项的 11 条建单类用例把它
放在第一位，于是 `--sabotage` 注入的违规正好是"建完单顺手把活派出去"。
要说清边界：这条只钉住"**不该调**"，"该调 match_service 出推荐"仍然没有金标 —— 少调一次推荐材料
现在不会被判失败。

version 5 是同日二次口径（推荐多个候选 + 对话内闭环落指派）：21 条的 `forbiddenTools` 摘掉
`assign_ticket`（不再禁用），`assign_ticket` 计入 `eval/runtime.CONFIRM_TOOLS` 走确认闸。后果是
11 条建单类用例的 forbiddenTools 清空，`--sabotage` 对它们的注入退化成"抽掉金标第一步"（软阈值
响），mustNotCall 红线的端到端验证改由知识问答/寒暄类用例（仍禁 ticket_create_confirm 等）承担。
「该调 match_service 出推荐」「该调 assign_ticket 落指派」仍无硬金标，只由提示词约束。

### 离线轴测的是量具，不是模型

默认那条 `make eval-trajectory` 用"理想模型"（`ScriptedChatModel` 照金标演）跑 21 条：
**必须全绿**（现在 21/21）。`make eval-sabotage` 把每条演坏一次（有禁用工具的就去调禁用工具，
没有的就抽掉金标第一步）：**必须每条都被判出来**（现在 21/21 判出、0 漏检，退出码 0）。
报告里 `sabotageDetail` 逐条写着是哪一项响的（`must_not_call`/`confirm_parked` 是红线，
`must_call`/`tool_sequence_score`/`answered` 是软阈值），所以那个 0 不是空口说的绿。
离线轴的延迟与 token 是脚本里的常数，报告里明写 `latencyIsCanned: true`，不冒充成本测量。
两条腿的落盘也分开了（`eval/reports/trajectory.json` 与 `trajectory_sabotage.json`）——
以前共用一个文件名，`make eval` 里跑在最后的 sabotage 会把"理想模型全绿"那份报告覆盖掉。

这条自检当场抓出两个真 bug：`final_text` 一直取不到（reducer 用的是 `get_text_content()`，
而正文其实以 `TextBlockDeltaEvent.delta` 流式过来，没有任何事件带那个方法）—— 症状是
所有案例的 `answered` 一起失败；以及上一条的 park 盲区。

## 7. openjudge 作为评委，边界写在这里

`eval/judge.py` 把 grader 指到 ollama 的 OpenAI 兼容端点（`/v1`）。三条不体面的事实要说明白：

1. **评委默认就是被评者自己**（同一个 `qwen3:8b`）。同一模型给自己打分有自偏好，所以报告
   必须带 `judgeModel`，分数只能看趋势，不能当达标线。要有可用的绝对分，得先拿一批人工标注
   算一致率（κ）—— 这一步没做，报告里也就明写着没做。
2. 一个维度 = 每条案例一次评委模型往返，所以 `--judge` 默认只判 `relevance`，别一次全开。
3. 本机系统代理会劫持 127.0.0.1 回空正文 502（probes 那边踩过）。评委这条链靠
   `helpdesk/__init__.py` 的 NO_PROXY 护栏，`judge.py` 里不重复装。

顺带一条方法论记录：这个仓里"Grep 工具跳过 .venv"曾直接导致我报出过 §1 那句假结论
（框架明明带 `TracingMiddleware`，我搜了三轮都说没有）。搜依赖内部的东西要用 bash grep。

## 8. 真机第一轮量到了什么（21 条 × qwen3:8b，temp=0）

这一轮跑在 2026-09-27 的"助手不直接派单"口径之前，所以里面的 `assign_ticket` 是历史数字：
现在模型面看不见这个工具，重跑这一轴不会再出现它。

```
wallMs   p50= 7961.4 p95=21923.1 max=28920.3   ← 用户等的时间
modelMs  p50= 7958.4 p95=21917.0 max=28718.2   ← 几乎等于全部
toolMs   p50=   58.3 p95=  365.9 max= 2394.8
tokens   in p50=6822  p95=16207 max=17804 ；out p50=80 p95=375
工具 p95：match_service=2214.9 search_knowledge=200.5 assign_ticket=128.7 ask_user=71.5
判定：通过 8 / 软阈值 9 / 红线 4 → exit=2
```

读法（这一轮的价值在问题清单，不在分数）：

- **延迟全是模型的时间**：modelMs ≈ wallMs（差在毫秒级），工具最慢的是 `match_service` 的
  一次 embedding 往返。想压 P95，方向是少一轮模型调用，不是优化工具。
- **`ask_user` 抢轮是这批失败的主因**：tj-001/002/003/007/019/021 第一步都调了 `ask_user`，
  而金标认为原话里已有逐字证据、不该追问（提示词规则 1）。后果分两种，报告里也分开写：
  一种是超轮次预算（tj-004 5 轮 / 上限 4），一种是案例只给了两条消息，模型多问一轮就把
  "点头"那条花掉了，`create_ticket` 停在确认闸上 —— 这是**夹具的形状**，不是丢单，
  所以 `ticket_created` 的 detail 会写明"停在确认闸：还差一次用户点头"。
- **红线 `ask_via_tool_only` 在 3/21 上响**（tj-011/015/020）：模型一个工具都没调，
  却在正文里直接发问。按提示词规则 2 这是致命的（不走过 `ask_user` 的追问不计数，
  同一个问题会问两遍）。这条判得严是故意的，但它确实只该管"没走工具的发问"——
  早先版本无条件判问句，会把规则 8 允许的"`propose_appointments` 之后把时刻念给用户挑"误伤。
- **`confirm_parked` 在 tj-020/021 上响**：金标要求这两条停在确认闸上，模型压根没递出草案。
  与上面同源（追问占掉了轮次），但它是另一条独立证据。

n=21 刚过 P95 的最低门槛（向上最近秩下它是第 20 名），要拿 P95 做前后对比得 `--repeat` 上去；
`metrics.flip_rate` 已经能算 N 次跑的翻转率，只是这一轮没跑多遍。

## 9. 还没做的

- `assign` 轴仍是硬退出码 2：它的方向不能由代码判，等 45 条人工盲标（`docs/P2_KERNEL.md` 那道口）。
  代码不代填金标。
- 评委的绝对分没有 κ 支撑；`--judge` 的输出只能读趋势。
- `TracingMiddleware` / OTLP 未接（§1 留了缝的位置）。
- 轨迹轴的真机跑一次 12 分钟，没进 `make eval`，所以红线目前只在本机手动跑时响。
- 运行时轴也没进 `make eval`（理由见 §5），所以它挂了什么得单独 `make eval-runtime` 才知道；
  而那份数据只有 **6 份有效 reply**，P95 按定义撑不起来。要拿它做前后对比，
  要么先攒真机会话，要么用 `--repeat` 把轨迹轴的样本推过 20。
