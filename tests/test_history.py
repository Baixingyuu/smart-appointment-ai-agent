"""历史语料（派单 v4 第三源）的离线闸：不花模型，也不开向量库。

这批数据是我写的，所以它的可信度不能靠"我看着没问题"：
归属必须由 SERVICES 反查得出来、工单号不能撞真跑、正文不能和 45 条评测样本重合。
第三条尤其要紧 —— 重合就等于把金标塞进召回结果里。
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from helpdesk.catalog import SERVICES, employees  # noqa: E402
from helpdesk.dispatch import ANSWER_CHOICES, HistoryCase, render_history  # noqa: E402
from helpdesk.history import BACKUP_CASE_IDS, HISTORY  # noqa: E402
from helpdesk.runtime.textmatch import COVERAGE_GATE, char_coverage, is_substring  # noqa: E402

CASES = json.loads((ROOT / "eval/datasets/assignment_v2.json").read_text())["cases"]


def test_history_assignees_are_owner_or_backup_never_a_third_person() -> None:
    """`kind` 从 SERVICES 反查：写进语料的每个归属都必须是归属表里的人。

    历史一旦被模型引用就是软答案，所以这里不允许"我觉得该给他"。
    """
    kinds = [r.kind.value for r in HISTORY]
    assert set(kinds) == {"owner", "backup"}
    assert kinds.count("backup") == len(BACKUP_CASE_IDS) == 3, "顶班条数漂了：语料开始改写法庭"
    for r in HISTORY:
        if r.kind.value == "backup":
            assert r.note.startswith("backup 顶班"), f"#{r.id} 顶班却没写为什么不是 owner"


def test_history_ids_stay_out_of_the_live_ticket_namespace() -> None:
    ids = [r.id for r in HISTORY]
    assert len(set(ids)) == len(ids)
    assert min(ids) >= 5001, "真跑工单号从 1 起，撞号会让 cited_tickets 分不清哪个世界"


def test_every_service_has_at_least_one_resolved_case() -> None:
    covered = {r.service_id for r in HISTORY}
    assert covered == {s.id for s in SERVICES}, f"这些服务没有历史可召：{sorted({s.id for s in SERVICES} - covered)}"


def test_history_assignees_are_active_and_pickable() -> None:
    """语料里不能出现选不出来的人：枚举由名册派生，离职者不在归属表里就当不了历史归属。"""
    by_id = {e.id: e for e in employees()}
    for r in HISTORY:
        assert str(r.assignee_id) in ANSWER_CHOICES
        assert by_id[r.assignee_id].active, f"#{r.id} 的历史归属 {r.assignee_id} 已离职"


def test_history_does_not_leak_the_eval_cases() -> None:
    """45 条评测正文与 20 条历史互相的字元覆盖都不能过闸。

    两个方向都算：gold→history 高 = 这条历史几乎就是那道题；history→gold 高 = 反过来。
    """
    worst = []
    for case in CASES:
        ticket = case["ticket"]
        gold = f"{ticket['title']}。{ticket['description']}"
        for r in HISTORY:
            body = r.searchable_text
            forward = char_coverage(gold, body)
            backward = char_coverage(body, gold)
            worst.append((max(forward, backward), case["id"], r.id, round(forward, 3), round(backward, 3)))
            assert not is_substring(r.title, ticket["title"]), f"{case['id']} 的正文含历史标题 #{r.id}"
            assert not is_substring(ticket["title"], r.title), f"历史 #{r.id} 的标题含评测标题 {case['id']}"
    worst.sort(reverse=True)
    assert worst[0][0] < COVERAGE_GATE, f"最接近的一对：{worst[:3]}（闸 {COVERAGE_GATE}）"


def test_render_history_names_the_handler_and_flags_departures() -> None:
    """裸工号在 24 人名册里读不出意义；离职者出现在历史里必须带警告。"""
    text = render_history((HistoryCase(5002, "订单接口雪崩", 107, 2001, 0.61),))
    assert "107 黄磊" in text
    warned = render_history((HistoryCase(5099, "老的下单故障", 109, 2001, 0.61),))
    assert "已离职，不可照抄" in warned
    assert "109 郑爽" in warned
    assert "未知" in render_history((HistoryCase(5098, "查不到归属", None, None, 0.4),))


def test_history_records_are_resolvable_to_a_real_service_name() -> None:
    svc = {s.id: s for s in SERVICES}
    for r in HISTORY:
        assert r.service_id in svc
        assert len(r.description) >= 30, f"#{r.id} 正文太短，dense 召不出区分度"
