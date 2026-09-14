#!/usr/bin/env python3
"""生成「对抗样本」指派评测集。

背景：基础集（assignment.json）120 条是在等权规则下推导的，
实测发现把技能权重置零后评测仍全部通过 —— 也就是说基础集
无法区分「按技能匹配」与「随机分配 + 负载均衡」。

本脚本生成一批真正有区分力的样本：每个样本都安排一个
「技能无关但很闲」的竞争者，只有当技能权重真正生效时才选得对。
若把技能权重置零，这批样本必然大面积失败。

评分规则（与 internal/assign/assigner.go 的 DefaultWeights 一致）：
  score = 0.60 * jaccard(需求技能, 员工技能)
        + 0.20 * (1 - load/max_concurrent)
        + 0.20 * recency
候选过滤：active 且 load < max_concurrent
选择：score 降序 → load_score 降序 → recency 降序 → id 升序

用法：
  python3 eval/gen_hard_dataset.py            # 仅校验，不写文件
  python3 eval/gen_hard_dataset.py --write    # 生成并校验后写入
"""

import json
import os
import sys
from itertools import product

SKILL_MAP = {
    1: "网络", 2: "接口", 3: "数据库", 4: "性能", 5: "安全", 6: "前端",
    7: "客户端", 8: "部署", 9: "账号权限", 10: "计费", 11: "数据同步", 12: "硬件",
}

W_SKILL, W_LOAD, W_RECENCY = 0.60, 0.20, 0.20


def jaccard(a, b):
    sa, sb = set(a), set(b)
    if not sa or not sb:
        return 0.0
    return len(sa & sb) / len(sa | sb)


def load_ratio(emp):
    mc = emp["maxConcurrent"]
    if mc <= 0:
        return 0.0
    return min(max(emp["currentLoad"] / mc, 0.0), 1.0)


def score(req_skills, emp):
    return (
        W_SKILL * jaccard(req_skills, emp["skillIds"])
        + W_LOAD * (1 - load_ratio(emp))
        + W_RECENCY * emp["recency"]
    )


def assign(req_skills, employees):
    """返回 (winner_id, no_match)。完全按规则实现，用于推导并校验金标。"""
    viable = [e for e in employees if e["active"] and e["currentLoad"] < e["maxConcurrent"]]
    if not viable:
        return 0, True
    ranked = sorted(
        viable,
        key=lambda e: (-score(req_skills, e), -(1 - load_ratio(e)), -e["recency"], e["id"]),
    )
    return ranked[0]["id"], False


def emp(eid, name, skills, load, maxc, recency=0.7, active=True):
    return {
        "id": eid,
        "name": name,
        "active": active,
        "currentLoad": load,
        "maxConcurrent": maxc,
        "recency": recency,
        "skillIds": list(skills),
    }


CATEGORIES = ["incident", "request", "consultation", "change"]
PRIORITIES = ["P0", "P1", "P2", "P3"]

