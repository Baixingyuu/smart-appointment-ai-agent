"""
文本嵌入工具

提供文本向量化与相似度匹配能力，用于工程师专长匹配和知识库检索。
"""

import numpy as np
import faiss
from config.model_provider import create_embedding_model


def embed_input(input_text: str) -> list:
    """将文本转换为嵌入向量"""
    embeddings = create_embedding_model()
    return embeddings.embed_query(input_text)


def find_best_match_indices(text: str, candidates: list) -> list:
    """
    将 text 与候选列表逐一计算相似度，返回按相似度从高到低排序的索引列表。

    :param text: 待匹配文本（如工单问题描述 / 期望技术栈）
    :param candidates: 候选文本列表（如工程师专长列表）
    :return: 候选索引列表，按相似度降序
    """
    if not candidates:
        return []

    candidate_embs = [embed_input(c) for c in candidates]
    candidate_embs = np.array(candidate_embs).astype("float32")
    dimension = candidate_embs.shape[1]

    index = faiss.IndexFlatL2(dimension)
    index.add(candidate_embs)

    text_emb = np.array([embed_input(text)]).astype("float32")
    k = len(candidates)
    _, indices = index.search(text_emb, k)
    return indices[0][:k].tolist()
