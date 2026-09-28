"""预约域（P6-b）：时段、可用性、占用与生命周期。全离线，一次模型都不调。

为什么这些账值得钉住：预约买的是"人和时间"这份稀缺资源，算错的代价是用户收到
两个工程师、或约上一个根本不站人的时段。而 `staff()` 这个带占用的名册视图同时是
派单侧的输入（`tools.match_service` → `recall_employees(roster=...)`），
所以这里的负载数字错了，派单也会跟着错 —— 第 4 节把那条同源关系钉成断言。

时刻选择：域层测试一律显式传 `now`（不跟真实时钟赛跑）；工具层测试用"明天"，
因为工具内部用的是 `datetime.now()`。
"""
from __future__ import annotations

import sys
from datetime import datetime, timedelta
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from agentscope.message import ToolResultState, UserMsg  # noqa: E402

from helpdesk.appointment import (  # noqa: E402
    DURATION_CHOICES,
    PROPOSAL_LIMIT,
    Appointment,
    AppointmentStatus,
    AppointmentStore,
    InvalidBooking,
    missing_appointment_slots,
    on_site_windows,
    openings_for,
    with_overlay,
)
from helpdesk.catalog import employees  # noqa: E402
from helpdesk.dispatch import recall_employees, render_candidates  # noqa: E402
from helpdesk.domain import Category, ExtractedSlot, Priority, slot_label  # noqa: E402
from helpdesk.eval.fakes import FakeIndex  # noqa: E402
from helpdesk.ticket_store import TicketStore  # noqa: E402
from helpdesk.tools import AskUserInput, HelpdeskContext, build_tools  # noqa: E402

#: 域层的"现在"。与被测时刻隔开四天，够排期又不跨年。
NOW = datetime(2026, 10, 8, 8, 0)


def at(hour: int, minute: int = 0, *, day: int = 12) -> datetime:
    return datetime(2026, 10, day, hour, minute)


def emp(emp_id: int):
    return next(e for e in employees() if e.id == emp_id)


def booked(store: AppointmentStore, emp_id: int, start: datetime, **kw) -> Appointment:
    return store.book(
        engineer_id=emp_id,
        start=start,
        duration_minutes=kw.pop("duration_minutes", 60),
        need=kw.pop("need", "现场排查下单接口 401"),
        now=NOW,
        **kw,
    )


# --- 1. 窗口与身份：谁能被约 ------------------------------------------------


def test_窗口之外的时段被挡下并报出窗口() -> None:
    store = AppointmentStore()
    with pytest.raises(InvalidBooking) as exc:
        booked(store, 101, at(19))
    assert "不在" in str(exc.value) and "上门窗口" in str(exc.value)
    assert store.appointments == {}


def test_收工时刻正好等于窗口上沿是允许的() -> None:
    """窗口 [09:00, 18:00]：17:00–18:00 站得下，17:30–18:30 站不下。"""
    store = AppointmentStore()
    assert booked(store, 101, at(17)).end == at(18)
    with pytest.raises(InvalidBooking):
        booked(store, 101, at(17, 30))


def test_不承接上门的组没有可约窗口() -> None:
    store = AppointmentStore()
    assert on_site_windows(emp(107)) == ()
    with pytest.raises(InvalidBooking) as exc:
        booked(store, 107, at(11))
    assert "不承接上门" in str(exc.value)


def test_离职者不能承接上门() -> None:
    """名册里留着 109/124 就是为了让这条闸可观测（派单侧同一个道理）。"""
    store = AppointmentStore()
    with pytest.raises(InvalidBooking) as exc:
        booked(store, 109, at(11))
    assert "已离职" in str(exc.value)


def test_时长只能取排班档位() -> None:
    store = AppointmentStore()
    with pytest.raises(InvalidBooking) as exc:
        booked(store, 101, at(11), duration_minutes=45)
    assert str(DURATION_CHOICES) in str(exc.value)
    assert store.appointments == {}
    # 101 是 2/5，恰好还剩三格：档位之内每档都约得出去
    for start, d in ((at(9), 30), (at(10), 60), (at(12), 120)):
        assert booked(store, 101, start, duration_minutes=d).duration_minutes == d


