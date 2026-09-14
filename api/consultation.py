"""
咨询 API

提供产品咨询问答接口（RAG）。
"""

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel

router = APIRouter(prefix="/api/consultation", tags=["咨询服务"])


class ConsultationRequest(BaseModel):
    question: str
    user_id: str = "default_customer"


@router.post("/ask")
async def ask_consultation(request: ConsultationRequest):
    """提交咨询问题"""
    try:
        from agents.consultant_agent import ConsultantAgent
        agent = ConsultantAgent()
        async with agent:
            answer = await agent.consult(request.question)
        return {"status": "success", "data": {"answer": answer, "question": request.question}}
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))
