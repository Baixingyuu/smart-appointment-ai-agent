# P5 前端接缝：CopilotKit 官方 UI 只走一个 `POST /ag-ui`

复现：`make probe-p5`（A+B：离线账面 + 框架原生 HITL）、`make probe-p5-agui`（A+C：只走前端看到的那一个端点）、
`make serve ARGS="--port 8011 --keep-index"` + `make web`（浏览器实跑）。
A 段加 `--offline` 不起模型；B、C 互斥，各花一次真机钱。
离线断言：`tests/test_agui_bridge.py` 10 条、全仓 95 条全过 —— 这是 P5 收尾时的账面，
2026-09-27 核过已涨到 12 条 / 全仓 263 条。

## 形状对不上，所以有一层桥

2.0.8 里唯一的一等前端协议就是 AG-UI（框架自带 `AGUIProtocolMiddleware`，CopilotKit 是它官方接的前端）。
按这个选定了 CopilotKit 官方 UI，**覆盖此前"零前端自研代码"的裁决 —— 只覆盖这条工作流**。

自研只剩 `src/helpdesk/agui_bridge.py`（504 行）。它存在的唯一理由：

| AG-UI 客户端 | AgentScope 2.0.8 |
|---|---|
| 一次 `POST`，在**同一条响应**上读到终点 | `POST /chat/` 是 fire-and-forget（`app/_router/_chat.py` 头部明写不再返回 SSE） |
| 终点帧决定 success / interrupt | 事件在另一条长连接 `GET /sessions/{id}/stream` |

桥只做四件事：threadId↔session_id（名字 `agui:<threadId>`）、把一句话或一次表态变成 `POST /chat` 的 body、
常驻订阅该 session 的 SSE 并转发、把框架只能表达成 `CUSTOM/require_user_confirm` 的待确认落成
`RUN_FINISHED.outcome={type:"interrupt"}`。它只走公开 HTTP 路由（连 `X-User-ID` 都自己带），
对服务端的了解不超过一个浏览器 —— 代价是它得回环访问自己。

## B 段：P3 手写的确认桥在托管路径上可以整块删掉

```
✓ B4 用 UserConfirmResultEvent 恢复（没有走任何自研桥）
✓ B5 落库 #1 指派=101 in_recall=True
```

park/唤醒是框架的原生能力，不需要我重新实现一遍。C 段再证明同一件事在"前端只有 `/ag-ui`"的前提下也成立：

```
✓ C0 重复启动幂等（10 credential / 10 agent 不变，session 复用 0f91aa2b）
✓ C1 park → interrupt（1 条 create_ticket，本轮 7 帧）
✓ C2 resume[] → 同一发 POST 里跑到终点（81 帧）
✓ C3 落库 #1 incident/P0 指派=101 缺=无
· C 段工具序列：['find_open_ticket','match_service','ask_user','create_ticket','match_service','assign_ticket']
```

**2026-09-27 重跑（"助手不直接派单"之后的两条新判据）**：B 段全绿，
`✓ B5 落库 #1，未指派（指派由人在系统外做）`、`· B6 真机工具调用序列：['create_ticket','match_service']`；
禁令在托管路径上是真的守住了（B、C 两段的序列里都没有 `assign_ticket`）。
**2026-09-27 二次口径（推荐多个 + 闭环落指派）**：`assign_ticket` 回到模型面并走确认闸
（`permission=_ASK`），所以 B/C 段的工具序列里会重新出现 `assign_ticket`，且它落库前先 park 成
interrupt 卡片等人确认。上面"序列里都没有 assign_ticket"是上一版口径（人在系统外指派）的读数。
C 段这次**退出码 1**，但不是桥坏了：模型连三次把 `create_ticket` 的参数填错
（漏 `category`、`priority` 填了 `"high"`、多出一个幻觉参数 `knowledge_bases`），工单簿于是空的。
这正是 P3 记下的 qwen3 实跑毛病，也说明**这一腿本来就是偶发的**（上面那段 81 帧的历史读数一次成功过一次翻车过），
所以失败信息现在会自带工具返回正文，免得把"模型填错参数"读成"桥把落库弄丢了"。
顺带修了一处早就断了的地方：A5 构造 `AguiBridge` 缺 `context_config`（阈值那轮加进签名的），整个探针在此之前连 `--offline` 都起不来。

