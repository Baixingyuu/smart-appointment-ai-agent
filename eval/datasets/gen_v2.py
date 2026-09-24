#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""生成 eval/datasets/assignment_v2.json —— 三段流水线的评测集。

设计：见 docs/DISPATCH_PIPELINE.md §5.2。

金标来源：三段全部规则/词典派生，不引入人工标注。
  · 抽取段 expectedServiceIds —— 每条 ticket 是我按服务字典手工改写的，
    正确答案在改写时就已锁定（serviceTop1 是这里唯一手工输入）；
  · 判弱段 + 排序段 —— 由本脚本 stage1() 复现 docs/DISPATCH_PIPELINE.md §1 的
    Stage 1 规则派生：对 serviceTop1 这一条可信命中，按花名册逐人算
    ownership/avail/seniority/recency 加权总分、跑 P0 senior 硬过滤、
    再用 top1-top2 margin 与 0.15 阈值判 low_margin。
    注意这是"给定抽取正确"前提下派生的理想金标：真实 BM25 若额外召回第二条
    过阈值服务，会改变 margin，从而与实际 pipeline 产生偏差——这类偏差正是要
    暴露的 ownership-leak 边界，不是 bug。
    （旧版这里只是查 ownership 表假设 owner 必赢，掩盖了 mid-owner / P0 过滤
      / 低 margin 场景，与实现不同源，故重做。）

工单文本来源：Console-AI/IT-helpdesk-synthetic-tickets (MIT)。
  每条 source.originId 指回原始英文工单，可追溯；中文措辞按当前 14 服务字典重写，
  不是翻译而是改编（保留原句式口语感、真实业务里的信息密度与噪声）。
  另有 ~10 条 handcrafted 边界样本压判弱段与过滤段。

外部真值语料 sanity-check：tasksource/it-support-tickets (CC-BY-4.0)。
  真工单常出现 "[TICKET ID] - xxx"、"Hi IT\\n\\n+cc 领导"、极短"登不上"等格式；
  v2 里的 handcrafted 边界样本按这些模式仿写，不复制文本。
