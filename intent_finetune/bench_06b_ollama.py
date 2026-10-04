"""微调后 0.6B（GGUF 导入 Ollama）延迟基准 —— 与 8B 完全同一运行时/调用协议。

用微调时的精简 prompt（该模型就是配这个 prompt 训练的），think=false、temp=0、输出上限 32。
"""
from __future__ import annotations

import json
import statistics
import sys
import time
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
PROJ = HERE.parent
HOST = "http://127.0.0.1:11434"
MODEL = "qwen3-0.6b-intent"
N = int(sys.argv[1]) if len(sys.argv) > 1 else 30
NONCE = len(sys.argv) > 2 and sys.argv[2] == "nonce"
import random as _random

sys.path.insert(0, str(HERE))
from build_dataset import INTENT_PROMPT as SHORT_PROMPT  # noqa: E402

data = json.loads((PROJ / "eval/datasets/intent.json").read_text(encoding="utf-8"))
texts = [c["text"] for c in data["cases"][:N]]


def run_one(text: str) -> dict:
    prefix = SHORT_PROMPT
    if NONCE:
        prefix = f"[{_random.randint(10**9, 10**10)}]\n" + SHORT_PROMPT
    payload = {
        "model": MODEL,
        "prompt": prefix + text,
        "stream": False,
        "think": False,
        "options": {"temperature": 0, "num_predict": 32, "seed": 13},
    }
    req = urllib.request.Request(
        f"{HOST}/api/generate", data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json"},
    )
    t0 = time.perf_counter()
    with urllib.request.urlopen(req, timeout=300) as r:
        resp = json.loads(r.read())
    return {
        "ms": (time.perf_counter() - t0) * 1000,
        "prompt_tokens": resp.get("prompt_eval_count", 0),
        "output_tokens": resp.get("eval_count", 0),
        "response": resp.get("response", "")[:60],
    }


for t in texts[:2]:
    run_one(t)

rows = [run_one(t) for t in texts]
lat = [r["ms"] for r in rows]
s = sorted(lat)
out = {
    "model": f"{MODEL} (GGUF f16 导入 Ollama, think=false, temp=0, max_new=32)",
    "where": "0.6B 微调模型 (Ollama HTTP 进程间)" + ("，无前缀缓存(nonce)" if NONCE else "，前缀缓存生效"),
    "prompt_tokens_avg": round(statistics.mean(r["prompt_tokens"] for r in rows), 0),
    "output_tokens_avg": round(statistics.mean(r["output_tokens"] for r in rows), 1),
    "latency_ms": {
        "mean": round(statistics.mean(lat), 1),
        "p50": round(s[len(s) // 2], 1),
        "p95": round(s[min(len(s) - 1, int(len(s) * 0.95))], 1),
        "min": round(s[0], 1), "max": round(s[-1], 1),
    },
    "n": N,
    "samples": rows[:3],
}
print(json.dumps(out, ensure_ascii=False, indent=2))
(HERE / f"bench_06b_ollama{'_nonce' if NONCE else ''}.json").write_text(
    json.dumps(out, ensure_ascii=False, indent=2), encoding="utf-8")
