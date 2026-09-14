"""
服务层核心逻辑测试

覆盖工程师初始化、工单创建与分派、客户健康度等核心业务。
使用独立 SQLite 文件，不依赖 LLM / API Key。
"""

import os
import sys
import uuid
import pytest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))


@pytest.fixture()
def db_path():
    """为每个测试生成独立的数据库路径，避免数据串扰"""
    data_dir = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "data")
    os.makedirs(data_dir, exist_ok=True)
    unique = f"test_{uuid.uuid4().hex[:8]}.db"
    return f"sqlite:///{os.path.join(data_dir, unique)}"


def test_engineer_initialization(db_path):
    from services.engineer_service import EngineerService
    service = EngineerService(db_path)
    assert service.initialize_default_engineers() is True
    engineers = service.get_all_engineers()
    assert len(engineers) == 10


def test_ticket_creation(db_path):
    from services.customer_service import CustomerService
    from services.ticket_service import TicketService

    customer = CustomerService(db_path).get_default_customer()
    assert customer['plan'] == 'enterprise'

    service = TicketService(db_path)
    ticket = service.create_ticket(
        customer_id=customer['id'],
        title="API 接口返回 500",
        description="调用 /v1/users 接口持续返回 500，影响线上业务",
        category="incident",
        priority="P1",
    )
    assert ticket is not None
    assert ticket['ticket_no'].startswith("TK-")
    assert ticket['status'] == 'pending'
    assert ticket['priority'] == 'P1'


def test_ticket_assign(db_path):
    from services.customer_service import CustomerService
    from services.ticket_service import TicketService
    from services.engineer_service import EngineerService

    customer = CustomerService(db_path).get_default_customer()
    ticket_service = TicketService(db_path)
    engineer_service = EngineerService(db_path)
    engineer_service.initialize_default_engineers()

    ticket = ticket_service.create_ticket(
        customer_id=customer['id'],
        title="测试工单",
        description="测试工单分派",
        category="incident",
        priority="P3",
    )

    engineers = engineer_service.get_all_engineers()
    engineer = engineers[0]

    assert ticket_service.assign_ticket(ticket['id'], engineer['id']) is True

    updated = ticket_service.get_ticket(ticket_id=ticket['id'])
    assert updated['status'] == 'assigned'
    assert updated['engineer_name'] == engineer['name']


def test_ticket_status_flow(db_path):
    from services.customer_service import CustomerService
    from services.ticket_service import TicketService

    customer = CustomerService(db_path).get_default_customer()
    service = TicketService(db_path)
    ticket = service.create_ticket(
        customer_id=customer['id'],
        title="状态流转测试",
        description="测试状态流转",
    )

    assert service.update_status(ticket['id'], 'resolved') is True
    updated = service.get_ticket(ticket_id=ticket['id'])
    assert updated['status'] == 'resolved'
    assert updated['resolved_at'] is not None


def test_customer_health(db_path):
    from services.customer_service import CustomerService

    service = CustomerService(db_path)
    health = service.get_customer_health("default_customer")
    assert health is not None
    assert 'health_score' in health
    assert 'churn_risk' in health
    assert 'statistics' in health


def test_priority_level_map():
    from config.constants import PRIORITY_LEVEL_MAP
    assert PRIORITY_LEVEL_MAP['P0'] == ['expert']
    assert 'senior' in PRIORITY_LEVEL_MAP['P1']
    assert 'junior' in PRIORITY_LEVEL_MAP['P3']
