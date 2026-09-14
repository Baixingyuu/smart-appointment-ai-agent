"""
工程师 API

提供工程师信息查询接口。
"""

from fastapi import APIRouter, HTTPException
from typing import List
from pydantic import BaseModel

router = APIRouter(prefix="/api/engineers", tags=["工程师管理"])


class EngineerResponse(BaseModel):
    id: int
    name: str
    level: str
    specialty: str


@router.get("/", response_model=List[EngineerResponse])
async def get_all_engineers():
    """获取所有工程师"""
    try:
        from services.engineer_service import EngineerService
        service = EngineerService()
        service.initialize_default_engineers()
        engineers = service.get_all_engineers()
        return [
            EngineerResponse(
                id=e["id"],
                name=e["name"],
                level=e.get("level", ""),
                specialty=e.get("specialty", ""),
            )
            for e in engineers
        ]
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.get("/{engineer_id}", response_model=EngineerResponse)
async def get_engineer(engineer_id: int):
    """获取单个工程师信息"""
    try:
        from services.engineer_service import EngineerService
        service = EngineerService()
        service.initialize_default_engineers()
        e = service.get_engineer_by_id(engineer_id)
        if not e:
            raise HTTPException(status_code=404, detail="工程师不存在")
        return EngineerResponse(
            id=e["id"],
            name=e["name"],
            level=e.get("level", ""),
            specialty=e.get("specialty", ""),
        )
    except HTTPException:
        raise
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))