# (场景, 标题, 需求技能, 员工列表)
SPECS = [
    # --- 技能主导：对手零技能且很闲，技能匹配者更忙却必须胜出 ---
    ("skill_dominates_idle_beginner", "接口返回 500，线上订单创建失败", [2, 3],
     [emp(101, "接口组-张伟", [2, 3, 11], 2, 5, 0.5),
      emp(102, "实习坐席-李娜", [6], 0, 5, 0.9)]),
    ("skill_dominates_idle_beginner", "数据库连接池耗尽导致查询超时", [3, 4],
     [emp(101, "数据库组-王强", [3, 4], 3, 5, 0.4),
      emp(102, "通用坐席-赵敏", [8], 0, 5, 0.95)]),
    ("skill_dominates_idle_beginner", "部署后静态资源 404", [8, 6],
     [emp(101, "运维组-陈磊", [8, 6], 2, 4, 0.5),
      emp(102, "通用坐席-孙悦", [10], 0, 4, 0.9)]),
    ("skill_dominates_idle_beginner", "账号被锁定无法登录后台", [9],
     [emp(101, "权限组-周涛", [9, 5], 3, 6, 0.4),
      emp(102, "通用坐席-吴迪", [4], 0, 6, 0.95)]),
    ("skill_dominates_idle_beginner", "计费账单金额与合同不一致", [10],
     [emp(101, "计费组-郑爽", [10, 11], 2, 4, 0.5),
      emp(102, "通用坐席-冯磊", [7], 0, 4, 0.9)]),

    # --- 负载对抗：技能匹配者负载高，但对手零技能，技能仍须胜出 ---
    ("skill_overcomes_heavy_load", "客户端启动闪退，影响所有安卓用户", [7],
     [emp(201, "客户端组-何静", [7, 4], 4, 5, 0.5),
      emp(202, "空闲坐席-许峰", [12], 0, 5, 0.95)]),
    ("skill_overcomes_heavy_load", "数据同步任务持续失败", [11],
     [emp(201, "数据组-邓超", [11, 3], 4, 5, 0.4),
      emp(202, "空闲坐席-曾毅", [1], 0, 5, 0.95)]),
    ("skill_overcomes_heavy_load", "硬件网关频繁掉线", [12],
     [emp(201, "硬件组-范冰", [12], 4, 5, 0.5),
      emp(202, "空闲坐席-方磊", [6], 0, 5, 0.9)]),

    # --- 响应对抗：技能匹配者最近响应差，对手零技能但响应优 ---
    ("skill_overcomes_poor_recency", "安全扫描报告存在高危漏洞", [5],
     [emp(301, "安全组-袁泉", [5, 8], 1, 4, 0.10),
      emp(302, "新鲜坐席-崔健", [2], 0, 4, 1.00)]),
    ("skill_overcomes_poor_recency", "前端页面白屏，控制台报错", [6],
     [emp(301, "前端组-钱枫", [6, 7], 1, 4, 0.10),
      emp(302, "新鲜坐席-汤唯", [9], 0, 4, 1.00)]),

    # --- 弱重叠：技能匹配者只有部分重叠，对手完全无关 ---
    ("weak_overlap_beats_unrelated", "接口偶发超时，需要链路排查", [2, 4],
     [emp(401, "接口组-姚明", [2], 1, 4, 0.6),
      emp(402, "无关坐席-刘翔", [12], 0, 4, 0.9)]),
    ("weak_overlap_beats_unrelated", "数据库慢查询导致报表加载缓慢", [3, 4],
     [emp(401, "数据库组-李宁", [3], 1, 4, 0.6),
      emp(402, "无关坐席-孙杨", [10], 0, 4, 0.9)]),
    ("weak_overlap_beats_unrelated", "移动端无法上传附件", [7],
     [emp(401, "客户端组-张怡宁", [7], 2, 4, 0.6),
      emp(402, "无关坐席-马龙", [5], 0, 4, 0.9)]),
    ("weak_overlap_beats_unrelated", "用户权限变更未生效", [9],
     [emp(401, "权限组-林丹", [9], 2, 4, 0.6),
      emp(402, "无关坐席-邹市明", [1], 0, 4, 0.9)]),

    # --- 双侧竞争：两个技能匹配者，专才（覆盖全）对通才（技能多但覆盖低） ---
    ("specialist_beats_generalist", "核心接口全链路超时", [2, 3],
     [emp(501, "接口专才-高峰", [2, 3], 2, 5, 0.6),
      emp(502, "全栈通才-黄磊", [2, 3, 5, 6, 7, 8], 2, 5, 0.6)]),
    ("specialist_beats_generalist", "部署脚本执行失败导致回滚", [8],
     [emp(501, "部署专才-文章", [8], 1, 5, 0.6),
      emp(502, "全栈通才-白百何", [8, 1, 2, 3, 4, 5, 6, 7], 1, 5, 0.6)]),
]

TITLES_EXTRA = [
    ("specialist_beats_generalist", "密码重置邮件收不到", [9],
     [emp(501, "权限专才-陈坤", [9], 2, 5, 0.6),
      emp(502, "全栈通才-周迅", [9, 1, 2, 3, 4, 5, 6], 2, 5, 0.6)]),
    ("specialist_beats_generalist", "账单导出功能报错", [10, 11],
     [emp(501, "计费专才-徐静蕾", [10, 11], 1, 5, 0.6),
      emp(502, "全栈通才-李冰冰", [10, 11, 1, 2, 3, 4, 5, 6, 7, 8], 1, 5, 0.6)]),
    ("skill_dominates_idle_beginner", "网关设备离线告警", [12, 1],
     [emp(101, "硬件网络组-葛优", [12, 1], 3, 6, 0.5),
      emp(102, "通用坐席-姜文", [7], 0, 6, 0.95)]),
    ("skill_overcomes_heavy_load", "订单状态同步延迟超过一小时", [11, 2],
     [emp(201, "同步组-巩俐", [11, 2], 5, 6, 0.4),
      emp(202, "空闲坐席-章子怡", [6], 0, 6, 0.95)]),
    ("skill_overcomes_poor_recency", "前端登录页样式错乱", [6, 4],
     [emp(301, "前端组-王菲", [6, 4], 2, 5, 0.15),
      emp(302, "新鲜坐席-那英", [3], 0, 5, 1.00)]),
    ("weak_overlap_beats_unrelated", "安全策略误拦截正常请求", [5, 2],
     [emp(401, "安全组-刘德华", [5], 2, 5, 0.6),
      emp(402, "无关坐席-张学友", [12], 0, 5, 0.9)]),
    ("skill_dominates_idle_beginner", "性能监控显示内存持续增长", [4],
     [emp(101, "性能组-郭富城", [4, 3], 4, 6, 0.5),
      emp(102, "通用坐席-黎明", [9], 0, 6, 0.95)]),
    ("skill_overcomes_heavy_load", "客户端推送收不到", [7, 11],
     [emp(201, "客户端组-梁朝伟", [7, 11], 4, 5, 0.5),
      emp(202, "空闲坐席-刘嘉玲", [8], 0, 5, 0.95)]),
    ("weak_overlap_beats_unrelated", "数据库主从延迟告警", [3, 11],
     [emp(401, "数据库组-周星驰", [3], 1, 5, 0.6),
      emp(402, "无关坐席-吴孟达", [4], 0, 5, 0.9)]),
    ("skill_overcomes_poor_recency", "账号权限批量导入失败", [9, 3],
     [emp(301, "权限组-成龙", [9, 3], 2, 6, 0.20),
      emp(302, "新鲜坐席-洪金宝", [10], 0, 6, 0.95)]),
    ("skill_overcomes_heavy_load", "接口鉴权失败率上升", [2, 5],
     [emp(201, "接口安全组-李连杰", [2, 5], 5, 6, 0.5),
      emp(202, "空闲坐席-甄子丹", [6], 0, 6, 0.95)]),
    ("specialist_beats_generalist", "部署环境变量缺失导致启动失败", [8, 9],
     [emp(501, "部署专才-吴京", [8, 9], 3, 6, 0.6),
      emp(502, "全栈通才-沈腾", [8, 9, 1, 2, 3, 4, 5, 6, 7], 3, 6, 0.6)]),
    ("skill_dominates_idle_beginner", "计费周期切换后金额翻倍", [10, 3],
     [emp(101, "计费组-马丽", [10, 3], 2, 5, 0.5),
      emp(102, "通用坐席-贾玲", [1], 0, 5, 0.95)]),
    ("weak_overlap_beats_unrelated", "前端打包产物体积异常增大", [6, 4],
     [emp(401, "前端组-雷军", [6], 3, 6, 0.55),
      emp(402, "无关坐席-马云", [12], 0, 6, 0.9)]),
]