## 真机撞出来的九条接缝

每条要么有线下断言，要么有帧证据（`data/p5_agui_c_events.jsonl`），没有一条是推出来的。

1. **park 不发终点帧。** 框架停在待确认时最后一条是 `CUSTOM/require_user_confirm`，之后这条流就此沉默。
   AG-UI 的 HTTP 响应必须有终点，所以 interrupt 语义由桥落地，不是等上游。
2. **`EventType` 是全大写 `StrEnum`。** 真机 422：`input` 是 `Msg | … | UserConfirmResultEvent` 的联合，
   判别字段取 `EventType.USER_CONFIRM_RESULT` 的值 `"USER_CONFIRM_RESULT"`；小写的 `"user_confirm_result"`
   只是它在 AG-UI 侧的 **CUSTOM 事件名**。这个联合不是 discriminated，失败会报成 `Msg` 的错，指错方向。
   **更要紧的是量具**：A5 当时用桥自己写的那个字面量验桥自己，于是线下全绿、线上 422。判据已换成服务端的真 schema（`ChatRequest`）。
3. **出流的 `tool_call` 不能原样递回去。** `RequireUserConfirmEvent` 出流走 `model_dump(exclude_none=True)`，
   `suggested_rules` 里那条因此丢了必填的 `rule_content`。`confirmable_tool_call()` 剥掉它，
   代价是"以后都允许这个工具"这类建议规则过不了 AG-UI 这一道，只剩纯 allow/deny。
4. **`RUN_FINISHED` 不等于 session 空了。** 真机 409：终点帧发出去之后，同一条 run task 还在善后 ——
   写消息、写 AgentState，然后 `_auto_name_session` **再调一次模型**给 session 起标题（每 session 一次，
   所以撞上的一定是首轮）。`ChatRunRegistry.spawn` 见未结束的 task 就拒同 session 的第二次触发。
   桥等它（`TRIGGER_RETRY_*`），且**只对这一种 409**，别的冲突照报。
5. **本轮第一帧必须是 `RUN_STARTED`。** 常驻订阅会在两轮之间掉进上一轮的善后帧，`@ag-ui/client` 的
   `verifyEvents` 对第一帧直接抛 `INCOMPLETE_STREAM / First event must be 'RUN_STARTED'`。
   实测每一轮（含 resume 那一轮）都以 `RUN_STARTED` 开头，所以开始前的帧一律丢掉，并给这个等待单独设界（`START_TIMEOUT`）。
6. **桥自己出异常也得给终点。** 一次回环被拦成 502 时生成器裸着断掉，CopilotKit 只能报
   "Run ended without emitting a terminal event"，用户看到的是"话发出去了但什么都没发生"。
7. **两名制。** POST 的响应叫 `credential_id`/`agent_id`，GET 列表里的同一条记录叫 `id`；
   `AgentRecord.name` 藏在 `data` 里，session 的名字藏在 `config.name` 里。按顶层读，第二次启动就堆重复记录
   （`AgentRecord.id != AgentRecord.data.id` 也算一名制没讲清楚）。C0 用"启动两次 + 前后清点"钉住。
8. **回环不能被系统代理接管。** httpx 默认 `trust_env=True`，代理来自 `urllib.request.getproxies()`，
   macOS 下它读系统配置、**不认系统自带的那份 bypass 名单**：本机一旦开着 Clash 一类代理，这个进程里每一个
   httpx 客户端 —— 调 `ollama:11434` 的模型客户端、调自己 `/credential/` 的桥 —— 都会收到一个空正文 502，
   而 uvicorn 侧连请求都没见过。修在 `service.py` 的 `NO_PROXY` 默认值 + 桥的 `trust_env=False`。
