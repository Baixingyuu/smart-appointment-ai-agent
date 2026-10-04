"""推理延迟基准：微调 0.6B LoRA（本地 MPS，进程内）vs 8B（Ollama HTTP）。

对比口径：
- 同一批 eval 文本、温度 0、贪心、输出上限 32 token、batch=1（线上路由就是逐条调用）
- 0.6B 测两种 prompt：short（微调用的精简版，~100 tok）与 full（线上 INTENT_PROMPT，~400 tok），
  用来区分"模型变小"和"prompt 变短"各自的贡献
- 分别报告 prefill(TTFT) 与 decode 速度，避免只看总量看不出瓶颈
"""
from __future__ import annotations

import argparse
import json
import re
import statistics
import sys
import time
from pathlib import Path

import torch
from peft import PeftModel
from transformers import AutoModelForCausalLM, AutoTokenizer

HERE = Path(__file__).resolve().parent
BASE = HERE / "qwen3-0.6b"
PROJ = HERE.parent

sys.path.insert(0, str(HERE))
from build_dataset import INTENT_PROMPT as SHORT_PROMPT  # noqa: E402

_full_src = (PROJ / "src/helpdesk/intent.py").read_text(encoding="utf-8")
FULL_PROMPT = re.search(r'INTENT_PROMPT = """(.*?)"""', _full_src, re.S).group(1)


def load_texts(n: int) -> list[str]:
    data = json.loads((PROJ / "eval/datasets/intent.json").read_text(encoding="utf-8"))
    return [c["text"] for c in data["cases"][:n]]


def stats(ms: list[float]) -> dict:
    s = sorted(ms)
    return {
        "mean": round(statistics.mean(ms), 1),
        "p50": round(s[len(s) // 2], 1),
        "p95": round(s[min(len(s) - 1, int(len(s) * 0.95))], 1),
        "min": round(s[0], 1),
        "max": round(s[-1], 1),
    }


@torch.no_grad()
def bench_lora(prompt: str, texts: list[str], n_warmup: int = 3, dtype: str = "fp32", merge: bool = False) -> dict:
    device = torch.device("mps")
    td = torch.float16 if dtype == "fp16" else torch.float32
    tok = AutoTokenizer.from_pretrained(BASE)
    tok.pad_token = tok.pad_token or "<|endoftext|>"
    tok.padding_side = "left"
    model = AutoModelForCausalLM.from_pretrained(BASE, dtype=td)
    model = PeftModel.from_pretrained(model, str(HERE / "lora_out/adapter"))
    if merge:  # 合并 LoRA 到基座，去掉 adapter 前向开销（部署形态）
        model = model.merge_and_unload().to(td)
    model = model.to(device).eval()

    def run_one(text: str) -> tuple[float, float, int, int]:
        msgs = [{"role": "system", "content": prompt}, {"role": "user", "content": text}]
        rendered = tok.apply_chat_template(msgs, tokenize=False, add_generation_prompt=True, enable_thinking=False)
        enc = tok(rendered, return_tensors="pt", add_special_tokens=False).to(device)
        t0 = time.perf_counter()
        out = model.generate(**enc, max_new_tokens=32, do_sample=False, pad_token_id=tok.pad_token_id)
        t1 = time.perf_counter()
        n_out = out.shape[1] - enc["input_ids"].shape[1]
        return (t1 - t0) * 1000, enc["input_ids"].shape[1], n_out

    for t in texts[:n_warmup]:
        run_one(t)
    lat, p_tok, o_tok = [], [], []
    for t in texts:
        ms, np_, no_ = run_one(t)
        lat.append(ms); p_tok.append(np_); o_tok.append(no_)
    return {
        "where": "0.6B LoRA (本地 MPS, 进程内)",
        "prompt_tokens_avg": round(statistics.mean(p_tok), 0),
        "output_tokens_avg": round(statistics.mean(o_tok), 1),
        "latency_ms": stats(lat),
        "n": len(lat),
    }


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--which", choices=["short", "full"], required=True)
    ap.add_argument("--n", type=int, default=30)
    ap.add_argument("--dtype", choices=["fp16", "fp32"], default="fp32")
    ap.add_argument("--merge", action="store_true")
    ap.add_argument("--out", default="")
    a = ap.parse_args()
    prompt = SHORT_PROMPT if a.which == "short" else FULL_PROMPT
    print(f"prompt variant: {a.which}, dtype: {a.dtype}, merge: {a.merge}", flush=True)
    res = bench_lora(prompt, load_texts(a.n), dtype=a.dtype, merge=a.merge)
    res["prompt_variant"] = a.which
    res["model"] = (f"Qwen3-0.6B + LoRA(r16), {a.dtype}{', merged' if a.merge else ''}, "
                    f"MPS, batch=1, greedy, max_new=32")
    print(json.dumps(res, ensure_ascii=False, indent=2))
    if a.out:
        (HERE / a.out).write_text(json.dumps(res, ensure_ascii=False, indent=2), encoding="utf-8")


if __name__ == "__main__":
    main()