def test_没有需求写不清这趟去干什么() -> None:
    with pytest.raises(InvalidBooking) as exc:
        Appointment(0, 101, at(11), 60, "   ").validate()
    assert "上门要做什么" in str(exc.value)


# --- 2. 冲突与并发 ----------------------------------------------------------


def test_时段冲突拦得住背靠背拦不住() -> None:
    store = AppointmentStore()
    booked(store, 101, at(10))
    with pytest.raises(InvalidBooking) as exc:
        booked(store, 101, at(10, 30))
    assert "时段冲突" in str(exc.value)
    # 上一单的收工时刻 = 这一单的开工时刻，不算冲突
    assert booked(store, 101, at(11)).start == at(11)


def test_并发上限用的是基线加运行期占用() -> None:
    """111 康宁基线 2/3：只有一个小格，第二单必须被这道闸而不是冲突闸挡下。"""
    store = AppointmentStore()
    assert emp(111).max_concurrent - emp(111).current_load == 1
    booked(store, 111, at(10))
    with pytest.raises(InvalidBooking) as exc:
        booked(store, 111, at(11))
    assert "基线负载 2 + 已有占用 1" in str(exc.value) and "上限 3" in str(exc.value)


def test_两道闸的次序也是账_先撞冲突就不报无余量() -> None:
    store = AppointmentStore()
    booked(store, 111, at(10))
    with pytest.raises(InvalidBooking) as exc:
        booked(store, 111, at(10, 30))
    assert "时段冲突" in str(exc.value)


def test_占用按人算不跨人串味() -> None:
    store = AppointmentStore()
    booked(store, 101, at(10))
    assert booked(store, 102, at(10)).engineer_id == 102
    assert store.occupancy() == {101: 1, 102: 1}


# --- 3. 生命周期：幂等、释放、改期、回访 ------------------------------------


def test_重复提交返回同一条且不二次占用() -> None:
    """确认闸之后模型再调一次 book 是常态，不能变成报错、也不能占两格。"""
    store = AppointmentStore()
    first = booked(store, 101, at(10))
    again = booked(store, 101, at(10))
    assert again.id == first.id
    assert store.occupancy() == {101: 1}
    assert [e.kind for e in store.changelog] == ["booked", "book-idempotent"]


def test_换个时刻的重复提交不算幂等() -> None:
    store = AppointmentStore()
    a = booked(store, 104, at(11))
    b = booked(store, 104, at(13))
    assert a.id != b.id


def test_履约释放占用() -> None:
    store = AppointmentStore()
    i = booked(store, 101, at(10)).id
    assert store.staff()[0].current_load == emp(101).current_load + 1
    store.visit(i, resolved=True, followup_note="换了令牌，已验证", now=NOW)
    assert store.occupancy() == {}
    assert store.staff()[0].current_load == emp(101).current_load


def test_取消释放占用() -> None:
    store = AppointmentStore()
    i = booked(store, 101, at(10)).id
    store.cancel(i, reason="用户改主意", now=NOW)
    assert store.occupancy() == {}
    assert store.staff()[0].current_load == emp(101).current_load


def test_状态机不让已结束的预约再走一遍() -> None:
    store = AppointmentStore()
    i = booked(store, 101, at(10)).id
    store.cancel(i, reason="用户改主意", now=NOW)
    with pytest.raises(InvalidBooking) as exc:
        store.visit(i, resolved=True, followup_note="其实去了", now=NOW)
    assert "不能重复登记到场" in str(exc.value)
    with pytest.raises(InvalidBooking):
        store.reschedule(i, start=at(14), now=NOW)
    with pytest.raises(InvalidBooking):
        store.cancel(i, reason="再取消一次", now=NOW)


def test_预约与改期都不能落到过去() -> None:
    store = AppointmentStore()
    with pytest.raises(InvalidBooking) as exc:
        booked(store, 101, at(11, day=1))  # 10-01 在 NOW(10-08) 之前
    assert "过去" in str(exc.value)
    i = booked(store, 101, at(10)).id
    with pytest.raises(InvalidBooking) as exc:
        store.reschedule(i, start=at(9), now=at(12))
    assert "过去" in str(exc.value)
    assert store.get(i).start == at(10)


