"""脚本模型：把 canned 响应喂给框架，用来观测框架自己的行为。

存在的理由有两条：
1. 评测轴要可复现——真实模型每次都不同，轨迹轴就无法断言工具序列；
2. P0 探针要问的是"框架在给定模型输出下怎么做"（轮次怎么记、park 怎么醒），
   这必须精确控制输出才能观测，用真模型问不出确定答案。

`ollama_ids=True` 让脚本复刻 OllamaChatModel 的 id 合成规则
（`model/_ollama/_model.py:324`：`f"{idx}_{name}"`，只在一次模型响应内唯一），
这样离线就能复现真机才会撞的 id 冲突（probes/p3_tool_call_id_collision.py）。

跑法：.venv/bin/python probes/p0_round_accounting.py
"""
from __future__ import annotations

import json
from typing import Any, AsyncGenerator

from pydantic import BaseModel
from agentscope.credential import OllamaCredential
from agentscope.message import TextBlock, ToolCallBlock
from agentscope.model import ChatModelBase, ChatResponse, ChatUsage
from agentscope.model._model_response import StructuredResponse
from agentscope.tool import ToolChoice


class ScriptExhausted(AssertionError):
    """模型被叫的次数超过脚本长度 —— 说明框架多跑了一轮。"""


class Turn:
    """One canned model response."""

    def __init__(
        self,
        text: str | None = None,
        tool_calls: list[tuple[str, dict[str, Any]]] | None = None,
        input_tokens: int = 100,
        output_tokens: int = 20,
        id_prefix: str = "t",
        ollama_ids: bool = False,
    ) -> None:
        self.text = text
        self.tool_calls = tool_calls or []
        self.input_tokens = input_tokens
        self.output_tokens = output_tokens
        self.id_prefix = id_prefix
        self.ollama_ids = ollama_ids

    @property
    def n_calls(self) -> int:
        return len(self.tool_calls)

    def call_id(self, index: int, turn_index: int, name: str) -> str:
        if self.ollama_ids:
            return f"{index}_{name}"
        return f"{self.id_prefix}{turn_index}-{index}"


class ScriptedChatModel(ChatModelBase):
    """A ChatModelBase that replays a fixed list of Turn objects.

    Records every request it receives so a probe/eval can assert on what the
    framework actually sent (tool schemas, tool_choice, message count, prompt texts).
    """

    class Parameters(BaseModel):
        """No real parameters — the responses are canned."""

    def __init__(
        self,
        turns: list[Turn],
        model: str = "scripted",
        context_size: int = 32768,
    ) -> None:
        super().__init__(
            credential=OllamaCredential(),
            model=model,
            parameters=self.Parameters(),
            stream=False,
            max_retries=0,
            context_size=context_size,
        )
        self.formatter = _StubFormatter()
        self.turns = turns
        self.requests: list[dict[str, Any]] = []

    @property
    def n_calls(self) -> int:
        return len(self.requests)

    async def generate_structured_output(
        self,
        messages: list[Any],
        structured_model: Any,
        **kwargs: Any,
    ) -> StructuredResponse:
        """脚本模型不做真分类，回一个固定结果，不消耗 turn。

        IntentRouter 的分类在离线/测试路径上会走到这里：真分类要调 ollama，而脚本
        模型没有那一步；让它照常走 `_call_api` 会白烧一个 turn，把脚本挤爆
        （`ScriptExhausted`）。默认判 incident 是"不丢单"的 fail-open 方向 ——
        工具面不裁剪，脚本照金标演，不受分类影响。
        """
        return StructuredResponse(
            content={"intent": "incident", "confidence": 0.9},
        )

    async def _call_api(
        self,
        model_name: str,
        messages: list[Any],
        tools: list[dict] | None = None,
        tool_choice: ToolChoice | None = None,
        **kwargs: Any,
    ) -> ChatResponse | AsyncGenerator[ChatResponse, None]:
        if len(self.requests) >= len(self.turns):
            raise ScriptExhausted(
                f"脚本只有 {len(self.turns)} 轮，框架叫了第 "
                f"{len(self.requests) + 1} 次模型（tool_choice={tool_choice}）",
            )
        index = len(self.requests)
        turn = self.turns[index]
        mode = getattr(tool_choice, "mode", tool_choice)
        calls = [] if mode == "none" else turn.tool_calls
        self.requests.append(
            {
                "n_messages": len(messages),
                "texts": [m.get_text_content() for m in messages],
                "tools": [t.get("function", {}).get("name") for t in (tools or [])],
                "tool_choice": repr(tool_choice),
                "dropped_tool_calls": turn.n_calls - len(calls),
            },
        )
        content: list[TextBlock | ToolCallBlock] = []
        if turn.text:
            content.append(TextBlock(type="text", text=turn.text))
        for i, (name, tool_input) in enumerate(calls):
            content.append(
                ToolCallBlock(
                    type="tool_call",
                    id=turn.call_id(i, index, name),
                    name=name,
                    input=json.dumps(tool_input, ensure_ascii=False),
                ),
            )
        return ChatResponse(
            content=content,
            is_last=True,
            usage=ChatUsage(
                input_tokens=turn.input_tokens,
                output_tokens=turn.output_tokens,
                time=0.0,
            ),
        )


class _StubFormatter:
    """The agent only reads ``supported_input_media_types`` off the formatter."""

    supported_input_media_types: list[str] = []

    def format(self, *args: Any, **kwargs: Any) -> list:  # pragma: no cover
        return []


__all__ = ["ScriptExhausted", "ScriptedChatModel", "Turn"]
