"""向量检索的离线替身：只有 embedding + 向量库是假的。

tests 与 probes 都用这一份：真索引要起 ollama + MilvusLite，而这两处要验的是
接线（工具、权限闸、事件流、AgentState），不是 embedding 质量。
"""
from __future__ import annotations

from agentscope.message import TextBlock
from agentscope.rag import Chunk, VectorSearchResult


def hit(doc_id: str, score: float, text: str, metadata: dict | None = None) -> VectorSearchResult:
    return VectorSearchResult(
        score=score,
        document_id=doc_id,
        chunk=Chunk(
            content=TextBlock(type="text", text=text),
            source=doc_id,
            chunk_index=0,
            total_chunks=1,
            metadata=metadata or {},
        ),
    )


def history_hit(
    ticket_id: int,
    title: str,
    assignee_id: int | None,
    service_id: int | None,
    score: float,
) -> VectorSearchResult:
    return hit(
        str(ticket_id),
        score,
        title,
        {
            "ticket_id": ticket_id,
            "title": title,
            "assignee_id": assignee_id or 0,
            "service_id": service_id or 0,
            "resolved": 1,
        },
    )


class FakeKnowledgeBase:
    name = "helpdesk-knowledge"
    description = "IT 帮助台知识库：接口/账号/部署/数据库类故障的处置说明。"

    def __init__(self) -> None:
        self.queries: list[str] = []

    async def search(self, queries, top_k=5, score_threshold=None):
        self.queries.append(queries[0])
        if score_threshold is not None and score_threshold > 0.6:
            return []
        return [hit("kb-interface-auth", 0.68, "接口返回 401 表示鉴权令牌过期或签名不正确。")]


class FakeIndex:
    def __init__(
        self,
        service_id: str = "2001",
        score: float = 0.61,
        employee_ids: tuple[str, ...] = ("101",),
        history: tuple[VectorSearchResult, ...] = (),
    ) -> None:
        self.knowledge = FakeKnowledgeBase()
        self.service_id = service_id
        self.score = score
        self.employee_ids = employee_ids
        self.history = history
        self.written: list[dict] = []

    async def search_services(self, query, top_k=3, score_threshold=None):
        return [hit(self.service_id, self.score, "核心下单接口")]

    async def search_employees(self, query, top_k=4, score_threshold=None):
        return [hit(i, 0.55, f"员工 {i} 的画像") for i in self.employee_ids[:top_k]]

    async def search_history(self, query, top_k=3, score_threshold=None):
        return list(self.history[:top_k])

    async def index_ticket(self, **kwargs):
        self.written.append(kwargs)


__all__ = ["FakeIndex", "FakeKnowledgeBase", "history_hit", "hit"]
