"""Chainlit 桥接前端 —— 把 helpdesk-agent 的 Go 后端会话 API 包装成对话 UI。

跑法（先启动后端，再启动前端）：
  1. 后端：在项目根目录执行 make serve（需 LLM_API_KEY）或 make serve-offline
  2. 前端：pip install -r ui/requirements.txt && chainlit run ui/app.py -w

数据流：本文件用 SSE（Accept: text/event-stream）调 Go 后端，把后端在 agent
运行过程中实时推送的事件渲染出来：
  - event: event（kind=tool）   → cl.Step 展开工具步骤（检索/查重/起草工单）
  - event: delta                → 回复文本逐字滚出（stream_token）
  - event: done（interrupted）  → 确认/取消按钮（cl.Action）
  - event: done（ticketId）     → 工单卡片
  - event: error                → 错误提示
"""
import json

import chainlit as cl
import httpx

BACKEND = "http://localhost:8080"

# 工具名与状态码 → 人话，避免把内部标识直接糊到界面上。
TOOL_LABELS = {
    "rag_search": "检索知识库",
    "ticket_find_open_by_topic": "查重复工单",
    "ticket_create_confirm": "起草工单",
}
TOOL_STATUS = {
    "completed": "完成",
    "awaiting_user_info": "信息不足，先追问",
    "awaiting_confirmation": "发起确认，等待回复",
    "failed": "执行失败",
}

# 派单流水线各阶段 → 展示名。
DISPATCH_STAGE_LABELS = {
    "resolve": "服务解析",
    "candidates": "候选排序",
    "stage2": "LLM 决策",
    "done": "派单完成",
}


@cl.on_chat_start
async def on_chat_start():
    """每个新聊天会话开始时，在 Go 后端创建一个会话并记住其 ID。"""
    async with httpx.AsyncClient(timeout=30) as client:
        resp = await client.post(
            f"{BACKEND}/api/conversations", json={"sourceChannel": "chainlit"}
        )
        resp.raise_for_status()
        conversation = resp.json()

    cl.user_session.set("conversation_id", conversation["id"])
    cl.user_session.set("request_seq", 0)
    await cl.Message(
        content="你好，我是企业技术支持助手。直接描述你遇到的问题，我可以解答或帮你登记工单。\n\n"
        "输入 `/eval` 可跑一遍派单评测并查看可视化报告（三轴 + 漏斗 + 失败样本审查）。"
    ).send()


@cl.on_message
async def on_message(message: cl.Message):
    content = message.content.strip()
    # /eval 是评测面板入口：跑一遍派单评测并可视化（三轴 + 漏斗 + 失败样本）。
    if content.startswith("/eval"):
        await render_eval_panel()
        return
    await process_turn(cl.user_session.get("conversation_id"), content)


@cl.action_callback("confirm_ticket")
async def on_confirm_ticket(action: cl.Action):
    # 用户点「确认建单」→ 把「确认」作为一条用户消息发给后端，走确认判定链路。
    await process_turn(int(action.payload.get("conversation_id")), "确认")


@cl.action_callback("cancel_ticket")
async def on_cancel_ticket(action: cl.Action):
    await process_turn(int(action.payload.get("conversation_id")), "取消")


async def process_turn(conversation_id: int, content: str):
    """发一条消息给后端，并消费 SSE 流渲染成 UI。on_message 与按钮回调共用。"""
    seq = cl.user_session.get("request_seq") + 1
    cl.user_session.set("request_seq", seq)
    request_id = f"chainlit-{conversation_id}-{seq}"

    # reply_msg 惰性创建：收到第一段 delta 才发气泡，避免纯工具回合留空气泡。
    reply_msg = None
    interrupted = False
    ticket_id = None
    error = None

    url = f"{BACKEND}/api/conversations/{conversation_id}/messages"
    payload = {"content": content, "requestId": request_id}

    async with httpx.AsyncClient(timeout=180) as client:
        async with client.stream(
            "POST", url, json=payload, headers={"Accept": "text/event-stream"}
        ) as resp:
            resp.raise_for_status()
            event_type = None
            async for line in resp.aiter_lines():
                if not line:
                    continue
                if line.startswith("event:"):
                    event_type = line.split(":", 1)[1].strip()
                    continue
                if line.startswith("data:"):
                    data = json.loads(line.split(":", 1)[1].strip())
                    if event_type == "delta":
                        if reply_msg is None:
                            reply_msg = cl.Message(content="")
                            await reply_msg.send()
                        await reply_msg.stream_token(data.get("text", ""))
                    elif event_type == "event":
                        await render_event(data)
                    elif event_type == "dispatch":
                        await render_dispatch(data)
                    elif event_type == "done":
                        interrupted = bool(data.get("interrupted"))
                        ticket_id = data.get("ticketId")
                    elif event_type == "error":
                        error = data.get("error")

    if error:
        await cl.Message(content=f"处理失败：{error}", author="system").send()
        return

    if interrupted:
        actions = [
            cl.Action(
                name="confirm_ticket",
                payload={"conversation_id": conversation_id},
                label="确认建单",
                icon="check",
            ),
            cl.Action(
                name="cancel_ticket",
                payload={"conversation_id": conversation_id},
                label="取消",
                icon="x",
            ),
        ]
        await cl.Message(content="**是否创建上述工单？**", actions=actions).send()

    if ticket_id:
        await show_ticket(ticket_id)