"""

import json
from pathlib import Path

# ---------- Stage 1 模型（复现 internal/assign 的规则）----------
#
# 花名册来自 internal/seed/{seed.go,service_catalog.go}：
#   level/team/ownership grants 来自 EmployeeExtensions()，
#   recency 来自 Employees()。eval 的 seedDirectory() 把 CurrentLoad 全置 0，
#   所以 avail 恒为 1.0（对所有人等值，不影响排序，但为忠实起见仍计入总分）。
# 109 郑爽已离职（Active=false），eligibility 会过滤掉，这里不入候选池。
OWNER, BACKUP, TEAM, NONE = "owner", "backup", "team", "none"

SENIOR, MID, JUNIOR = "senior", "mid", "junior"

# id: (level, teamID, recency, {serviceID: OWNER|BACKUP})
ROSTER = {
    101: (SENIOR, 1, 0.85, {2001: OWNER, 2011: OWNER, 2013: OWNER, 2009: BACKUP}),
    102: (SENIOR, 2, 0.80, {2002: OWNER, 2009: OWNER, 2011: BACKUP}),
    103: (MID, 4, 0.75, {2005: OWNER, 2006: OWNER, 2012: OWNER, 2010: BACKUP}),
    104: (SENIOR, 6, 0.70, {2010: OWNER, 2003: BACKUP, 2006: BACKUP, 2013: BACKUP}),
    105: (MID, 5, 0.78, {2007: OWNER, 2008: OWNER}),
    106: (MID, 3, 0.72, {2003: OWNER, 2004: OWNER}),
    107: (SENIOR, 7, 0.60, {2014: OWNER, 2001: BACKUP, 2002: BACKUP, 2004: BACKUP, 2005: BACKUP}),
    108: (JUNIOR, 8, 0.95, {2007: BACKUP, 2008: BACKUP}),
}

# serviceID -> teamID，来自 Services()。用于 ownershipOf 的 team 兜底判定。
SERVICE_TEAMS = {
    2001: 1, 2002: 2, 2003: 3, 2004: 3, 2005: 4, 2006: 4, 2007: 5,
    2008: 5, 2009: 2, 2010: 6, 2011: 1, 2012: 4, 2013: 1, 2014: 7,
}

# 与 score.go / pipeline.go 对齐的常量。
OWN_SCORE = {OWNER: 1.0, BACKUP: 0.3, TEAM: 0.1, NONE: 0.0}
SEN_SCORE = {SENIOR: 1.0, MID: 0.5, JUNIOR: 0.0}
WEIGHTS = {"ownership": 0.40, "similar": 0.20, "avail": 0.20,
           "seniority": 0.10, "recency": 0.10}
MARGIN_THRESHOLD = 0.15  # DefaultPipelineConfig 里的判弱阈值


def _ownership_kind(emp_id, service_id):
    """复现 ownershipOf：直接归属优先，否则同 team 记 team，都不满足记 none。"""
    _, team, _, grants = ROSTER[emp_id]
    if service_id in grants:
        return grants[service_id]
    if team != 0 and SERVICE_TEAMS.get(service_id) == team:
        return TEAM
    return NONE


def _eligible(emp_id, priority):
    """复现 eligibilityCheck：在职 + 有并发（load 0 恒满足）+ P0 需 senior。"""
    level = ROSTER[emp_id][0]
    if priority == "P0" and level != SENIOR:
        return False
    return True


def _total(emp_id, service_id):
    """复现 scoreAll：五特征加权和（求和顺序与 Go 一致，保证浮点逐位相同）。"""
    level, _, recency, _ = ROSTER[emp_id]
    own = OWN_SCORE[_ownership_kind(emp_id, service_id)]
    similar = 0.0          # NoopSimilarIndex
    avail = 1.0            # eval 里 CurrentLoad 全 0
    senior = SEN_SCORE[level]
    recent = recency
    return (WEIGHTS["ownership"] * own + WEIGHTS["similar"] * similar +
            WEIGHTS["avail"] * avail + WEIGHTS["seniority"] * senior +
            WEIGHTS["recency"] * recent)


def stage1(service_id, priority):
    """给定抽取正确（可信命中=service_id），派生 (weakness, outcome, assignee_id)。

    与 Run() 的 Stage 1 判定对齐：
      · 无可过滤后候选人 → empty_candidates → no_candidate（离线 chooser 也救不了）
      · margin < 0.15 → low_margin；本 runner 默认离线（chooser 缺失）→ Stage3 → fallback_pool
      · 否则 none → Stage1 直出 → matched，assignee = top1
    service_id == 0 表示抽取段没解出（人工标 no_service_match）→ 判 no_service_match → fallback_pool。
    """
    if service_id == 0:
        return "no_service_match", "fallback_pool", 0

    cands = [e for e in ROSTER if _eligible(e, priority)]
    if not cands:
        return "empty_candidates", "no_candidate", 0

    # 稳定排序：Total desc，再 avail desc（恒等），再 recency desc，最后 id asc。
    ranked = sorted(
        cands,
        key=lambda e: (-_total(e, service_id), -1.0, -ROSTER[e][2], e),
    )
    if len(ranked) >= 2:
        margin = _total(ranked[0], service_id) - _total(ranked[1], service_id)
        if margin < MARGIN_THRESHOLD:
            return "low_margin", "fallback_pool", 0
    return "none", "matched", ranked[0]




CASES = [
    # ============ 2001 核心下单接口 (owner=101 senior) ============
    {
        "source": {"kind": "console-ai", "originId": "86eza0fwq", "originCategory": "Software",
                   "originPriority": "High",
                   "originSubject": "Software Conflict Causing App Crashes"},
        "ticket": {"title": "下单接口频繁 500，客户端拿不到订单号",
                   "description": "从今天上午 10:20 起，线上下单接口持续返回 500，"
                                  "订单无法创建。已经排查网络与机房链路都正常，怀疑与最近的下单服务发布相关。"
                                  "错误率约 35%，全部用户都受影响。",
                   "category": "incident", "priority": "P1"},
        "expected": {"serviceTop1": 2001, "weakness": "none", "outcome": "matched"},
        "notes": "服务名字面出现 '下单接口' 与 '订单' + 症状 500，BM25 top-1 应命中 2001；owner 101 senior 直派。",
    },
    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "订单创建接口 P0 挂了",
                   "description": "线上环境下单服务大面积失败，客服反馈订单创建不进去。",
                   "category": "incident", "priority": "P0"},
        "expected": {"serviceTop1": 2001, "weakness": "none", "outcome": "matched"},
        "notes": "P0 + owner 是 senior → 直派 101。验证 P0-senior gate 不误伤。",
    },
    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "订单一直转圈",
                   "description": "客户下单一直提交不成功，页面转圈没报错。",
                   "category": "incident", "priority": "P2"},
        "expected": {"serviceTop1": 2001, "weakness": "none", "outcome": "matched"},
        "notes": "无 '接口' 关键字，靠 '订单' + '下单' 语义命中；压 BM25 别名覆盖。",
    },

    # ============ 2002 报表与 BI (owner=102 senior) ============
    {
        "source": {"kind": "console-ai", "originId": "f3czsohfi", "originCategory": "Performance",
                   "originPriority": "High",
                   "originSubject": "Enterprise app performance monitoring issue"},
        "ticket": {"title": "月度经营看板加载 30 秒还没出来",
                   "description": "运营侧同事反馈，从昨晚开始月报看板打开很慢，"
                                  "有时直接超时。临时导出一份数据也报 '查询超时'。",
                   "category": "incident", "priority": "P2"},
        "expected": {"serviceTop1": 2002, "weakness": "none", "outcome": "matched"},
        "notes": "命中关键词 '月报' + '看板' + '查询'；owner 102 senior。",
    },
    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "BI 报表导出 Excel 打不开",
                   "description": "客户报导出的 xlsx 打不开，重导几次都是这样。",
                   "category": "incident", "priority": "P3"},
        "expected": {"serviceTop1": 2002, "weakness": "none", "outcome": "matched"},
        "notes": "短文本 + 'BI' + '报表' + '导出'；P3 不启用 P0 gate。",
    },

    # ============ 2003 用户账号中心 (owner=106 mid, backup=104 senior) ============
    {
        "source": {"kind": "console-ai", "originId": "u43q44ro8", "originCategory": "Account",
                   "originPriority": "Medium",
                   "originSubject": "Assistance Needed: Password Reset for Email Account"},
        "ticket": {"title": "账号被锁定，无法登录",
                   "description": "同事本人反馈：连续输错三次密码后账号被锁，"
                                  "现在登录页显示 '账号已锁定，请联系管理员'。已经确认密码是对的。",
                   "category": "request", "priority": "P3"},
        "expected": {"serviceTop1": 2003, "weakness": "none", "outcome": "matched"},
        "notes": "命中 '账号' + '登录' + '锁定'；owner 106 mid 非 P0 时可用。",
    },
    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "核心用户全部登录不上，P0 事故",
                   "description": "所有客户登录接口返回 401，客服排队工单 200+。",
                   "category": "incident", "priority": "P0"},
        "expected": {"serviceTop1": 2003, "weakness": "none", "outcome": "matched",
                     "assigneeOverride": 104},
        "notes": "P0 + owner 106 是 mid → 走 backup 104 senior。压 P0-senior gate 走 backup 分支。",
    },
    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "登不上",
                   "description": "登不上，急。",
                   "category": "incident", "priority": "P2"},
        "expected": {"serviceTop1": 2003, "weakness": "none", "outcome": "matched"},
        "notes": "仿 tasksource 真工单里的极短文本。BM25 靠别名 '登录' 单点命中，压最低信息量。",
    },

    # ============ 2004 计费与账单 ============
    {
        "source": {"kind": "console-ai", "originId": "zuj1ksh92", "originCategory": "Licensing",
                   "originPriority": "High",
                   "originSubject": "Software License Activation Failures"},
        "ticket": {"title": "订阅续费扣款成功但账单未生成",
                   "description": "财务反馈：本批次 40+ 家客户订阅续费扣款都到账，"
                                  "但账单系统里查不到对应单据，客户催发票。",
                   "category": "incident", "priority": "P2"},
        "expected": {"serviceTop1": 2004, "weakness": "none", "outcome": "matched"},
        "notes": "'账单' + '扣费' + '订阅' + '发票' 全部命中；owner 106 mid。",
    },
    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "客户咨询：发票开错了能不能重开",
                   "description": "客户来电说上月发票抬头开错，想重开。",
                   "category": "request", "priority": "P3"},
        "expected": {"serviceTop1": 2004, "weakness": "none", "outcome": "matched"},
        "notes": "'发票' 关键词单点，request 类型。",
    },

    # ============ 2005 网络与机房链路 ============
    {
        "source": {"kind": "console-ai", "originId": "fs7blkcux", "originCategory": "Network",
                   "originPriority": "High",
                   "originSubject": "Alert: Inconsistent VPN Connectivity"},
        "ticket": {"title": "VPN 频繁掉线，办公室多人受影响",
                   "description": "从早上开始，多个部门的同事 VPN 断断续续，"
                                  "重连几次才能上。怀疑专线或对端机房有问题。",
                   "category": "incident", "priority": "P2"},
        "expected": {"serviceTop1": 2005, "weakness": "none", "outcome": "matched"},
        "notes": "'VPN'/'网络'/'机房'/'链路' 多个关键词，owner 103 mid。",
    },
    {
        "source": {"kind": "console-ai", "originId": "kz5mjjpox", "originCategory": "Network",
                   "originPriority": "High",
                   "originSubject": "Access Issue with Shared Network Drive"},
        "ticket": {"title": "内网共享盘打不开",
                   "description": "以前能访问的文件服务器，今天开始一直 '找不到网络路径'。"
                                  "同事里做设计的和做合同的都受影响。",
                   "category": "incident", "priority": "P2"},
        "expected": {"serviceTop1": 2005, "weakness": "none", "outcome": "matched"},
        "notes": "'网络路径' + '内网' → 2005。测 BM25 是否会误命中到 2007 Web 控制台（'打不开'）。",
    },
    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "跨机房调用大面积超时",
                   "description": "A 机房访问 B 机房的接口，昨晚 22 点后延迟从 30ms 涨到 3000ms，"
                                  "已经影响业务方。",
                   "category": "incident", "priority": "P1"},
        "expected": {"serviceTop1": 2005, "weakness": "none", "outcome": "matched"},
        "notes": "'机房' + '链路' + '超时' 都命中；同时含 '接口' 是 2001 关键词，压歧义。"
                 "期望 top-1 是 2005（'机房' 出现两次，权重高）。",
    },

    # ============ 2006 私有化部署环境 ============
    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "私有化部署需要的最低硬件配置",
                   "description": "客户想了解一下私有化部署服务器配置和端口出网要求。",
                   "category": "consultation", "priority": "P3"},
        "expected": {"serviceTop1": 2006, "weakness": "none", "outcome": "matched"},
        "notes": "咨询类，命中 '私有化' + '部署' + '硬件要求'；owner 103 mid。",
    },
    {
        "source": {"kind": "console-ai", "originId": "esr24h5h2", "originCategory": "Infrastructure",
                   "originPriority": "High",
                   "originSubject": "VM Snapshots Intermittently Failing"},
        "ticket": {"title": "现场部署包升级到 v3.2 后启动失败",
                   "description": "客户 A 现场部署，升级包跑起来卡在初始化。"
                                  "日志显示是私有化环境配置项不兼容。",
                   "category": "incident", "priority": "P2"},
        "expected": {"serviceTop1": 2006, "weakness": "none", "outcome": "matched"},
        "notes": "'现场部署' + '私有化' + '部署' 都命中。",
    },

    # ============ 2007 Web 控制台 ============
    {
        "source": {"kind": "console-ai", "originId": "2apbmy56h", "originCategory": "Security",
                   "originPriority": "High",
                   "originSubject": "Workstation Security Configuration: Inconsistent Group Policies"},
        "ticket": {"title": "管理控制台某些菜单点了没反应",
                   "description": "客户反馈：控制台首页能进，但订单管理、退款管理这两个菜单点了不响应。"
                                  "浏览器 F12 看到前端报错，跟版本发布有关系。",
                   "category": "incident", "priority": "P2"},
        "expected": {"serviceTop1": 2007, "weakness": "none", "outcome": "matched"},
        "notes": "'控制台' + '菜单' + '页面' + '前端' 命中。owner 105 mid。",
    },
    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "管理后台白屏",
                   "description": "客户登录后台以后一片白，什么菜单都出不来。",
                   "category": "incident", "priority": "P1"},
        "expected": {"serviceTop1": 2007, "weakness": "none", "outcome": "matched"},
        "notes": "'后台' 是 2007 别名 + '白屏' 是关键词。",
    },
    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "P0：控制台大面积打不开",
                   "description": "多家客户反馈控制台完全打不开，前端报错。",
                   "category": "incident", "priority": "P0"},
        "expected": {"serviceTop1": 2007, "weakness": "empty_candidates", "outcome": "no_candidate"},
        "notes": "P0 + owner 105 mid + backup 108 junior → P0 gate 把两人都过滤；"
                 "其他 senior 员工与 2007 无 ownership 关联但仍在候选池里（101/102/104/107）。"
                 "验证 P0 gate 是否会把 ownership 命中但级别不够的人过滤掉，"
                 "同时看非 ownership senior 会不会被 top-1（应该走 Stage 1 generic 或 Stage 3）。"
                 "实际期望：weakness 判定要看 top1/top2 margin —— "
                 "如果 4 位 senior 无 ownership 打平，就是 low_margin；如果全部被过滤 empty_candidates。",
    },

    # ============ 2008 桌面客户端 ============
    {
        "source": {"kind": "console-ai", "originId": "w163exwly", "originCategory": "RemoteWork",
                   "originPriority": "Medium",
                   "originSubject": "Remote Desktop Access Instructions"},
        "ticket": {"title": "Mac 端客户端启动后闪退",
                   "description": "从昨晚更新以后，Mac 桌面客户端一打开就闪退。"
                                  "同事里两台电脑都一样。",
                   "category": "incident", "priority": "P2"},
        "expected": {"serviceTop1": 2008, "weakness": "none", "outcome": "matched"},
        "notes": "'Mac' + '客户端' + '闪退' 命中。owner 105 mid。",
    },
    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "Windows 端不能自动升级到新版本",
                   "description": "桌面端升级一直失败，卡在小版本更新。",
                   "category": "incident", "priority": "P3"},
        "expected": {"serviceTop1": 2008, "weakness": "none", "outcome": "matched"},
        "notes": "'Windows' + '桌面' + '升级' 命中。",
    },

    # ============ 2009 数据库集群 ============
    {
        "source": {"kind": "console-ai", "originId": "cythbyl5p", "originCategory": "Account",
                   "originPriority": "Medium",
                   "originSubject": "Access Request: DB Storage Folder"},
        "ticket": {"title": "生产 MySQL 连接池打满",
                   "description": "线上数据库连接数持续飙高，业务方反馈 '获取连接超时'。"
                                  "主从切换演练之后开始的。",
                   "category": "incident", "priority": "P1"},
        "expected": {"serviceTop1": 2009, "weakness": "none", "outcome": "matched"},
        "notes": "'数据库' + 'MySQL' + '连接池' + '主从' 密集命中。owner 102 senior。",
    },
    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "分库分表路由不对，部分订单落错库",
                   "description": "业务反馈有些订单查询走不到对应分片，怀疑分库分表中间件。",
                   "category": "incident", "priority": "P2"},
        "expected": {"serviceTop1": 2009, "weakness": "none", "outcome": "matched"},
        "notes": "'分库分表' 是 2009 别名；'订单' 又落在 2001 关键词里，压歧义。"
                 "期望 2009 top-1（'分库分表' 只在 2009 出现）。",
    },

    # ============ 2010 安全加固与合规 ============
    {
        "source": {"kind": "console-ai", "originId": "ejw6honqh", "originCategory": "Security",
                   "originPriority": "Medium",
                   "originSubject": "Request for Instructions on Reporting a Suspected Phishing Email"},
        "ticket": {"title": "疑似钓鱼邮件在公司邮箱流传",
                   "description": "有员工反馈收到一封伪装成 OA 系统的邮件，"
                                  "已经有人点了。请安全组协助处理。",
                   "category": "incident", "priority": "P1"},
        "expected": {"serviceTop1": 2010, "weakness": "none", "outcome": "matched"},
        "notes": "'安全' + '合规' 相关；owner 104 senior。文本没出现 '安全' 关键词，"
                 "但'钓鱼邮件' 是 2010 常见场景。压 BM25 语义泛化边界——可能落 low_margin。",
    },
    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "P0 漏洞响应：外部报了一个 RCE",
                   "description": "外部安全团队上报，管理台某接口存在远程代码执行漏洞，需要立即加固。",
                   "category": "incident", "priority": "P0"},
        "expected": {"serviceTop1": 2010, "weakness": "none", "outcome": "matched"},
        "notes": "P0 + owner 104 senior。'漏洞' + '加固' + '安全' 密集命中。",
    },

    # ============ 2011 数据同步管道 ============
    {
        "source": {"kind": "console-ai", "originId": "ujwveoos9", "originCategory": "Infrastructure",
                   "originPriority": "High",
                   "originSubject": "Enterprise Email Archiving Retrieval Issues"},
        "ticket": {"title": "上下游数据同步延迟两小时",
                   "description": "客户订单同步到数据仓库，昨天开始延迟两小时；"
                                  "定时任务积压，对账对不上。",
                   "category": "incident", "priority": "P2"},
        "expected": {"serviceTop1": 2011, "weakness": "none", "outcome": "matched"},
        "notes": "'同步' + '延迟' + '对账' + '任务' 命中 2011。owner 101 senior。"
                 "'订单' 又是 2001 关键词，压歧义。",
    },
    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "数据不同步，报表看不到昨天的数",
                   "description": "数据同步任务今天没跑，看板数据还是前天的。",
                   "category": "incident", "priority": "P3"},
        "expected": {"serviceTop1": 2011, "weakness": "none", "outcome": "matched"},
        "notes": "'数据不同步' 是 2011 别名；'报表' + '看板' 是 2002 关键词。"
                 "top-1 应该是 2011（'同步' 只出现在 2011）。这条压 BM25 是否会误命中到 2002。",
    },

    # ============ 2012 硬件与设备 (owner 103 mid，无 backup) ============
    {
        "source": {"kind": "console-ai", "originId": "1aiu3lrqi", "originCategory": "Network",
                   "originPriority": "Medium",
                   "originSubject": "Our network printer keeps disconnecting"},
        "ticket": {"title": "办公室打印机老断连",
                   "description": "打印机隔一会儿就掉线，需要重新配 IP 才能用。同事好几位都反应同样问题。",
                   "category": "incident", "priority": "P3"},
        "expected": {"serviceTop1": 2012, "weakness": "none", "outcome": "matched"},
        "notes": "'打印机' 只出现在 2012 关键词；owner 103 mid。"
                 "'网络'/'IP'/'断' 又落在 2005，压 BM25 优先级。",
    },
    {
        "source": {"kind": "console-ai", "originId": "dn74utk13", "originCategory": "Hardware",
                   "originPriority": "High",
                   "originSubject": "Persistent Issues with Biometric Authentication System"},
        "ticket": {"title": "POS 机扫脸设备失灵",
                   "description": "门店 POS 终端的指纹/面部识别一直识别不到，需要现场检修。",
                   "category": "incident", "priority": "P2"},
        "expected": {"serviceTop1": 2012, "weakness": "none", "outcome": "matched"},
        "notes": "'POS' + '终端' 命中 2012 别名；'识别' 可能牵到 2013 令牌。",
    },
    {
        "source": {"kind": "console-ai", "originId": "rhxsdvazi", "originCategory": "Hardware",
                   "originPriority": "Medium",
                   "originSubject": "Dual Monitor Display Issue"},
        "ticket": {"title": "工位服务器硬盘灯亮红灯",
                   "description": "机房一台老服务器的硬盘故障灯亮了，系统还能跑，先做个排查。",
                   "category": "incident", "priority": "P2"},
        "expected": {"serviceTop1": 2012, "weakness": "none", "outcome": "matched"},
        "notes": "'服务器' + '硬盘' 都是 2012 关键词。'机房' 在 2005 出现，压歧义。",
    },

    # ============ 2013 接口鉴权与令牌 ============
    {
        "source": {"kind": "console-ai", "originId": "1sj8czs0k", "originCategory": "Security",
                   "originPriority": "High",
                   "originSubject": "Persistent Authentication Failures with MFA"},
        "ticket": {"title": "接口大量返回 401，token 刷新失败",
                   "description": "外部合作方调用我们接口，反馈 401 一直拿不到有效 token。"
                                  "我们内部鉴权服务日志里看到大量 '签名校验不通过'。",
                   "category": "incident", "priority": "P1"},
        "expected": {"serviceTop1": 2013, "weakness": "none", "outcome": "matched"},
        "notes": "'401' + 'token' + '鉴权' + '签名' 密集命中。owner 101 senior。",
    },
    {
        "source": {"kind": "console-ai", "originId": "7rh5ne450", "originCategory": "Hardware",
                   "originPriority": "Medium",
                   "originSubject": "Instructions for Configuring Multiple Monitors"},
        "ticket": {"title": "令牌过期后没有触发刷新",
                   "description": "客户端反馈访问某些接口一直 401，重启才能好一会儿。",
                   "category": "incident", "priority": "P2"},
        "expected": {"serviceTop1": 2013, "weakness": "none", "outcome": "matched"},
        "notes": "'令牌' 是 2013 别名；'接口' 又出现在 2001。压 BM25。",
    },

    # ============ 2014 员工工具台 (owner 107 senior) ============
    {
        "source": {"kind": "console-ai", "originId": "l0nhq0pys", "originCategory": "Account",
                   "originPriority": "Medium",
                   "originSubject": "JIRA Board Access Request"},
        "ticket": {"title": "内部工具台权限申请：客服需要看工单后台",
                   "description": "新入职客服需要访问工单后台与内部知识库，走审批流程申请权限。",
                   "category": "request", "priority": "P3"},
        "expected": {"serviceTop1": 2014, "weakness": "none", "outcome": "matched"},
        "notes": "'内部工具' + '工单' + '客服' 都命中 2014。owner 107 senior。",
    },
    {
        "source": {"kind": "console-ai", "originId": "tso616mbn", "originCategory": "Software",
                   "originPriority": "Medium",
                   "originSubject": "Software Access: Asana Project for Jordan Smith"},
        "ticket": {"title": "工具台某个跨系统入口报 500",
                   "description": "点开工单后台的 '关联客户' 按钮返回 500，其他同事也复现不了。",
                   "category": "incident", "priority": "P2"},
        "expected": {"serviceTop1": 2014, "weakness": "none", "outcome": "matched"},
        "notes": "'工具台' + '工单' 命中。'500' 也在 2001 关键词里，压歧义。",
    },

    # ============ 边界样本：判弱段专门覆盖 ============

    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "预约会议室",
                   "description": "行政帮忙订一个明天下午 3 点的会议室。",
                   "category": "request", "priority": "P3"},
        "expected": {"serviceTop1": 0, "weakness": "no_service_match", "outcome": "fallback_pool"},
        "notes": "无关请求，14 服务字典里没这条。BM25 top-1 应该 < 0.25 → 判 no_service_match。"
                 "压 ServiceMatchFloor 的下界。",
    },
    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "报修下单接口 500，另外 VPN 也断了",
                   "description": "客户下单一直失败；顺便我们这边 VPN 也不稳定，两件事一起看下。",
                   "category": "incident", "priority": "P2"},
        "expected": {"serviceTop1": 2001, "weakness": "none", "outcome": "matched"},
        "notes": "混合意图：2001 + 2005 都在文本里。top-1 应该 2001（'下单' 只出现在 2001 别名），"
                 "派给 owner 101。测 BM25 在多命中下的 top-1 稳定性。",
    },
    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "老板说系统崩了",
                   "description": "老板反馈系统坏了，具体不清楚。",
                   "category": "incident", "priority": "P2"},
        "expected": {"serviceTop1": 0, "weakness": "no_service_match", "outcome": "fallback_pool"},
        "notes": "极低信息量。仿 tasksource 真工单里的 'impressora com erro'（打印机报错，一词短文本）。"
                 "字典里没有 '系统' 别名，BM25 应该给不出可信命中。",
    },
    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "客户投诉：账单金额不对",
                   "description": "客户要求核对上月账单差异，怀疑是计费系统的账单生成有 bug。",
                   "category": "incident", "priority": "P2"},
        "expected": {"serviceTop1": 2004, "weakness": "none", "outcome": "matched"},
        "notes": "'账单' 密集出现在 2004；'计费' 也是。这条测正例 margin 稳定。",
    },
    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "登录后台看不到菜单",
                   "description": "用户中心登进来后管理台一片空白。",
                   "category": "incident", "priority": "P2"},
        "expected": {"serviceTop1": 2007, "weakness": "none", "outcome": "matched"},
        "notes": "两个服务都可能出现：2003 (登录/用户中心) 与 2007 (后台/控制台/白屏)。"
                 "期望 top-1 是 2007（'后台' 与 '白屏/一片空白' 语义更近）。压 BM25 谁赢。",
    },
    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "同步任务和接口调用一起挂了",
                   "description": "数据同步任务大面积失败，同时下游调用返回超时。",
                   "category": "incident", "priority": "P1"},
        "expected": {"serviceTop1": 2011, "weakness": "low_margin", "outcome": "fallback_pool"},
        "notes": "两个服务都可能：2011 (同步/任务) 与 2001 (接口)。owner 都是 101。"
                 "top1/top2 分差可能 < 0.15 → low_margin → 走 Stage 2；本 runner 里 chooser=nil → 直落 Stage 3。"
                 "压 MarginThreshold 上界。",
    },
    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "P0：数据库集群挂了没人能连",
                   "description": "订单和报表都受影响，客户全部投诉。",
                   "category": "incident", "priority": "P0"},
        "expected": {"serviceTop1": 2009, "weakness": "none", "outcome": "matched"},
        "notes": "描述里没有 '数据库' 关键词，但标题里有。owner 102 senior。"
                 "P0 + '订单' + '报表' 会牵出 2001/2002 干扰；压 BM25 短描述 + 强标题场景。",
    },
    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "员工咨询：VPN 密码是什么规则",
                   "description": "新同事问 VPN 密码的构成规则是什么。",
                   "category": "consultation", "priority": "P3"},
        "expected": {"serviceTop1": 2005, "weakness": "none", "outcome": "matched"},
        "notes": "咨询类 + 'VPN' 命中 2005。测非 incident 类目的分类不影响 BM25 打分。",
    },
    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "订单接口和令牌接口都在报错",
                   "description": "客户端调用下单接口返回 401，同时鉴权服务日志刷 '签名过期'。",
                   "category": "incident", "priority": "P1"},
        "expected": {"serviceTop1": 2013, "weakness": "none", "outcome": "matched"},
        "notes": "两个 owner 都是 101（2001 owner=101，2013 owner=101），所以派谁赢都是 101；"
                 "但 top-1 服务判定要看 BM25 —— '401' 只在 2013 出现，'签名' 也是，应该 top-1=2013。"
                 "测 owner 相同但 top-1 服务正确的场景，抽取轴独立于排序轴。",
    },
    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "系统整体变慢了",
                   "description": "感觉整个系统访问都很慢，说不上来哪个模块。",
                   "category": "incident", "priority": "P3"},
        "expected": {"serviceTop1": 0, "weakness": "no_service_match", "outcome": "fallback_pool"},
        "notes": "'慢' 是 2002/2009 关键词但没其他锚点；文本没有指名任何具体服务。"
                 "压 BM25 在 '什么都像但又都不像' 场景下是否会给出虚高 top-1。",
    },
    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "硬件到货，请运维来搬",
                   "description": "一批服务器上架到货，需要运维协助搬到机房。",
                   "category": "request", "priority": "P3"},
        "expected": {"serviceTop1": 2012, "weakness": "none", "outcome": "matched"},
        "notes": "'硬件' + '服务器' 命中 2012；'机房' 又落在 2005，压歧义。owner 103 相同（都是运维），所以派谁赢一样；"
                 "抽取轴能区分才有意义。",
    },
    {
        "source": {"kind": "handcrafted"},
        "ticket": {"title": "订单查询走了老库",
                   "description": "订单查询接口好像走的不是新数据库，帮忙看看是不是路由不对。",
                   "category": "incident", "priority": "P2"},
        "expected": {"serviceTop1": 2009, "weakness": "none", "outcome": "matched"},
        "notes": "跨三个服务的措辞：订单(2001)/查询(2002)/数据库(2009)。"
                 "'路由不对' 语义上更像 2009 分库分表；测 BM25 在 3 关键词密集场景的 top-1 稳定性。"
                 "如果最终判为 low_margin 也是可接受结果 —— 只要与 expectedWeakness 一致。",
    },
]


def build():
    """派生 expected.{weakness,outcome,assigneeId} —— 全部由 stage1() 按公式生成。

    serviceTop1 是唯一手工输入（抽取段金标，改写时锁定）；
    判弱/排序在此前提下由 Stage 1 规则派生，与 internal/assign 同源。
    """
    dist_outcome = {}
    dist_weak = {}
    for c in CASES:
        exp = c["expected"]
        top1 = exp.get("serviceTop1", 0)
        priority = c["ticket"]["priority"]
        weakness, outcome, assignee = stage1(top1, priority)

        authored = exp.get("weakness")
        if authored and authored != weakness:
            print(f"  [派生覆盖手工] {c.get('id','?')} {priority} top1={top1}: "
                  f"weakness {authored} → {weakness}")
        exp["weakness"] = weakness
        exp["outcome"] = outcome
        exp["assigneeId"] = assignee
        exp.pop("assigneeOverride", None)
        dist_outcome[outcome] = dist_outcome.get(outcome, 0) + 1
        dist_weak[weakness] = dist_weak.get(weakness, 0) + 1

    # 重新排布 id
    out = {
        "version": 2,
        "pipelineAxes": True,
        "provenance": {
            "primary": {
                "name": "Console-AI/IT-helpdesk-synthetic-tickets",
                "license": "MIT",
                "url": "https://huggingface.co/datasets/Console-AI/IT-helpdesk-synthetic-tickets",
                "use": "工单结构与口语感蓝本；本数据集不复制英文原文，只做中文改写与场景重映射",
            },
            "sanity_check": {
                "name": "tasksource/it-support-tickets",
                "license": "CC-BY-4.0",
                "url": "https://huggingface.co/datasets/tasksource/it-support-tickets",
                "use": "真实工单分布参考（短文本、含掩码实体、多语种）；边界样本按这些模式仿写",
            },
        },
        "goldSource": "formula-derived",
        "goldBoundary": "抽取段金标(serviceTop1)为手工锁定；判弱/排序段由 gen_v2.py 的 "
                        "stage1() 复现 internal/assign 的 Stage 1 加权打分 + P0 过滤 + margin 判弱"
                        "派生，前提是'抽取正确'。因此本数据集证明的是"
                        " 'pipeline 在给定抽取下的实现 == 我们宣称的规则'，不是外部准确率的人类判断；"
                        "真实 BM25 多召回第二条过阈值服务会与实际产生偏差（ownership-leak 边界）。"
                        "对外准确率声明必须挂人工标注卡 —— 见 README。",
        "description": "三段流水线（Stage 1 规则+检索 / Stage 2 LLM / Stage 3 人工）"
                       "的评测集。每条 = 一份中文 IT 服务台工单 + 三段金标。",
        "cases": [],
    }
    for i, c in enumerate(CASES, start=1):
        c2 = dict(c)
        c2["id"] = f"dp-v2-{i:03d}"
        out["cases"].append(c2)
    print(f"派生结果分布：outcome={dist_outcome}  weakness={dist_weak}")
    return out


if __name__ == "__main__":
    data = build()
    Path("assignment_v2.json").write_text(
        json.dumps(data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    print(f"生成 {len(data['cases'])} 条 → assignment_v2.json")