def test_回访记录不能留空且结论落在预约上() -> None:
    store = AppointmentStore()
    i = booked(store, 101, at(10)).id
    with pytest.raises(InvalidBooking) as exc:
        store.visit(i, resolved=True, followup_note="   ", now=NOW)
    assert "回访记录不能为空" in str(exc.value)
    assert store.get(i).status is AppointmentStatus.BOOKED
    assert store.get(i).resolved is None
    appt = store.visit(i, resolved=False, followup_note="网络正常，接口仍 401", now=NOW)
    assert (appt.status, appt.resolved, appt.followup_note) == (
        AppointmentStatus.VISITED,
        False,
        "网络正常，接口仍 401",
    )


def test_改期失败时原预约一个字段都不动() -> None:
    store = AppointmentStore()
    i = booked(store, 101, at(10), service_id=2001).id
    with pytest.raises(InvalidBooking):
        store.reschedule(i, start=at(19), now=NOW)  # 窗口外
    appt = store.get(i)
    assert (appt.start, appt.engineer_id, appt.duration_minutes) == (at(10), 101, 60)
    assert [e.kind for e in store.changelog] == ["booked"]


def test_改期在同一把闸里重算而不是先取消再重订() -> None:
    store = AppointmentStore()
    i = booked(store, 101, at(10)).id
    j = booked(store, 101, at(12)).id
    # 挪到另一格的时刻上要撞冲突闸：ignore_id 只豁免自己这一条
    with pytest.raises(InvalidBooking) as exc:
        store.reschedule(i, start=at(12), now=NOW)
    assert "时段冲突" in str(exc.value)
    # 挪回自己原来的时刻是合法的（重算通过，占用不变）
    assert store.reschedule(i, start=at(10), now=NOW).start == at(10)
    assert store.reschedule(i, start=at(11), now=NOW).start == at(11)
    assert store.occupancy() == {101: 2}
    assert [e.kind for e in store.changelog] == ["booked", "booked", "rescheduled", "rescheduled"]
    assert f"#{j}" not in store.changelog[-1].detail


def test_改期可以换人并把两边都腾干净() -> None:
    store = AppointmentStore()
    i = booked(store, 101, at(10)).id
    store.reschedule(i, start=at(10), engineer_id=104, now=NOW)
    appt = store.get(i)
    assert (appt.engineer_id, appt.start) == (104, at(10))
    assert store.occupancy() == {104: 1}
    assert store.busy(101) == ()
    with pytest.raises(InvalidBooking) as exc:
        booked(store, 104, at(10, 30))
    assert "时段冲突" in str(exc.value)


def test_变更日志是事实源不是调试输出() -> None:
    """AutoDream 的增量回放读它（任务 #10），所以 kind 与时刻都是契约。"""
    store = AppointmentStore()
    i = booked(store, 101, at(10)).id
    store.visit(i, resolved=True, followup_note="已解决", now=NOW)
    assert [e.kind for e in store.changelog] == ["booked", "visited"]
    assert all(e.at is NOW for e in store.changelog)
    assert all(e.appointment_id == i for e in store.changelog)
    assert "上门 60 分钟" in store.changelog[0].detail
    assert "已解决" in store.changelog[1].detail


def test_待上门清单只看没履约的并按时间排() -> None:
    store = AppointmentStore()
    far = booked(store, 101, at(10, day=25)).id
    near = booked(store, 101, at(11, day=15)).id
    soon = booked(store, 102, at(10, day=13)).id
    store.visit(far, resolved=True, followup_note="提前处理了", now=NOW)
    assert [a.id for a in store.upcoming(now=at(8, day=12), within_days=7)] == [soon, near]
    assert [a.id for a in store.upcoming(now=at(8, day=12), within_days=2)] == [soon]


# --- 4. 占用视图与派单同源 --------------------------------------------------


def test_占用只叠在在职者身上() -> None:
    staff = with_overlay(employees(), {101: 2, 109: 3, 107: 1})
    by_id = {e.id: e for e in staff}
    assert by_id[101].current_load == emp(101).current_load + 2
    assert by_id[109].current_load == emp(109).current_load  # 离职者不接活
    assert by_id[107].current_load == emp(107).current_load + 1


