"""dense 检索层：embedding 模型 + 向量库 + 四个逻辑知识源。

Go 侧的 BM25、CJK 分词与 6 桶置信门整体删除，这里只剩框架的
KnowledgeBase(embedding_model, vector_store, collection)。
score_threshold 取代阈值门，且必须等分数分布出来再定，不在代码里拍。

派单 v4 的三源召回都在这一层：services（工单→服务）、employees（能力画像）、
tickets（历史工单）。其中"只召回已结工单"用框架的 metadata_filter —— 它是
**构造期**绑定、search/list 永远逃不掉、insert 还会强制盖到每个 chunk 上
（`rag/_knowledge.py:25`），所以同一个物理集合上开两个句柄就够：写入句柄不带过滤，
读取句柄带 {"resolved": 1}。自己再写一遍过滤器等于把框架给的接缝实现第二遍。
"""
from __future__ import annotations

import os
from dataclasses import dataclass
from typing import Self

from agentscope.credential import OllamaCredential
from agentscope.embedding import EmbeddingModelBase, FileEmbeddingCache, OllamaEmbeddingModel
from agentscope.rag import Chunk, KnowledgeBase, MilvusLiteStore, VectorSearchResult
from agentscope.message import TextBlock

from .catalog import KNOWLEDGE, SERVICES, KnowledgeChunk, Service, employees
from .history import HISTORY

EMBEDDAGE_MODEL = os.environ.get("HELPDESK_EMBED_MODEL", "bge-m3")
EMBEDDING_DIMS = int(os.environ.get("HELPDESK_EMBED_DIMS", "1024"))
DB_PATH = os.environ.get("HELPDESK_VECTOR_DB", "./data/milvus_lite.db")
CACHE_DIR = os.environ.get("HELPDESK_EMBED_CACHE", "./data/embed_cache")