async def render_event(data: dict):
    """把一个回合事件渲染为 Chainlit 的 step（工具步骤逐步展开）。"""
    kind = data.get("kind")
    if kind == "tool":
        tool = data.get("tool", "")
        label = TOOL_LABELS.get(tool, tool)
        status = TOOL_STATUS.get(data.get("status", ""), data.get("status", ""))
        detail = data.get("detail", "")
        output = status
        if detail:
            output += f"：{detail}"
        async with cl.Step(name=label, type="tool") as step:
            step.output = output
    elif kind == "confirm":
        async with cl.Step(name="确认判定", type="tool") as step:
            step.output = data.get("detail", "")
    # kind == "round"（模型决策轮次）在 Chainlit 里忽略：工具 step 已足够表达进展。


async def render_eval_panel():
    """跑派单评测（v2 数据集）并把报告渲染成可视化面板 + 失败样本供事后审查。"""
    with cl.Step(name="派单评测", type="run") as step:
        step.output = "正在运行 assignment_v2 评测（离线，不注入 Stage 2）…"

    async with httpx.AsyncClient(timeout=180) as client:
        resp = await client.post(f"{BACKEND}/api/eval/assign-v2")
        resp.raise_for_status()
        rep = resp.json()

    # --- 总览：三轴 + 漏斗 ---
    lines = [f"**派单评测：{rep.get('dataset', '')}（共 {rep.get('total', 0)} 条）**", ""]
    lines.append("| 轴 | 通过 / 适用 | 准确率 |")
    lines.append("| --- | --- | --- |")
    for key, name in [("extraction", "服务解析"), ("weakness", "判弱"), ("sort", "排序")]:
        ax = rep.get(key) or {}
        lines.append(
            f"| {name} | {ax.get('pass', 0)} / {ax.get('applicable', 0)} "
            f"| {ax.get('accuracy', 0) * 100:.1f}% |"
        )
    funnel = rep.get("funnel") or {}
    lines.append("")
    lines.append(
        f"漏斗：Stage1 直出={funnel.get('stage1Direct', 0)}　"
        f"Stage2={funnel.get('stage2', 0)}　Stage3={funnel.get('stage3', 0)}"
    )
    await cl.Message(content="\n".join(lines), author="system").send()

    # --- 失败样本：逐条展示期望 vs 实际，供事后审查 ---
    failures = rep.get("failures") or []
    if not failures:
        await cl.Message(content="✅ 全部用例与金标一致。", author="system").send()
        return

    rows = ["**失败样本（期望 vs 实际）**", "", "| 用例 | 走哪段 | 不符的轴 | 详情 |", "| --- | --- | --- | --- |"]
    for f in failures:
        diffs = []
        if not f.get("extractPass"):
            diffs.append(f"抽取 {f.get('expectedTop1')}→{f.get('actualTop1')}")
        if not f.get("weakPass"):
            diffs.append(f"判弱 {f.get('expectedWeakness')}→{f.get('actualWeakness')}")
        if f.get("sortApplicable") and not f.get("sortPass"):
            diffs.append(f"排序 {f.get('expectedAssignee')}→{f.get('actualAssignee')}")
        detail = f.get("reason", "") or ""
        rows.append(
            f"| `{f.get('caseId')}` | {f.get('actualPath')} | {'、'.join(diffs) or '-'} | {detail[:80]} |"
        )
    await cl.Message(content="\n".join(rows), author="system").send()


async def render_dispatch(data: dict):
    """把派单流水线事件渲染为 Step：摘要 + 结构化明细表格。"""
    stage = data.get("stage", "")
    label = DISPATCH_STAGE_LABELS.get(stage, stage)
    table = render_dispatch_table(data)
    output = data.get("detail", "")
    if table:
        output += "\n\n" + table
    async with cl.Step(name=label, type="tool") as step:
        step.output = output


def render_dispatch_table(data: dict) -> str:
    """把派单事件里的结构化明细（服务命中 / 候选打分）渲染为 markdown 表格。"""
    blocks = []
    services = data.get("services") or []
    if services:
        rows = ["| 服务 | ID | 分数 |", "| --- | --- | --- |"]
        for s in services:
            rows.append(f"| {s.get('serviceName')} | {s.get('serviceId')} | {s.get('score')} |")
        blocks.append("**服务解析 top-K**\n\n" + "\n".join(rows))

    top = data.get("top") or []
    if top:
        rows = [
            "| 候选人 | 归属 | own | sim | avail | senior | recent | 总分 |",
            "| --- | --- | --- | --- | --- | --- | --- | --- |",
        ]
        for c in top:
            rows.append(
                f"| {c.get('name')} | {c.get('ownership')} | {c.get('own')} | {c.get('sim')} "
                f"| {c.get('avail')} | {c.get('senior')} | {c.get('recent')} | {c.get('total')} |"
            )
        blocks.append("**候选打分 top-3**\n\n" + "\n".join(rows))
    return "\n\n".join(blocks)


async def show_ticket(ticket_id: int):
    """建单成功后拉取工单详情，以摘要卡片形式展示。"""
    async with httpx.AsyncClient(timeout=30) as client:
        resp = await client.get(f"{BACKEND}/api/tickets/{ticket_id}")
        resp.raise_for_status()
    ticket = resp.json().get("ticket", {})

    lines = [
        f"**工单 #{ticket.get('id')} 已创建**",
        f"- 标题：{ticket.get('title')}",
        f"- 分类：{ticket.get('category')}　优先级：{ticket.get('priority')}",
    ]
    if ticket.get("assigneeName"):
        lines.append(f"- 处理人：{ticket.get('assigneeName')}")
    if ticket.get("missingInfo"):
        lines.append(f"- 待补充：{'、'.join(ticket.get('missingInfo'))}")
    await cl.Message(content="\n".join(lines), author="system").send()