async def test_提案与写入对还有没有余量给同一个答案() -> None:
    """两道闸共用一个视图才算数：约满的人两边都拒，还剩小格的人两边都放行。"""
    store = AppointmentStore()
    booked(store, 111, at(10))  # 2/3，满了
    assert store.propose(need="x", earliest=at(9), latest=at(18), engineer_ids=(111,), now=NOW) == ()
    with pytest.raises(InvalidBooking):
        booked(store, 111, at(11))

    booked(store, 101, at(9))  # 2/5，两格之后还剩最后一格
    booked(store, 101, at(10))
    assert store.occupancy() == {111: 1, 101: 2}  # 同一个 store：满着的人也在账上
    openings = store.propose(
        need="x", earliest=at(9), latest=at(18), engineer_ids=(101,), now=NOW
    )
    assert [o.start for o in openings] == [at(11)]
    booked(store, 101, at(11))  # 写入闸也放行 —— 两边答案一致
    assert store.occupancy()[101] == 3
    assert store.propose(need="x", earliest=at(9), latest=at(18), engineer_ids=(101,), now=NOW) == ()


async def test_派单读的是同一个带占用的名册视图() -> None:
    """`tools.match_service` → `recall_employees(roster=ctx.appointments.staff())`：
    派单提示词里的负载数字必须和预约侧刚写下的占用同步，否则同一句话有两个答案。"""
    store = AppointmentStore()
    booked(store, 101, at(10))
    overlaid = await recall_employees(FakeIndex(), "下单接口 401", (), roster=store.staff())
    baseline = await recall_employees(FakeIndex(), "下单接口 401", (), roster=None)
    assert [c.employee.current_load for c in overlaid] == [3]
    assert [c.employee.current_load for c in baseline] == [2]


# --- 5. 时段枚举 -----------------------------------------------------------


def test_时段扫描只给窗口内的格子并受步长约束() -> None:
    openings = openings_for(emp(101), earliest=at(8), latest=at(19), duration_minutes=60, limit=99)
    assert [o.start for o in openings[:3]] == [at(9), at(10), at(11)]
    assert openings[-1].start == at(17)  # 18:00 收工，17:00–18:00 是最后一格
    assert len(openings) == 9
    assert all(o.duration_minutes == 60 and o.engineer.id == 101 for o in openings)


def test_时段扫描跳过与已有预约交叠的格子() -> None:
    openings = openings_for(
        emp(101),
        earliest=at(9),
        latest=at(13),
        duration_minutes=60,
        busy=((at(10), at(11)),),
        limit=99,
    )
    assert [o.start for o in openings] == [at(9), at(11), at(12)]


def test_扫格子不会越过窗口的上下沿() -> None:
    late = openings_for(emp(104), earliest=at(9), latest=at(19), duration_minutes=60, limit=99)
    assert late[0].start == at(10)  # 安全组 10:00 才站人
    assert all(at(10) <= o.start and o.end <= at(18) for o in late)
    assert openings_for(emp(101), earliest=at(18), latest=at(23), duration_minutes=60) == ()


def test_没有窗口的组扫不出任何格子() -> None:
    for emp_id in (107, 108, 120, 121):
        assert on_site_windows(emp(emp_id)) == ()
        assert openings_for(emp(emp_id), earliest=at(9), latest=at(18), duration_minutes=60) == ()


def test_提案拒绝非档位时长与空搜索区间() -> None:
    store = AppointmentStore()
    with pytest.raises(InvalidBooking):
        store.propose(need="x", earliest=at(9), latest=at(18), duration_minutes=90, now=NOW)
    with pytest.raises(InvalidBooking) as exc:
        store.propose(need="x", earliest=at(14), latest=at(14), now=NOW)
    assert "没有可搜索的窗口" in str(exc.value)


def test_提案最多给三个格子并按时间先后() -> None:
    store = AppointmentStore()
    openings = store.propose(need="x", earliest=at(8), latest=at(19), duration_minutes=60, now=NOW)
    assert len(openings) == PROPOSAL_LIMIT
    assert [o.start for o in openings] == sorted(o.start for o in openings)
    # 运维组（08:00–20:00）是全名册唯一 8 点开门的组，所以最早三格都落在 8 点；
    # 同一时刻的先后由 senior 优先决定，不是靠名册顺序。
    assert [o.start for o in openings] == [at(8)] * PROPOSAL_LIMIT
    assert {o.engineer.id for o in openings} <= {103, 115, 116, 123}
    assert [o.engineer.level.value for o in openings] == ["senior", "mid", "mid"]
    assert all(o.engineer.active and on_site_windows(o.engineer) for o in openings)


