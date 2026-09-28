"""判分层：把 py-openjudge 的 grader 指到本地 ollama 上。

分工是这样切的：`gates.py` 管**有没有穿闸**（红线，纯代码），这里管**答得好不好**
（无金标可断言的那些维度）。`ToolCallStepSequenceMatchGrader` 是个例外 —— 它是
纯规则打分（步序 F1/recall），不叫模型，所以轨迹分放在离线轴里跑也成立。

评委模型默认跟被评模型同一个（`qwen3:8b`）。这不是中性选择：同一个模型给自己打分有
自偏好，所以报告必须写 `judgeModel`，而且分数只能看趋势不能当达标线 —— 想要可用的
绝对分，得先拿一批人工标注算一致率（κ），这一步我们没做，报告里也就明写着没做。

评委走 OpenAI 兼容端点（ollama 的 `/v1`），而本机的系统代理会劫持 127.0.0.1 回空正文
502（`probes` 那边踩过）—— 靠 `helpdesk/__init__.py` 的 NO_PROXY 护栏，这里不重复装。
"""
from __future__ import annotations

import os
from typing import Any

#: 评委与被评者是同一个模型这件事本身就是要报出来的事实，不是实现细节。
JUDGE_MODEL = os.environ.get("HELPDESK_JUDGE_MODEL", os.environ.get("HELPDESK_CHAT_MODEL", "qwen3:8b"))
OLLAMA_BASE_URL = os.environ.get(
    "HELPDESK_JUDGE_BASE_URL",
    f"{(os.environ.get('OLLAMA_HOST') or 'http://127.0.0.1:11434').rstrip('/')}/v1",
)
LANGUAGE = os.environ.get("HELPDESK_JUDGE_LANGUAGE", "zh")


def judge_meta() -> dict[str, Any]:
    return {"judgeModel": JUDGE_MODEL, "judgeBase": OLLAMA_BASE_URL, "judgeLanguage": LANGUAGE}


def make_judge_model() -> Any:
    from openjudge.models import OpenAIChatModel

    return OpenAIChatModel(
        model=JUDGE_MODEL,
        api_key=os.environ.get("OLLAMA_API_KEY", "ollama"),
        base_url=OLLAMA_BASE_URL,
        timeout=float(os.environ.get("HELPDESK_JUDGE_TIMEOUT", "120")),
    )


async def sequence_score(
    messages: list[dict[str, Any]],
    reference_steps: list[list[dict[str, Any]]],
    *,
    strict: bool = False,
    jaccard: bool = False,
    metric: str = "recall",
) -> float | None:
    """轨迹分：纯规则，不调模型。reference_steps 按轮分组，每轮一组 {name, arguments}。"""
    from openjudge.graders.agent import ToolCallStepSequenceMatchGrader

    grader = ToolCallStepSequenceMatchGrader(
        strict_mode=strict,
        use_jaccard_similarity=jaccard,
        metric_type=metric,
    )
    result = await grader.aevaluate(
        messages=messages,
        reference_tool_calls=reference_steps,
    )
    if getattr(result, "error", None):
        return None
    return float(result.score)


#: 无金标的质量维度 → grader 名。维度由调用方挑（`run.py` 默认只判 relevance）：
#: 一个维度 = 每条案例一次评委模型往返，这不免费，所以不要一次全开。
LLM_GRADERS = ("relevance", "correctness", "tool_selection", "trajectory")


async def judge_trace(
    *,
    query: str,
    response: str,
    messages: list[dict[str, Any]],
    tool_definitions: list[dict[str, Any]] | None = None,
    dims: tuple[str, ...] = ("relevance",),
    context: str = "",
    reference_response: str = "",
) -> list[dict[str, Any]]:
    """按维度逐个打分。grader 自己会回 GraderError，这里把它转成 `{"error": ...}`。"""
    from openjudge.graders.agent import ToolSelectionGrader, TrajectoryAccuracyGrader
    from openjudge.graders.common import CorrectnessGrader, RelevanceGrader

    model = make_judge_model()
    built = {
        "relevance": lambda: RelevanceGrader(model=model, language=LANGUAGE),
        "correctness": lambda: CorrectnessGrader(model=model, language=LANGUAGE),
        "tool_selection": lambda: ToolSelectionGrader(model=model, language=LANGUAGE),
        "trajectory": lambda: TrajectoryAccuracyGrader(model=model, language=LANGUAGE),
    }
    out: list[dict[str, Any]] = []
    for dim in dims:
        if dim not in built:
            out.append({"grader": dim, "error": f"没有这个维度：{dim}"})
            continue
        grader = built[dim]()
        if dim in {"relevance", "correctness"}:
            score = await grader.aevaluate(
                query=query,
                response=response,
                context=context,
                reference_response=reference_response,
            )
        elif dim == "tool_selection":
            calls = [c for m in messages for c in (m.get("tool_calls") or [])]
            score = await grader.aevaluate(
                query=query,
                tool_definitions=tool_definitions or [],
                tool_calls=calls,
            )
        else:
            score = await grader.aevaluate(messages=messages)
        row: dict[str, Any] = {"grader": dim, "score": getattr(score, "score", None)}
        if getattr(score, "error", None):
            row["error"] = str(score.error)
        else:
            row["reason"] = getattr(score, "reason", "")
        out.append(row)
    return out


__all__ = [
    "JUDGE_MODEL",
    "LLM_GRADERS",
    "judge_meta",
    "judge_trace",
    "make_judge_model",
    "sequence_score",
]