ALL_SPECS = SPECS + TITLES_EXTRA


def build():
    cases = []
    for idx, (scenario, title, req, employees) in enumerate(ALL_SPECS, start=1):
        winner, no_match = assign(req, employees)
        case = {
            "id": f"as-hard-{idx:03d}",
            "scenario": scenario,
            "ticket": {
                "title": title,
                "category": CATEGORIES[idx % len(CATEGORIES)],
                "priority": PRIORITIES[idx % len(PRIORITIES)],
                "requiredSkillIds": req,
            },
            "employees": employees,
            "expect": {
                "goldAssigneeId": winner,
                "noMatch": no_match,
                "rationale": build_rationale(req, employees, winner),
            },
        }
        cases.append(case)
    return cases


def build_rationale(req, employees, winner):
    parts = []
    for e in employees:
        parts.append(
            f"{e['name']}(id={e['id']}) Jaccard={jaccard(req, e['skillIds']):.3f} "
            f"负载={1 - load_ratio(e):.3f} 响应={e['recency']:.2f} → {score(req, e):.4f}"
        )
    return "; ".join(parts) + f" ⇒ 选中 {winner}"


def main():
    cases = build()
    # 自带校验：重新独立推理一遍金标，与生成结果比对。
    rebuild_ok = True
    for case in cases:
        winner, no_match = assign(case["ticket"]["requiredSkillIds"], case["employees"])
        if winner != case["expect"]["goldAssigneeId"] or no_match != case["expect"]["noMatch"]:
            rebuild_ok = False
            print(f"✗ {case['id']} 金标不一致")
    if not rebuild_ok:
        print("校验失败：金标不可复现")
        return 1

    # 对抗性检查：每个样本都必须有「零或弱技能但更闲/响应更好」的对手，
    # 否则该样本无区分力。
    weak = 0
    for case in cases:
        req = case["ticket"]["requiredSkillIds"]
        winner = case["expect"]["goldAssigneeId"]
        rival = [e for e in case["employees"] if e["id"] != winner]
        if any(jaccard(req, r["skillIds"]) == 0 for r in rival):
            weak += 1
    print(f"样本数 {len(cases)}；含零技能竞争的样本 {weak} 条")

    if "--write" in sys.argv:
        out = os.path.join(os.path.dirname(os.path.abspath(__file__)), "datasets", "assignment_hard.json")
        os.makedirs(os.path.dirname(out), exist_ok=True)
        payload = {
            "version": 1,
            "description": (
                "对抗样本集：每条都安排'技能无关但更闲/响应更好'的竞争者，"
                "仅当技能权重真正生效时才能选对。用于证明评测具备区分能力，"
                f"可与基础集合并或单独运行。技能编号见基础集；权重 skill={W_SKILL} load={W_LOAD} recency={W_RECENCY}。"
            ),
            "cases": cases,
        }
        with open(out, "w", encoding="utf-8") as f:
            json.dump(payload, f, ensure_ascii=False, indent=2)
            f.write("\n")
        print(f"已写入 {out}")
    else:
        print("（未写文件；加 --write 生成）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
