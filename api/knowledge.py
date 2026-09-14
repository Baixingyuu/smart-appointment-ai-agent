"""
知识库管理 API
"""

from fastapi import APIRouter, HTTPException
from typing import List, Optional
from pydantic import BaseModel

router = APIRouter(prefix="/api/knowledge", tags=["知识库管理"])


class KnowledgeItem(BaseModel):
    content: str
    category: str = "general"
    keywords: Optional[List[str]] = None


class SearchRequest(BaseModel):
    query: str
    top_k: int = 3


def _get_service():
    from services.knowledge_service import KnowledgeService
    return KnowledgeService()


@router.get("/")
async def get_all_knowledge():
    """获取所有知识条目"""
    try:
        service = _get_service()
        if not service.initialized:
            await service.initialize()
        entries = service.get_all_documents()
        categories = service.get_all_categories()
        return {
            "documents": entries or [],
            "categories": categories or [],
            "total_count": len(entries) if entries else 0,
            "status": "success",
        }
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.get("/{knowledge_id}")
async def get_knowledge(knowledge_id: int):
    """获取特定知识条目"""
    try:
        service = _get_service()
        if not service.initialized:
            await service.initialize()
        entry = service.get_document(knowledge_id)
        if not entry:
            raise HTTPException(status_code=404, detail="知识条目不存在")
        return {"status": "success", "data": entry}
    except HTTPException:
        raise
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.post("/")
async def add_knowledge(item: KnowledgeItem):
    """添加知识条目"""
    try:
        service = _get_service()
        if not service.initialized:
            await service.initialize()
        result = await service.add_document(
            content=item.content,
            category=item.category,
            keywords=item.keywords,
        )
        return {"status": "success", "message": "知识条目添加成功", "data": result}
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.put("/{knowledge_id}")
async def update_knowledge(knowledge_id: int, item: KnowledgeItem):
    """更新知识条目"""
    try:
        service = _get_service()
        if not service.initialized:
            await service.initialize()
        result = await service.update_document(
            doc_id=knowledge_id,
            content=item.content,
            category=item.category,
            keywords=item.keywords,
        )
        if not result:
            raise HTTPException(status_code=404, detail="知识条目不存在")
        return {"status": "success", "message": "知识条目更新成功"}
    except HTTPException:
        raise
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.delete("/{knowledge_id}")
async def delete_knowledge(knowledge_id: int):
    """删除知识条目"""
    try:
        service = _get_service()
        if not service.initialized:
            await service.initialize()
        result = await service.delete_document(knowledge_id)
        if not result:
            raise HTTPException(status_code=404, detail="知识条目不存在")
        return {"status": "success", "message": "知识条目删除成功"}
    except HTTPException:
        raise
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.post("/search")
async def search_knowledge(request: SearchRequest):
    """搜索知识库"""
    try:
        service = _get_service()
        if not service.initialized:
            await service.initialize()
        results = await service.search(request.query, top_k=request.top_k)
        return {"status": "success", "data": results, "count": len(results)}
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))
