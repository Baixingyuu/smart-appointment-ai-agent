"""
企业技术支持工单调度系统 - 数据模型

核心实体：
- Engineer: 技术支持工程师（含等级、专长）
- Customer: 企业客户（含订阅套餐）
- Ticket: 工单（含类型、优先级、状态）
- KnowledgeDocument: 产品知识库文档
- CustomerActivity: 客户行为记录（用于健康度分析）
"""

from sqlalchemy import Column, Integer, String, DateTime, ForeignKey, Text, JSON
from sqlalchemy.ext.declarative import declarative_base
from sqlalchemy.orm import relationship
from datetime import datetime

Base = declarative_base()


class Engineer(Base):
    """技术支持工程师"""
    __tablename__ = 'engineers'

    id = Column(Integer, primary_key=True)
    name = Column(String, unique=True, nullable=False)
    level = Column(String, nullable=True)          # 等级：junior/intermediate/senior/expert
    specialty = Column(String, nullable=True)      # 专长：技术栈 / 负责产品模块

    tickets = relationship("Ticket", back_populates="engineer")


class Customer(Base):
    """企业客户"""
    __tablename__ = 'customers'

    id = Column(Integer, primary_key=True)
    name = Column(String, nullable=False)
    company = Column(String, nullable=True)
    plan = Column(String, default="standard")      # 套餐：standard/enterprise/ultimate

    tickets = relationship("Ticket", back_populates="customer")


class Ticket(Base):
    """工单"""
    __tablename__ = 'tickets'

    id = Column(Integer, primary_key=True)
    ticket_no = Column(String, unique=True, nullable=False)   # 工单号，如 TK-20260914-0001
    customer_id = Column(Integer, ForeignKey('customers.id'), nullable=False)
    engineer_id = Column(Integer, ForeignKey('engineers.id'), nullable=True)

    title = Column(String, nullable=False)
    description = Column(Text, nullable=False)
    category = Column(String, default="incident")  # incident/consultation/request/change
    priority = Column(String, default="P3")        # P0/P1/P2/P3
    status = Column(String, default="pending")     # pending/assigned/in_progress/pending_confirm/resolved/closed

    created_at = Column(DateTime, default=datetime.utcnow)
    updated_at = Column(DateTime, default=datetime.utcnow, onupdate=datetime.utcnow)
    resolved_at = Column(DateTime, nullable=True)

    customer = relationship("Customer", back_populates="tickets")
    engineer = relationship("Engineer", back_populates="tickets")


class KnowledgeDocument(Base):
    """产品知识库文档"""
    __tablename__ = 'knowledge_documents'

    id = Column(Integer, primary_key=True)
    content = Column(Text, nullable=False)
    category = Column(String, nullable=False)
    keywords = Column(JSON, nullable=True)
    embedding = Column(JSON, nullable=True)
    created_at = Column(DateTime, default=datetime.utcnow)
    updated_at = Column(DateTime, default=datetime.utcnow, onupdate=datetime.utcnow)
    is_active = Column(Integer, default=1)


class CustomerActivity(Base):
    """客户行为记录"""
    __tablename__ = 'customer_activities'

    id = Column(Integer, primary_key=True)
    customer_id = Column(String, nullable=False, default='default_customer')
    action_type = Column(String, nullable=False)   # ticket_created/consultation/ticket_query
    action_data = Column(JSON, nullable=True)
    ticket_id = Column(Integer, ForeignKey('tickets.id'), nullable=True)
    session_id = Column(String, nullable=True)
    created_at = Column(DateTime, default=datetime.utcnow)