def test_提案里看不到已被占用的小格() -> None:
    store = AppointmentStore()
    booked(store, 101, at(10))
    openings = store.propose(
        need="x",
        earliest=at(9),
        latest=at(12),
        engineer_ids=(101, 104),
        now=NOW,
        per_person=2,
    )
    assert [(o.engineer.id, o.start) for o in openings] == [
        (101, at(9)),
        (104, at(10)),
        (101, at(11)),
    ]  # 104 的 11:00 被 PROPOSAL_LIMIT 截掉，101 的 10:00 被占用挡掉


def test_预约槽位的阻塞与可选分得开() -> None:
    def filled(*names: str) -> tuple[ExtractedSlot, ...]:
        return tuple(ExtractedSlot(name=n, quote="用户原话") for n in names)

    assert missing_appointment_slots(filled()) == ("visit_time", "visit_duration")
    assert missing_appointment_slots(filled("visit_time")) == ("visit_duration",)
    assert missing_appointment_slots(filled("visit_time", "visit_duration")) == ()
    assert missing_appointment_slots(filled("service_preference")) == (
        "visit_time",
        "visit_duration",
    )
    # 空引文等于没填
    assert missing_appointment_slots((ExtractedSlot(name="visit_time", quote="  "),)) == (
        "visit_time",
        "visit_duration",
    )


def test_可追问的每一个槽位都有人话措辞() -> None:
    """真机 2026-09-27 20:15：预约三槽进了 ask_user 的枚举，模型就把
    "请补充：visit_time" 原样念给了用户 —— 那个回落分支是给日志用的，不该出现在问句里。
    判据取工具自己生成的 schema（P5 的教训：别拿自己抄的字面量验自己）。"""
    enum = AskUserInput.model_json_schema()["properties"]["slots"]["items"]["enum"]
    assert {"visit_time", "visit_duration", "service_preference"} <= set(enum)
    for name in enum:
        assert slot_label(name) != f"请补充：{name}", f"{name} 没有措辞，会说给用户听"
    # 枚举越宽，qwen3 越倾向一次全问出口（同一轮实测：4 条 → 8 条）。这条是观测到的宽度。
    assert len(enum) == 8


# --- 6. 工具层：模型递进来的时刻方言与错误话术 ------------------------------


class _State:
    """框架注入的 AgentState 的最小替身：`user_texts` 只读 context 与 session_id。"""

    def __init__(self, *texts: str) -> None:
        self.context = [UserMsg(name="user", content=t) for t in texts]
        self.session_id = "sess-visit-1"


def _ctx(appointments: AppointmentStore | None = None):
    ctx = HelpdeskContext(
        index=FakeIndex(),
        store=TicketStore(),
        appointments=appointments or AppointmentStore(),
    )
    return ctx, {t.name: t for t in build_tools(ctx)}


def _tomorrow(hour: int, minute: int = 0) -> datetime:
    day = (datetime.now() + timedelta(days=1)).replace(hour=0, minute=0, second=0, microsecond=0)
    return day.replace(hour=hour, minute=minute)


def _iso(moment: datetime) -> str:
    return f"{moment:%Y-%m-%d %H:%M}"


async def test_工具_查时段是只读的不占人() -> None:
    ctx, tools = _ctx()
    args = dict(
        need="现场排查下单接口 401",
        earliest=_iso(_tomorrow(9)),
        latest=_iso(_tomorrow(18)),
        duration_minutes=60,
    )
    first = await tools["propose_appointments"].call(**args)
    again = await tools["propose_appointments"].call(**args)
    assert first.metadata["empty"] is False
    assert first.metadata["openings"] == again.metadata["openings"]
    assert "负载=" in first.content[0].text
    assert "book_appointment" in first.content[0].text  # 话术要求先给用户挑
    assert ctx.appointments.occupancy() == {}
    assert ctx.appointments.changelog == []


