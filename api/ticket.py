"""
工单 API

提供工单的创建、查询、分派与状态流转接口。
"""

from fastapi import APIRouter, HTTPException
from typing import List, Optional
from pydantic import BaseModel

router = APIRouter(prefix="/api/tickets", tags=["工单管理"])


class TicketCreateRequest(BaseModel):
    customer_id: int
    title: str
    description: str
    category: str = "incident"
    priority: str = "P3"


class TicketAssignRequest(BaseModel):
    engineer_id: int


class TicketStatusUpdateRequest(BaseModel):
    status: str


@router.post("/create")
async def create_ticket(request: TicketCreateRequest):
    """创建工单"""
    try:
        from services.ticket_service import TicketService
        service = TicketService()
        ticket = service.create_ticket(
            customer_id=request.customer_id,
            title=request.title,
            description=request.description,
            category=request.category,
            priority=request.priority,
        )
        if not ticket:
            raise HTTPException(status_code=400, detail="创建工单失败，请检查客户是否存在")
        return {"status": "success", "data": ticket}
    except HTTPException:
        raise
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.get("/")
async def get_all_tickets():
    """获取所有工单"""
    try:
        from services.ticket_service import TicketService
        service = TicketService()
        return {"status": "success", "data": service.get_all_tickets()}
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.get("/{ticket_id}")
async def get_ticket(ticket_id: int):
    """获取单个工单详情"""
    try:
        from services.ticket_service import TicketService
        service = TicketService()
        ticket = service.get_ticket(ticket_id=ticket_id)
        if not ticket:
            raise HTTPException(status_code=404, detail="工单不存在")
        return {"status": "success", "data": ticket}
    except HTTPException:
        raise
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.get("/no/{ticket_no}")
async def get_ticket_by_no(ticket_no: str):
    """根据工单号查询工单"""
    try:
        from services.ticket_service import TicketService
        service = TicketService()
        ticket = service.get_ticket(ticket_no=ticket_no)
        if not ticket:
            raise HTTPException(status_code=404, detail="工单不存在")
        return {"status": "success", "data": ticket}
    except HTTPException:
        raise
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.post("/{ticket_id}/assign")
async def assign_ticket(ticket_id: int, request: TicketAssignRequest):
    """分派工单给工程师"""
    try:
        from services.ticket_service import TicketService
        service = TicketService()
        if not service.assign_ticket(ticket_id, request.engineer_id):
            raise HTTPException(status_code=400, detail="分派失败，请检查工单或工程师是否存在")
        return {"status": "success", "message": "工单分派成功"}
    except HTTPException:
        raise
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.post("/{ticket_id}/status")
async def update_ticket_status(ticket_id: int, request: TicketStatusUpdateRequest):
    """更新工单状态"""
    try:
        from services.ticket_service import TicketService
        service = TicketService()
        if not service.update_status(ticket_id, request.status):
            raise HTTPException(status_code=400, detail="状态更新失败，请检查工单是否存在")
        return {"status": "success", "message": "工单状态已更新"}
    except HTTPException:
        raise
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))
