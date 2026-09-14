# 评测模块（eval/）

本目录提供智能工单助手的完整评测体系，用于量化验证系统能力、产出可写入简历的数据。

## 评测维度总览

| 评测维度 | 被测组件 | 核心指标 | 是否需 LLM | 脚本 |
|---|---|---|---|---|
| 意图分类 | Task Classification Agent | Accuracy、Macro-F1 | 是 | `run_intent_eval.py` |
| 工单信息提取 | Ticketing Agent / InputParser | 字段级准确率（标题/类型/优先级） | 是 | `run_extraction_eval.py` |
| 工程师匹配 | EngineerFinder | Top-1 命中率 | 否（离线相似度） | `run_matching_eval.py` |

> 说明：本项目刻意**不做“当前负载均衡”评测**。原因见面试文档第四章：演示规模下负载均衡的区分度与业务价值有限，更适合放在工单池/调度层而非首轮匹配。

## 快速开始

### 无需 LLM 的评测（可直接跑）

```bash
python eval/run_matching_eval.py    # 工程师匹配准确率（Jaccard 近似 embedding）
```

### 需要 LLM 的评测（先配 .env）

```bash
# .env 配置好 MODEL_PROVIDER、LLM_API_KEY 后：
python eval/run_intent_eval.py      # 意图分类准确率
python eval/run_extraction_eval.py  # 工单信息提取准确率
```

## 评测数据

- `datasets/intent_classification.json`：40 条用户输入，标注 consultation / ticketing / other 三类意图。
- `datasets/ticket_extraction.json`：20 条问题描述，标注标题 / 类型 / 优先级。
- 工程师匹配评测集内置于 `run_matching_eval.py`（30 条标注工单）。

## 核心对比实验（体现创新点）

### 工程师匹配：专长匹配 vs 随机

工程师匹配采用「等级过滤 + 专长相似度」二级策略，专长相似度用离线 Jaccard 相似度
（可替换为真实 embedding），Top-1 命中率显著高于随机匹配基线（约 10%  vs  83.3%）。

## 扩展建议

- 将离线 Jaccard 相似度替换为真实 embedding（`services/text_embedding.find_best_match_indices`），
  得到更贴近生产环境的匹配准确率。
- 引入 Ragas 对 RAG 问答做 Faithfulness / 相关性评测。
- 增加端到端任务成功率评测（完整对话能否完成工单创建）。