async def test_工具_约不到时明说没有并给出路而不是编一个() -> None:
    _ctx_, tools = _ctx()
    chunk = await tools["propose_appointments"].call(
        need="上门修打印机",
        earliest=_iso(_tomorrow(9)),
        latest=_iso(_tomorrow(18)),
        duration_minutes=60,
        engineer_ids=[120],  # 综合组，没有上门窗口
    )
    assert chunk.metadata == {"openings": [], "empty": True}
    assert chunk.state is ToolResultState.RUNNING  # 约不出来不是错误，是事实
    text = chunk.content[0].text
    assert "不要自己编一个时段出来" in text and "改成远程处理" in text


async def test_工具_时刻方言只剥时区后缀不做换算() -> None:
    """模型爱写 ISO：T、秒、+08:00、Z 都得认，但都不换算 —— 上门比的是本地墙钟。"""
    ctx, tools = _ctx()
    moment = _tomorrow(10)
    for raw in (
        f"{moment:%Y-%m-%d}T10:00:00+08:00",
        f"{moment:%Y-%m-%d}T10:00:00Z",
        f"{moment:%Y-%m-%d}T10:00",
        f"{moment:%Y-%m-%d} 10:00:00",
        f" {moment:%Y-%m-%d} 10:00 ",
    ):
        chunk = await tools["book_appointment"].call(
            engineer_id=101, start=raw, duration_minutes=60, need="现场排查", slots=[]
        )
        assert chunk.metadata["start"] == _iso(moment), raw
    assert len(ctx.appointments.appointments) == 1  # 五种写法是同一条预约


async def test_工具_看不懂的时刻报错而不是猜一个() -> None:
    ctx, tools = _ctx()
    chunk = await tools["book_appointment"].call(
        engineer_id=101, start="下周二下午三点", duration_minutes=60, need="现场排查", slots=[]
    )
    assert chunk.state is ToolResultState.ERROR
    assert "本地时刻" in chunk.content[0].text
    assert ctx.appointments.appointments == {}


async def test_工具_查时段也认方言并且拒绝非档位() -> None:
    _ctx_, tools = _ctx()
    chunk = await tools["propose_appointments"].call(
        need="现场排查",
        earliest=f"{_tomorrow(9):%Y-%m-%d}T09:00:00+08:00",
        latest=f"{_tomorrow(18):%Y-%m-%d}T18:00:00+08:00",
        duration_minutes=45,
    )
    assert chunk.state is ToolResultState.ERROR
    assert "上门时长只能是" in chunk.content[0].text
    assert chunk.metadata["openings"] == []


async def test_工具_落约把会话与槽位证据一起记下来() -> None:
    ctx, tools = _ctx()
    chunk = await tools["book_appointment"].call(
        engineer_id=101,
        start=_iso(_tomorrow(10)),
        duration_minutes=60,
        need="现场排查下单接口 401",
        service_id=2001,
        slots=[
            {"name": "visit_time", "quote": "10 月 12 号上午十点前后"},
            {"name": "visit_duration", "quote": "大概要一个小时"},
        ],
        _agent_state=_State("希望 10 月 12 号上午十点前后有人上门，大概要一个小时"),
    )
    appt = ctx.appointments.get(chunk.metadata["appointment_id"])
    assert appt.conversation_id == "sess-visit-1"
    assert appt.service_id == 2001
    assert chunk.metadata["missing_info"] == []
    assert chunk.metadata["unverified"] == []
    assert chunk.metadata["already_booked"] is False


async def test_工具_证据查不到就按未填记账但不拦这次落约() -> None:
    ctx, tools = _ctx()
    chunk = await tools["book_appointment"].call(
        engineer_id=101,
        start=_iso(_tomorrow(10)),
        duration_minutes=60,
        need="现场排查下单接口 401",
        slots=[{"name": "visit_time", "quote": "下周三整天都行，你定"}],
        _agent_state=_State("下单接口报 401，帮我看看"),
    )
    assert chunk.state is ToolResultState.RUNNING
    assert set(chunk.metadata["missing_info"]) == {"visit_time", "visit_duration"}
    assert chunk.metadata["unverified"] == ["visit_time"]
    assert "查不到" in chunk.content[0].text
    assert set(ctx.appointments.get(chunk.metadata["appointment_id"]).missing_info) == {
        "visit_time",
        "visit_duration",
    }


