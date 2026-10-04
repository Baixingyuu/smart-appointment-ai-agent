"""意图 LoRA 模型评测：held-out eval/datasets/intent.json（180 条）。

- 评测协议与线上 classify_intent 一致：同 INTENT_PROMPT + Qwen3 非 thinking 渲染
- 输出与 eval/reports/intent.json 同构：accuracy / perLabel P/R / incident_recall gate
- 附带零-shot Qwen3-0.6B 对照（同一协议），展示微调增益
"""
from __future__ import annotations

import json
import re
import sys
from collections import Counter
from pathlib import Path

import torch
from peft import PeftModel
from transformers import AutoModelForCausalLM, AutoTokenizer

HERE = Path(__file__).resolve().parent
BASE = HERE / "qwen3-0.6b"
INTENTS = ["knowledge", "incident", "handoff", "out_of_scope"]
#: 4 类口径：评测集里的 chitchat 归入 out_of_scope（与训练目标一致）
LABEL_MERGE = {"chitchat": "out_of_scope"}


def merge_label(lab: str) -> str:
    return LABEL_MERGE.get(lab, lab)

sys.path.insert(0, str(HERE))
from build_dataset import INTENT_PROMPT  # noqa: E402  与训练/线上同源


def build_prompt(tok, text: str) -> str:
    return tok.apply_chat_template(
        [{"role": "system", "content": INTENT_PROMPT}, {"role": "user", "content": text}],
        tokenize=False, add_generation_prompt=True, enable_thinking=False,
    )


@torch.no_grad()
def predict_batch(model, tok, texts: list[str], device, batch_size: int = 16) -> list[str]:
    """decoder-only 批量生成必须 left padding，否则续写起点落在 pad 上会生成垃圾。"""
    preds: list[str] = []
    model.eval()
    tok.padding_side = "left"
    for i in range(0, len(texts), batch_size):
        chunk = texts[i : i + batch_size]
        prompts = [build_prompt(tok, t) for t in chunk]
        enc = tok(prompts, return_tensors="pt", padding=True, add_special_tokens=False).to(device)
        out = model.generate(
            **enc, max_new_tokens=32, do_sample=False,
            pad_token_id=tok.pad_token_id or tok.eos_token_id,
        )
        for j in range(len(chunk)):
            new = out[j][enc["input_ids"].shape[1]:]
            s = tok.decode(new, skip_special_tokens=True)
            preds.append(s)
    return preds


def parse_pred(raw: str) -> str:
    m = re.search(r"\{[^{}]*\}", raw, re.S)
    if m:
        try:
            return json.loads(m.group(0)).get("intent", "")
        except Exception:
            pass
    for lab in INTENTS:  # 容错：退化成词面匹配
        if lab in raw:
            return lab
    return ""


def score(cases: list[dict], preds: list[str]) -> dict:
    gold = [merge_label(c["intent"]) for c in cases]
    per = {}
    for lab in INTENTS:
        tp = sum(1 for g, p in zip(gold, preds) if g == lab and p == lab)
        fp = sum(1 for g, p in zip(gold, preds) if g != lab and p == lab)
        fn = sum(1 for g, p in zip(gold, preds) if g == lab and p != lab)
        per[lab] = {
            "precision": round(tp / (tp + fp), 4) if tp + fp else None,
            "recall": round(tp / (tp + fn), 4) if tp + fn else None,
            "gold": tp + fn, "pred": tp + fp, "correct": tp,
        }
    acc = sum(1 for g, p in zip(gold, preds) if g == p) / len(gold)
    conf = Counter((g, p) for g, p in zip(gold, preds))
    return {"accuracy": round(acc, 4), "n": len(gold), "perLabel": per, "confusion": {f"{g}->{p}": c for (g, p), c in conf.items() if g != p}}


def main() -> None:
    import argparse
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default=str(BASE))
    ap.add_argument("--adapter", default=str(HERE / "lora_out/adapter"))
    ap.add_argument("--out", default=str(HERE / "eval_report.json"))
    ap.add_argument("--skip-zero-shot", action="store_true")
    ap.add_argument("--dtype", choices=["fp16", "fp32"], default="fp32")
    a = ap.parse_args()
    adapter, base_path = a.adapter, a.base
    td = torch.float16 if a.dtype == "fp16" else torch.float32
    device = torch.device("mps" if torch.backends.mps.is_available() else "cpu")

    data = json.loads((HERE.parent / "eval/datasets/intent.json").read_text(encoding="utf-8"))
    cases = data["cases"]
    texts = [c["text"] for c in cases]
    tok = AutoTokenizer.from_pretrained(base_path)
    if tok.pad_token is None:
        tok.pad_token = "<|endoftext|>"

    report: dict = {"n": len(cases), "protocol": "4类（chitchat 并入 out_of_scope）",
                    "base": base_path, "adapter": adapter}

    # 1) 零-shot 基线
    if not a.skip_zero_shot:
        print("evaluating zero-shot base ...")
        base = AutoModelForCausalLM.from_pretrained(base_path, dtype=td).to(device)
        zs = predict_batch(base, tok, texts, device)
        report["zero_shot"] = score(cases, [parse_pred(r) for r in zs])
        del base

    # 2) LoRA 微调后
    print("evaluating LoRA adapter ...")
    model = AutoModelForCausalLM.from_pretrained(base_path, dtype=td)
    model = PeftModel.from_pretrained(model, adapter).to(device)
    ft = predict_batch(model, tok, texts, device)
    report["lora"] = score(cases, [parse_pred(r) for r in ft])
    n_empty = sum(1 for r in ft if not parse_pred(r))
    print(f"lora empty/unparsable outputs: {n_empty}/{len(ft)}")
    print("raw samples:")
    for c, r in list(zip(cases, ft))[:5]:
        print(f"  [{c['intent']}] {c['text'][:24]!r} -> {r[:60]!r}")

    ir = report["lora"]["perLabel"]["incident"]["recall"]
    report["checks"] = [{"name": "incident_recall", "ok": ir >= 0.90, "fatal": False,
                          "detail": f"incident 召回 {ir:.1%}，低于 90% 会漏建单"}]
    report["adapter"] = adapter

    out_path = Path(a.out)
    out_path.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")

    if "zero_shot" in report:
        print(f"\nzero-shot acc = {report['zero_shot']['accuracy']}")
    print(f"lora      acc = {report['lora']['accuracy']}")
    for lab in INTENTS:
        p = report["lora"]["perLabel"][lab]
        print(f"  {lab:<13} P={p['precision']} R={p['recall']}")
    print("incident gate:", report["checks"][0]["detail"], "->", "OK" if report["checks"][0]["ok"] else "FAIL")
    print("confusion (lora):", json.dumps(report["lora"]["confusion"], ensure_ascii=False))
    print("saved:", out_path)


if __name__ == "__main__":
    main()
