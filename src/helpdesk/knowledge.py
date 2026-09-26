"""dense 检索层：embedding 模型 + 向量库 + 两个 KnowledgeBase。

Go 侧的 BM25、CJK 分词与 6 桶置信门整体删除，这里只剩框架的
KnowledgeBase(embedding_model, vector_store, collection)。
score_threshold 取代阈值门，且必须等分数分布出来再定，不在代码里拍。
"""
from __future__ import annotations

import os
from dataclasses import dataclass
from typing import Self

from agentscope.credential import OllamaCredential
from agentscope.embedding import EmbeddingModelBase, FileEmbeddingCache, OllamaEmbeddingModel
from agentscope.rag import Chunk, KnowledgeBase, MilvusLiteStore, VectorSearchResult
from agentscope.message import TextBlock

from .catalog import KNOWLEDGE, SERVICES, KnowledgeChunk, Service

EMBEDDAGE_MODEL = os.environ.get("HELPDESK_EMBED_MODEL", "bge-m3")
EMBEDDING_DIMS = int(os.environ.get("HELPDESK_EMBED_DIMS", "1024"))
DB_PATH = os.environ.get("HELPDESK_VECTOR_DB", "./data/milvus_lite.db")
CACHE_DIR = os.environ.get("HELPDESK_EMBED_CACHE", "./data/embed_cache")

KNOWLEDGE_COLLECTION = "knowledge"
SERVICES_COLLECTION = "services"

# 取代 Go 的 6 桶置信门。取值来自实测分布（eval/reports/retrieval.json）：
# 不可回答查询的 top-1 最高 0.544，可回答查询过 0.55 闸的仍有 93.8%，
# 再高就开始误杀（0.60 → pass 86.3%，0.70 → 20.0%）。
KNOWLEDGE_SCORE_THRESHOLD = 0.55


def cache_enabled() -> bool:
    """Embedding cache is a latency confound — allow turning it off."""
    return os.environ.get("HELPDESK_EMBED_CACHE", "on").lower() not in {"off", "0", "none"}


def make_embedding_model(use_cache: bool | None = None) -> EmbeddingModelBase:
    if use_cache is None:
        use_cache = cache_enabled()
    return OllamaEmbeddingModel(
        credential=OllamaCredential(host=os.environ.get("OLLAMA_HOST")),
        model=EMBEDDAGE_MODEL,
        dimensions=EMBEDDING_DIMS,
        embedding_cache=FileEmbeddingCache(cache_dir=CACHE_DIR) if use_cache else None,
    )


def _chunk(text: str, source: str, metadata: dict) -> Chunk:
    return Chunk(
        content=TextBlock(type="text", text=text),
        source=source,
        chunk_index=0,
        total_chunks=1,
        metadata=metadata,
    )


@dataclass
class HelpdeskIndex:
    """One store connection, two logical knowledge bases (own collection each)."""

    store: MilvusLiteStore
    knowledge: KnowledgeBase
    services: KnowledgeBase

    async def __aenter__(self) -> Self:
        await self.store.__aenter__()
        await self.ensure_ready()
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.store.__aexit__(*exc)

    async def ensure_ready(self) -> None:
        """把两个集合置为可查/可写。

        第二条框架坑（实跑 qwen3 时撞出来的）：`KnowledgeBase.ensure_collection` 只
        memoize「这个实例建过集合没有」，重新打开一个已存在的库时不会 load_collection，
        于是 search 抛 `Collection 'knowledge' is in state 'released'`；反过来
        delete_collection 之后 memoize 也仍是 True。`store.create_collection` 对已存在的
        集合就是 load_collection，拿它当幂等闸。
        """
        for kb in (self.knowledge, self.services):
            await self.store.create_collection(
                kb.collection,
                dimensions=kb.embedding_model.dimensions,
            )

    async def build(
        self,
        recreate: bool = True,
        corpus: tuple[KnowledgeChunk, ...] | None = None,
    ) -> dict[str, int]:
        """(Re)index the catalog and the corpus, returning collection counts."""
        chunks = KNOWLEDGE if corpus is None else corpus
        for name in (self.knowledge.collection, self.services.collection):
            if recreate:
                await self.store.delete_collection(name)
        await self.ensure_ready()
        for svc in SERVICES:
            await self.services.insert_document(
                [_chunk(svc.searchable_text, f"service:{svc.id}", {
                    "service_id": svc.id,
                    "name": svc.name,
                    "team_id": svc.team_id,
                    "owner_id": svc.owner_id,
                    "backup_owner_id": svc.backup_owner_id or 0,
                })],
                document_id=str(svc.id),
            )
        for c in chunks:
            await self.knowledge.insert_document(
                [_chunk(c.searchable_text, c.id, {
                    "chunk_id": c.id,
                    "doc_id": c.doc_id,
                    "title": c.title,
                    "keywords": list(c.keywords),
                })],
                document_id=c.id,
            )
        return {
            self.services.collection: len(await self.services.list_documents()),
            self.knowledge.collection: len(await self.knowledge.list_documents()),
        }

    async def search_knowledge(
        self,
        query: str,
        top_k: int = 5,
        score_threshold: float | None = KNOWLEDGE_SCORE_THRESHOLD,
    ) -> list[VectorSearchResult]:
        return await self.knowledge.search([query], top_k, score_threshold)

    async def search_services(
        self,
        query: str,
        top_k: int = 3,
        score_threshold: float | None = None,
    ) -> list[VectorSearchResult]:
        return await self.services.search([query], top_k, score_threshold)


async def open_index(
    embedding_model: EmbeddingModelBase | None = None,
    *,
    db_path: str = DB_PATH,
    knowledge_collection: str = KNOWLEDGE_COLLECTION,
) -> HelpdeskIndex:
    model = embedding_model or make_embedding_model()
    store = MilvusLiteStore(uri=db_path, metric_type="COSINE")
    os.makedirs(os.path.dirname(db_path) or ".", exist_ok=True)
    return HelpdeskIndex(
        store=store,
        knowledge=KnowledgeBase(
            name="helpdesk-knowledge",
            description="IT 帮助台知识库：接口/账号/部署/数据库类故障的处置说明。",
            embedding_model=model,
            vector_store=store,
            collection=knowledge_collection,
        ),
        services=KnowledgeBase(
            name="service-catalog",
            description="可运维服务字典：服务名、别名与职责描述，用于把工单文本定位到具体服务。",
            embedding_model=model,
            vector_store=store,
            collection=SERVICES_COLLECTION,
        ),
    )


__all__ = [
    "HelpdeskIndex",
    "Service",
    "make_embedding_model",
    "open_index",
]


if __name__ == "__main__":
    import asyncio

    async def _main() -> None:
        async with await open_index() as index:
            print(await index.build(recreate=True))

    asyncio.run(_main())