9. **常驻 SSE 不能有读超时。** 客户端那个 30s 总超时套在长连接上，模型想超过 30s 就把订阅读死，
   下一轮再也没有帧（真机 16:24:47）。改成 `read=None` + 发现 pump 死了重挂。
   副作用记在这里：桥自连自，uvicorn 的优雅停机会一直等这条连接，而取消订阅的 `on_shutdown` 在它后面 ——
   SIGTERM 永远落不下来（真机 25s 未退，向量库的锁一直被握着）。已用 `timeout_graceful_shutdown=5` 封顶。

## 工具面（A2）

托管路径的 workspace 会附送 6 个文件/命令工具（`Bash` `Edit` `Glob` `Grep` `Read` `Write`），
`ToolSurfaceMiddleware` 把它们挡在每次模型调用的 schema 之外 —— **挂进 Toolkit ≠ 递进上下文**，
这条在 Go 侧没有对位，是托管形态自己带出来的。业务工具 5 个：`ask_user` `assign_ticket` `create_ticket` `find_open_ticket` `match_service`。
（这一行是当时的快照：2026-09-27 起 `assign_ticket` 已不递交给模型，见 `docs/P3_AGENT.md` 顶部那条口径裁决。）
A1 装配出 68 条路由，A4 证明 sqlite + `InMemoryMessageBus` + `LocalWorkspaceManager` 不需要 Redis。

## 浏览器实跑（qwen3:8b temp=0）

一句"研发楼三楼的 VPN 网关今早八点起全面故障，大约四十人连不上内网……"：
卡片显示 `create_ticket` 的标题/描述/`incident`/`P0`/缺=无 → 点确认执行 →
"工单 #1 已成功指派给 103 陈磊（运维组）"。全程前端只看见 `POST /ag-ui` 这一个端点。

两条如实记录（模型质量，不是接缝）：

- `ask_user` 会把五个模板问题一次问出口（"请问是哪个系统或服务出现了问题？""计划的变更时间窗口是什么时候？"），
  即使用户第一句已经给了系统、症状、影响面和紧急度 —— 追问轴该在 P4 的四轴里量化。
- C 段第一轮抓到 qwen3 把工具调用当正文漏出来：`TEXT_MESSAGE_CONTENT` 里逐 token 吐出一段
  "尖括号包住的 `tool_call` + name/arguments JSON"的伪标记，框架照常收尾、前端照原样显示。
  同一轮补答之后（探针的第二轮）就正常 park，所以这是采样噪声而不是接缝问题
  （temp=0 也挡不住，与 P1 那条"temp=0 仍不可复现"对得上）。

## 已知边界（没做的，都写清楚）

- `pending` 与 `_threads` 在进程内，与 `InMemoryMessageBus` 同寿命：服务重启后旧卡片答不了（interruptId 找不到人）。
- 历史所有权是双份的：客户端 `messages[]` 与服务端 AgentState 各有一份，桥信服务端，只递最新一句 user message。
- **"用新话题回答待确认"没做**（AG-UI 的 UserInterruptEvent 语义）：官方 UI 在表态前锁住输入，这条路上不会发生；
  但直连 `/ag-ui` 的不可信客户端能把 session 一直吊在 park 上。
- 一张卡片一个 interrupt：`_interrupts()` 支持多条，CopilotKit 的 `useInterrupt` 只把 primary 交给 render。
- 无鉴权：`X-User-ID` 固定 `ops`，桥自己带；这条路径只适合本机单人测试。
- CopilotKit 的 dev Inspector 浮层会盖在页面上（`Hide Inspector for a day` 可关），不是我们的 UI 代码。