async def test_工具_重复落约回同一条并告诉模型没有二次占用() -> None:
    ctx, tools = _ctx()
    kwargs = dict(
        engineer_id=101,
        start=_iso(_tomorrow(10)),
        duration_minutes=60,
        need="现场排查",
        slots=[],
    )
    await tools["book_appointment"].call(**kwargs)
    chunk = await tools["book_appointment"].call(**kwargs)
    assert chunk.metadata["already_booked"] is True
    assert ctx.appointments.occupancy() == {101: 1}
    assert "没有二次占用" in chunk.content[0].text


async def test_工具_改期撞闸时给出拒绝原因且预约不动() -> None:
    ctx, tools = _ctx()
    created = await tools["book_appointment"].call(
        engineer_id=101, start=_iso(_tomorrow(10)), duration_minutes=60, need="现场排查", slots=[]
    )
    appt_id = created.metadata["appointment_id"]
    refused = await tools["reschedule_appointment"].call(
        appointment_id=appt_id, start=_iso(_tomorrow(21))
    )
    assert refused.state is ToolResultState.ERROR
    assert "不在" in refused.content[0].text
    assert ctx.appointments.get(appt_id).start == _tomorrow(10)
    moved = await tools["reschedule_appointment"].call(
        appointment_id=appt_id, start=_iso(_tomorrow(14))
    )
    assert moved.metadata["start"] == _iso(_tomorrow(14))
    assert "变更日志里留着原来的时段" in moved.content[0].text


async def test_工具_关联的工单不存在时照实说() -> None:
    ctx, tools = _ctx()
    created = await tools["book_appointment"].call(
        engineer_id=101,
        start=_iso(_tomorrow(10)),
        duration_minutes=60,
        need="现场排查",
        ticket_id=99,
        slots=[],
    )
    appt_id = created.metadata["appointment_id"]
    chunk = await tools["close_appointment"].call(
        appointment_id=appt_id, resolved=False, followup_note="令牌已换，接口仍 401"
    )
    assert chunk.metadata == {
        "appointment_id": appt_id,
        "resolved": False,
        "ticket_id": 99,
        "freed_load": True,
    }
    assert ctx.appointments.occupancy() == {}
    assert "不存在" in chunk.content[0].text
    assert ctx.store.progress == []


async def test_工具_回访写进真实工单进展() -> None:
    ctx, tools = _ctx()
    ticket = ctx.store.create(
        title="下单接口 401",
        description="线上下单接口持续返回 401",
        category=Category.INCIDENT,
        priority=Priority.P1,
    ).ticket
    created = await tools["book_appointment"].call(
        engineer_id=101,
        start=_iso(_tomorrow(10)),
        duration_minutes=60,
        need="现场排查",
        ticket_id=ticket.id,
        slots=[],
    )
    appt_id = created.metadata["appointment_id"]
    await tools["close_appointment"].call(
        appointment_id=appt_id, resolved=True, followup_note="换了令牌，用户验证通过"
    )
    notes = [p.content for p in ctx.store.progress_of(ticket.id)]
    assert any("上门回访：换了令牌，用户验证通过" in n for n in notes)
    assert ctx.appointments.get(appt_id).resolved is True
    assert ctx.appointments.get(appt_id).status is AppointmentStatus.VISITED


async def test_工具_待上门清单空的时候也提醒别凭记忆答() -> None:
    _ctx_, tools = _ctx()
    chunk = await tools["upcoming_appointments"].call(within_days=7)
    assert chunk.metadata == {"appointments": []}
    assert "别凭记忆答" in chunk.content[0].text
    await tools["book_appointment"].call(
        engineer_id=101, start=_iso(_tomorrow(10)), duration_minutes=60, need="现场排查", slots=[]
    )
    chunk = await tools["upcoming_appointments"].call(within_days=7)
    assert [a[1] for a in chunk.metadata["appointments"]] == [101]


async def test_工具_占用人之后派单提示词里的负载立刻同步() -> None:
    ctx, tools = _ctx()
    before = await tools["match_service"].call(query="下单接口 401。token 过期")
    assert "负载=2/5" in before.content[0].text
    await tools["book_appointment"].call(
        engineer_id=101, start=_iso(_tomorrow(10)), duration_minutes=60, need="现场排查", slots=[]
    )
    after = await tools["match_service"].call(query="下单接口 401。token 过期")
    assert "负载=3/5" in after.content[0].text
    assert "负载=2/5" not in render_candidates(ctx.evidence.candidates)
