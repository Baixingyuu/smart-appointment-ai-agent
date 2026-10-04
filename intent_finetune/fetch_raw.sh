#!/usr/bin/env bash
# 下载 build_dataset.py 需要的全部公开语料到 raw/（国内走 hf-mirror 镜像）
# 用法: bash fetch_raw.sh
set -euo pipefail
cd "$(dirname "$0")"
mkdir -p raw && cd raw
M=https://hf-mirror.com

# MASSIVE zh-CN（智能助手指令/闲聊）— 用 parquet 转换分支，无需 datasets 脚本
curl -sL -o massive_zh_train.parquet "$M/datasets/AmazonScience/massive/resolve/refs%2Fconvert%2Fparquet/zh-CN/train/0000.parquet"
# ChnSentiCorp（中文情感，取差评）
curl -sL -o chnsenticorp_train.arrow "$M/datasets/seamew/ChnSentiCorp/resolve/main/chn_senti_corp-train.arrow"
# 电商评论 10 类（差评，label 是类别编码不是情感）
curl -sL -o shopping.parquet "$M/datasets/ttxy/online_shopping_10_cats/resolve/main/data/train-00000-of-00001.parquet"
# LCCC 中文闲聊（353M，取对话首轮）
curl -sL -o lccc_base_train.jsonl.gz "$M/datasets/silver/lccc/resolve/main/lccc_base_train.jsonl.gz"
# COIG-CQIA 各子集：IT问答 / how-to / 小红书 / 弱智吧 / 考试 / 知乎
for p in "segmentfault segmentfault_upvote5_clean" "wikihow wikihow" "xhs xhs" "ruozhiba ruozhiba_ruozhiba" "exam coig_exam_sampled_clean_v3"; do
  set -- $p
  curl -sL -o "cqia_$1.jsonl" "$M/datasets/m-a-p/COIG-CQIA/resolve/main/$1/$2.jsonl"
done
curl -sL -o cqia_zhihu.jsonl "$M/datasets/m-a-p/COIG-CQIA/resolve/main/zhihu/zhihu_score8.5-9.0_clean_v4.jsonl"
echo "raw 语料下载完成，接着跑: .venv/bin/python build_dataset.py"