KNOWLEDGE_COLLECTION = "knowledge"
SERVICES_COLLECTION = "services"
EMPLOYEES_COLLECTION = "employees"
TICKETS_COLLECTION = "tickets"

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
    """One store connection; logical knowledge bases with their own collections.

    `tickets` 和 `resolved_tickets` 是**同一个物理集合上的两个句柄**：前者只写
    （不带过滤，所以能把 resolved 标记写进去），后者只读（带框架的构造期过滤）。
    """

    store: MilvusLiteStore
    knowledge: KnowledgeBase
    services: KnowledgeBase
    employees: KnowledgeBase
    tickets: KnowledgeBase
    resolved_tickets: KnowledgeBase
    #: 打开连接时自动把已结历史语料灌进去。关掉它才能拿到"空历史"的世界。
    seed_history: bool = True
    history_loaded: int = 0

    @property
    def _bases(self) -> tuple[KnowledgeBase, ...]:
        seen: dict[str, KnowledgeBase] = {}
        for kb in (self.knowledge, self.services, self.employees, self.tickets):
            seen.setdefault(kb.collection, kb)
        return tuple(seen.values())

    async def __aenter__(self) -> Self:
        await self.store.__aenter__()
        await self.ensure_ready()
        # 语料在这里就位，而不是让每个入口自己记得调：app 与 P3 冒烟都不调的话，
        # v4 的第三源在真跑里永远是空的，实测也就永远测不到它。
        if self.seed_history:
            self.history_loaded = await self.load_history()
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.store.__aexit__(*exc)

    async def ensure_ready(self) -> None:
        """把所有集合置为可查/可写。

        第二条框架坑（实跑 qwen3 时撞出来的）：`KnowledgeBase.ensure_collection` 只
        memoize「这个实例建过集合没有」，重新打开一个已存在的库时不会 load_collection，
        于是 search 抛 `Collection 'knowledge' is in state 'released'`；反过来
        delete_collection 之后 memoize 也仍是 True。`store.create_collection` 对已存在的
        集合就是 load_collection，拿它当幂等闸。
        """
        for kb in self._bases:
            await self.store.create_collection(
                kb.collection,
                dimensions=kb.embedding_model.dimensions,
            )

    async def build(
        self,
        recreate: bool = True,
        corpus: tuple[KnowledgeChunk, ...] | None = None,
    ) -> dict[str, int]:
        """(Re)index the static catalogs.

        历史工单**不在这里重建** —— 它是跑出来的真实数据，不是目录的派生物。
        """
        chunks = KNOWLEDGE if corpus is None else corpus
        names = {kb.collection for kb in self._bases} - {self.tickets.collection}
        for name in names:
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
        for emp in employees():
            await self.employees.insert_document(
                [_chunk(emp.searchable_text, f"employee:{emp.id}", {
                    "employee_id": emp.id,
                    "name": emp.name,
                    "team_id": emp.team_id,
                    "level": emp.level.value,
                    "active": 1 if emp.active else 0,
                })],
                document_id=str(emp.id),
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
            self.employees.collection: len(await self.employees.list_documents()),
            self.knowledge.collection: len(await self.knowledge.list_documents()),
        }

    async def index_ticket(
        self,
        ticket_id: int,
        title: str,
        description: str,
        assignee_id: int | None,
        service_id: int | None,
        resolved: bool,
        category: str = "",
        priority: str = "",
    ) -> None:
        """写/更新一条历史工单进 tickets 集合（document_id=工单号）。

        **不是 upsert**：框架的 `insert_document` 只 append，同一个 document_id 再插一次
        就是两条向量、检索里出重复。重复灌库的幂等由 `load_history` 的 document_id 闸负责。

        `resolved` 同时是读取句柄的过滤键，所以未结的单**不可能**被历史召回看见 ——
        这条隔离由框架保证，不靠调用方自觉。
        """
        await self.tickets.insert_document(
            [_chunk(f"{title}。{description}", f"ticket:{ticket_id}", {
                "ticket_id": ticket_id,
                "title": title,
                "assignee_id": assignee_id or 0,
                "service_id": service_id or 0,
                "category": category,
                "priority": priority,
                "resolved": 1 if resolved else 0,
            })],
            document_id=str(ticket_id),
        )

    async def load_history(self) -> int:
        """把已结历史语料（`helpdesk.history.HISTORY`）灌进 tickets 集合，返回新写条数。

        幂等靠先 `list_documents()` 再跳过 —— 框架的 insert 不去重，不设这道闸每次开库
        都会把同一批工单多写一遍，检索里就出现重复历史。

        不并进 `build()`：build 会把集合 recreate 掉，而这个集合里除了语料还有真跑攒下的
        单，重灌目录不该把它们冲没。
        """
        existing = {d.document_id for d in await self.tickets.list_documents()}
        written = 0
        for r in HISTORY:
            if str(r.id) in existing:
                continue
            await self.index_ticket(
                ticket_id=r.id,
                title=r.title,
                description=r.description,
                assignee_id=r.assignee_id,
                service_id=r.service_id,
                resolved=True,
                category=r.category.value,
                priority=r.priority.value,
            )
            written += 1
        return written

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

    async def search_employees(
        self,
        query: str,
        top_k: int = 4,
        score_threshold: float | None = None,
    ) -> list[VectorSearchResult]:
        return await self.employees.search([query], top_k, score_threshold)

    async def search_history(
        self,
        query: str,
        top_k: int = 3,
        score_threshold: float | None = None,
    ) -> list[VectorSearchResult]:
        """只召回已结工单：过滤发生在框架的读取句柄里。"""
        return await self.resolved_tickets.search([query], top_k, score_threshold)


async def open_index(
    embedding_model: EmbeddingModelBase | None = None,
    *,
    db_path: str = DB_PATH,
    knowledge_collection: str = KNOWLEDGE_COLLECTION,
    services_collection: str = SERVICES_COLLECTION,
    employees_collection: str = EMPLOYEES_COLLECTION,
    tickets_collection: str = TICKETS_COLLECTION,
    seed_history: bool = True,
) -> HelpdeskIndex:
    """打开索引。评测用 *_collection 参数换一套集合名，与真跑攒下的历史隔离。

    `seed_history=False` 拿不到已结语料 —— 只有需要"空历史世界"的探针会用。
    """
    model = embedding_model or make_embedding_model()
    store = MilvusLiteStore(uri=db_path, metric_type="COSINE")
    os.makedirs(os.path.dirname(db_path) or ".", exist_ok=True)
    tickets_meta = {
        "embedding_model": model,
        "vector_store": store,
        "collection": tickets_collection,
    }
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
            collection=services_collection,
        ),
        employees=KnowledgeBase(
            name="employee-roster",
            description="员工能力画像：谁负责哪类链路、谁的层级与兜底能力如何，用于召回候选人。",
            embedding_model=model,
            vector_store=store,
            collection=employees_collection,
        ),
        tickets=KnowledgeBase(
            name="ticket-history-write",
            description="历史工单写入句柄。",
            **tickets_meta,
        ),
        resolved_tickets=KnowledgeBase(
            name="ticket-history",
            description="已结历史工单：同类问题当初谁处理的、归到哪个服务。",
            metadata_filter={"resolved": 1},
            **tickets_meta,
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
