"""8B（Ollama, qwen3:8b, Q4_K_M GGUF）意图分类延迟基准。

走线上同一条路径：OllamaChatModel + generate_structured_output 等价调用（think=false,
temperature=0, 输出上限 32 token），HTTP 进程间往返计入延迟（线上也是这么调的）。
"""
from __future__ import annotations

import json
import re
import statistics
import sys
import time
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
PROJ = HERE.parent
HOST = "http://127.0.0.1:11434"
MODEL = "qwen3:8b"
N = int(sys.argv[1]) if len(sys.argv) > 1 else 30
#: nonce 模式：在 prompt 最前面插入随机串，破坏 Ollama 前缀 KV 缓存，测无缓存最坏情况
NONCE = len(sys.argv) > 2 and sys.argv[2] == "nonce"
import random as _random

_full_src = (PROJ / "src/helpdesk/intent.py").read_text(encoding="utf-8")
FULL_PROMPT = re.search(r'INTENT_PROMPT = """(.*?)"""', _full_src, re.S).group(1)

data = json.loads((PROJ / "eval/datasets/intent.json").read_text(encoding="utf-8"))
texts = [c["text"] for c in data["cases"][:N]]


def run_one(text: str) -> dict:
    prefix = FULL_PROMPT
    if NONCE:
        prefix = f"[{_random.randint(10**9, 10**10)}]\n" + FULL_PROMPT
    payload = {
        "model": MODEL,
        "prompt": prefix + text,
        "stream": False,
        "think": False,
        "options": {"temperature": 0, "num_predict": 32, "seed": 13},
    }
    req = urllib.request.Request(
        f"{HOST}/api/generate",
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json"},
    )
    t0 = time.perf_counter()
    with urllib.request.urlopen(req, timeout=300) as r:
        resp = json.loads(r.read())
    dt = (time.perf_counter() - t0) * 1000
    return {
        "ms": dt,
        "prompt_tokens": resp.get("prompt_eval_count", 0),
        "output_tokens": resp.get("eval_count", 0),
        "response": resp.get("response", "")[:60],
    }


for t in texts[:2]:  # 预热（含模型装载）
    run_one(t)

rows = [run_one(t) for t in texts]
lat = [r["ms"] for r in rows]
s = sorted(lat)
out = {
    "model": f"{MODEL} (Ollama, GGUF Q4 量化, think=false, temp=0, max_new=32)",
    "where": "8B (Ollama HTTP 进程间)" + ("，无前缀缓存(nonce)" if NONCE else "，前缀缓存生效"),
    "prompt_tokens_avg": round(statistics.mean(r["prompt_tokens"] for r in rows), 0),
    "output_tokens_avg": round(statistics.mean(r["output_tokens"] for r in rows), 1),
    "latency_ms": {
        "mean": round(statistics.mean(lat), 1),
        "p50": round(s[len(s) // 2], 1),
        "p95": round(s[min(len(s) - 1, int(len(s) * 0.95))], 1),
        "min": round(s[0], 1),
        "max": round(s[-1], 1),
    },
    "n": N,
    "samples": rows[:3],
}
print(json.dumps(out, ensure_ascii=False, indent=2))
(HERE / "bench_8b.json").write_text(json.dumps(out, ensure_ascii=False, indent=2), encoding="utf-8")
