"""
客户 API

提供客户信息与健康度分析接口。
"""

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel

router = APIRouter(prefix="/api/customers", tags=["客户管理"])


class HealthRequest(BaseModel):
    customer_id: str = "default_customer"


@router.get("/")
async def get_all_customers():
    """获取所有客户"""
    try:
        from services.customer_service import CustomerService
        service = CustomerService()
        return {"status": "success", "data": service.get_all_customers()}
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.get("/default")
async def get_default_customer():
    """获取默认客户"""
    try:
        from services.customer_service import CustomerService
        service = CustomerService()
        return {"status": "success", "data": service.get_default_customer()}
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.get("/{customer_id}/health")
async def get_customer_health(customer_id: str):
    """获取客户健康度分析"""
    try:
        from services.customer_service import CustomerService
        service = CustomerService()
        return {"status": "success", "data": service.get_customer_health(customer_id)}
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.post("/insight")
async def generate_insight(request: HealthRequest):
    """生成客户洞察文案（续费/流失预警）"""
    try:
        from agents.customer_insight_agent import CustomerInsightAgent
        agent = CustomerInsightAgent()
        insight = await agent.generate_insight(request.customer_id)
        return {"status": "success", "data": {"insight": insight}}
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))
