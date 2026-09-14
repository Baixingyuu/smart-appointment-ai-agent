"""
Agent 组件测试（不依赖 LLM）

覆盖消息构建、工程师等级过滤、输入解析兜底、工单信息完整性判断等纯逻辑。
"""

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))


# ---------- MessageBuilder ----------

def test_message_builder_ticket_success():
    from agents.ticketing.message_builder import MessageBuilder
    builder = MessageBuilder()
    ticket = {
        'ticket_no': 'TK-20260914-0001',
        'priority': 'P1',
    }
    engineer = {'name': '张伟', 'level': 'expert'}
    msg = builder.create_ticket_success_message(ticket, engineer)
    assert 'TK-20260914-0001' in msg
    assert '张伟' in msg
    assert 'P1' in msg


def test_message_builder_missing_info():
    from agents.ticketing.message_builder import MessageBuilder
    builder = MessageBuilder()
    msg = builder.create_missing_info_questions(['description'])
    assert '问题' in msg


# ---------- EngineerFinder 等级过滤 ----------

def test_engineer_finder_filter_by_level():
    from agents.ticketing.engineer_finder import EngineerFinder
    finder = EngineerFinder()
    engineers = [
        {'id': 1, 'level': 'expert'},
        {'id': 2, 'level': 'senior'},
        {'id': 3, 'level': 'junior'},
    ]
    # P0 只允许 expert
    result = finder._filter_by_level(engineers, 'P0')
    assert len(result) == 1
    assert result[0]['level'] == 'expert'

    # P3 允许 intermediate 和 junior
    result3 = finder._filter_by_level(engineers, 'P3')
    levels = {e['level'] for e in result3}
    assert 'junior' in levels


# ---------- InputParser 解析兜底 ----------

def test_input_parser_parse_data_valid_json():
    from agents.ticketing.input_parser import InputParser
    parser = object.__new__(InputParser)  # 绕过 __init__（不需要 LLM）
    data = parser.parse_data('{"title":"接口报错","description":"返回500","category":"incident","priority":"P1"}')
    assert data['description'] == '返回500'
    assert data['priority'] == 'P1'


def test_input_parser_parse_data_invalid_json():
    from agents.ticketing.input_parser import InputParser
    parser = object.__new__(InputParser)
    data = parser.parse_data('这不是 JSON')
    assert data['info_complete'] is False
    assert data['description'] == '未知'


# ---------- TicketProcessor 信息完整性 ----------

def test_ticket_processor_update_history_complete():
    from agents.ticketing.ticket_processor import TicketProcessor
    processor = TicketProcessor(None, None, None, None, None)
    history = {"title": None, "description": None, "category": None,
               "priority": None, "engineer_name": None}
    data = {
        "title": "接口报错",
        "description": "接口返回 500",
        "category": "incident",
        "priority": "P1",
        "engineer_name": "未知",
    }
    finished = processor.update_history_from_data(history, data)
    assert finished is True
    assert history['description'] == '接口返回 500'


def test_ticket_processor_update_history_incomplete():
    from agents.ticketing.ticket_processor import TicketProcessor
    processor = TicketProcessor(None, None, None, None, None)
    history = {"title": None, "description": None, "category": None,
               "priority": None, "engineer_name": None}
    data = {"title": "接口报错", "description": "未知"}
    finished = processor.update_history_from_data(history, data)
    assert finished is False
