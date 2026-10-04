"""Qwen3-0.6B LoRA 意图分类微调（本机 M1 Max / MPS / fp32）。

- 系统/用户提示与线上 classify_intent 完全一致（system=INTENT_PROMPT, user=文本）
- assistant 目标 = IntentAnswer JSON（intent + 伪标签 confidence）
- Qwen3 非 thinking 渲染：enable_thinking=False，空 think 块算进监督 span，
  推理时无论是否预填 think 块，模型都直接续 JSON
- LoRA: r=16, alpha=32, dropout=0.05, 7 个投影层; lr=1e-4 cosine; 3 epochs
"""
from __future__ import annotations

import argparse
import json
import math
import random
from dataclasses import dataclass
from pathlib import Path

import torch
from torch.utils.data import Dataset
from transformers import (
    AutoModelForCausalLM,
    AutoTokenizer,
    EarlyStoppingCallback,
    Trainer,
    TrainingArguments,
)
from peft import LoraConfig, get_peft_model

HERE = Path(__file__).resolve().parent
BASE = HERE / "qwen3-0.6b"
OUTDIR = HERE / "lora_out"
SEED = 13
random.seed(SEED)
torch.manual_seed(SEED)


def load_rows(path: Path) -> list[dict]:
    return [json.loads(l) for l in path.read_text(encoding="utf-8").splitlines() if l.strip()]


class IntentDataset(Dataset):
    def __init__(self, rows: list[dict], tokenizer, max_len: int = 256) -> None:
        self.tok = tokenizer
        self.examples: list[dict] = []
        n_prefix_mismatch = 0
        for r in rows:
            msgs = r["messages"]
            prefix = tokenizer.apply_chat_template(
                msgs[:2], tokenize=False, add_generation_prompt=True, enable_thinking=False
            )
            full = tokenizer.apply_chat_template(
                msgs, tokenize=False, add_generation_prompt=False, enable_thinking=False
            )
            assert full.startswith(prefix), f"template prefix mismatch:\n{prefix!r}\n{full!r}"
            prefix_ids = tokenizer(prefix, add_special_tokens=False)["input_ids"]
            full_ids = tokenizer(full, add_special_tokens=False)["input_ids"]
            if full_ids[: len(prefix_ids)] != prefix_ids:
                n_prefix_mismatch += 1
                continue
            labels = [-100] * len(prefix_ids) + full_ids[len(prefix_ids):]
            if len(full_ids) > max_len:
                # 长尾样本：保留尾部（含 assistant 答案），丢弃超长系统提示头部
                full_ids = full_ids[-max_len:]
                labels = labels[-max_len:]
            self.examples.append({"input_ids": full_ids, "labels": labels})
        if n_prefix_mismatch:
            print(f"warn: dropped {n_prefix_mismatch} rows due to token-level prefix mismatch")
        lens = [len(e["input_ids"]) for e in self.examples]
        print(f"dataset: {len(self.examples)} rows, len avg={sum(lens)/len(lens):.0f} max={max(lens)}")

    def __len__(self) -> int:
        return len(self.examples)

    def __getitem__(self, i: int) -> dict:
        return self.examples[i]


@dataclass
class Collator:
    pad_id: int

    def __call__(self, batch: list[dict]) -> dict:
        maxlen = max(len(b["input_ids"]) for b in batch)
        input_ids, labels, attn = [], [], []
        for b in batch:
            pad = maxlen - len(b["input_ids"])
            input_ids.append(b["input_ids"] + [self.pad_id] * pad)
            labels.append(b["labels"] + [-100] * pad)
            attn.append([1] * len(b["input_ids"]) + [0] * pad)
        return {
            "input_ids": torch.tensor(input_ids, dtype=torch.long),
            "labels": torch.tensor(labels, dtype=torch.long),
            "attention_mask": torch.tensor(attn, dtype=torch.long),
        }


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default=str(BASE))
    ap.add_argument("--out", default=str(OUTDIR))
    ap.add_argument("--dtype", choices=["fp16", "fp32"], default="fp32")
    ap.add_argument("--epochs", type=float, default=2)
    ap.add_argument("--batch", type=int, default=2)
    ap.add_argument("--accum", type=int, default=8)
    ap.add_argument("--grad-ckpt", action="store_true")
    ap.add_argument("--save-steps", type=int, default=0)
    ap.add_argument("--max-len", type=int, default=320)  # p99≈276，320 保证不截断
    a = ap.parse_args()
    outdir = Path(a.out)
    td = torch.float16 if a.dtype == "fp16" else torch.float32

    device = torch.device("mps" if torch.backends.mps.is_available() else "cpu")
    print(f"device={device} base={a.base} dtype={a.dtype} out={outdir}")

    tok = AutoTokenizer.from_pretrained(a.base)
    if tok.pad_token is None:
        tok.pad_token = "<|endoftext|>"

    model = AutoModelForCausalLM.from_pretrained(a.base, dtype=td)
    model.config.use_cache = False
    if a.grad_ckpt:
        # 梯度检查点：用重算换激活内存（swap 打满时这是唯一能同时保速度的杠杆）
        model.gradient_checkpointing_enable()
        model.enable_input_require_grads()

    lcfg = LoraConfig(
        task_type="CAUSAL_LM",
        r=16,
        lora_alpha=32,
        lora_dropout=0.05,
        target_modules=["q_proj", "k_proj", "v_proj", "o_proj", "gate_proj", "up_proj", "down_proj"],
    )
    model = get_peft_model(model, lcfg)
    model.print_trainable_parameters()

    train_ds = IntentDataset(load_rows(HERE / "data/train.jsonl"), tok, max_len=a.max_len)
    dev_ds = IntentDataset(load_rows(HERE / "data/dev.jsonl"), tok, max_len=a.max_len)

    args = TrainingArguments(
        output_dir=str(outdir),
        per_device_train_batch_size=a.batch,  # 小 batch：峰值激活内存与 swap 压力下最优
        per_device_eval_batch_size=8,
        gradient_accumulation_steps=a.accum,  # effective batch = batch × accum
        learning_rate=1e-4,
        num_train_epochs=a.epochs,
        # 注意：不用 Trainer 的 fp16 AMP（MPS 上 autocast 收益差），
        # 直接把模型以 fp16 权重加载，PEFT 会把 LoRA 参数保持在 fp32
        lr_scheduler_type="cosine",
        warmup_steps=25,
        weight_decay=0.01,
        max_grad_norm=1.0,
        logging_steps=20,
        # load_best_model_at_end 要求 eval 与 save 策略一致
        eval_strategy="steps" if a.save_steps else "epoch",
        eval_steps=a.save_steps or 500,
        save_strategy="steps" if a.save_steps else "epoch",
        save_steps=a.save_steps or 500,
        save_total_limit=2,
        load_best_model_at_end=True,
        metric_for_best_model="eval_loss",
        greater_is_better=False,
        report_to=[],
        seed=SEED,
        use_cpu=(device.type != "mps"),
        dataloader_num_workers=0,
        remove_unused_columns=False,
    )

    trainer = Trainer(
        model=model,
        args=args,
        train_dataset=train_ds,
        eval_dataset=dev_ds,
        data_collator=Collator(tok.pad_token_id),
        callbacks=[EarlyStoppingCallback(early_stopping_patience=2)],
    )
    trainer.train()

    model.save_pretrained(str(outdir / "adapter"))
    tok.save_pretrained(str(outdir / "adapter"))
    print("saved:", outdir / "adapter")


if __name__ == "__main__":
    main()
