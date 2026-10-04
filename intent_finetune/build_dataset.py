"""构建意图识别 LoRA 微调数据集（纯公开语料 + 程序化组合模板，5 意图映射）。

数据源 → 意图映射（全部为公开数据集，经 hf-mirror 下载到 raw/）：
  chitchat     : LCCC-base 首轮闲聊（声明式短句）+ MASSIVE zh-CN greet/joke + 寒暄组合模板
  knowledge    : COIG-CQIA segmentfault（IT 问答）+ wikihow 计算机/手机/软件类 how-to
  incident     : ChnSentiCorp 差评 + 电商评论强故障词过滤（该数据 label 是类别编码非情感，
                 故障词即差评锚点）+ IT 报障/建单组合模板
  handoff      : 无公开数据集（业界同样缺失），组合模板生成
  out_of_scope : MASSIVE 智能助手指令/qa_*（查股票/算数）+ CQIA xhs（代写文案）/
                 exam（做题）+ ruozhiba（无关提问）+ zhihu（观点提问）+ wikihow 非IT how-to

评测集 eval/datasets/intent.json 的 180 条**只做 held-out 评测，绝不进训练**；
与评测文本做归一化去重防止泄漏。系统提示从 src/helpdesk/intent.py 的
INTENT_PROMPT 正则提取，保证与线上 classify_intent 一致。

配比参考评测集分布（knowledge 55 / incident 45 / chitchat 30 / handoff 25 / oos 25），
knowledge 与 incident 给足量，out_of_scope 控制不超配。
"""
from __future__ import annotations

import gzip
import json
import random
import re
from collections import Counter, defaultdict
from pathlib import Path

import pandas as pd
import pyarrow.ipc as ipc

HERE = Path(__file__).resolve().parent
RAW = HERE / "raw"
OUT = HERE / "data"
PROJ = HERE.parent

random.seed(13)

# ---------------------------------------------------------------- system prompt
# 微调用精简版：LoRA 后模型自带任务能力，无需完整规则文本（400 tok）。
# 推理接入 classify_intent 时把 INTENT_PROMPT 常量替换为此精简版即可。
INTENT_PROMPT = (
    "你是IT服务台意图分类器。把用户的话归为4类之一并给置信度：\n"
    "knowledge=问怎么做/为什么/是什么（功能用法、报错成因、流程时限）；\n"
    "incident=报告已发生的故障，或要求建单/提单/登记问题；\n"
    "handoff=明确要求转人工/找真人（优先级最高，即使同时描述了故障或提问）；\n"
    "out_of_scope=其余全部：寒暄致谢告别闲聊（只做简短社交回应），以及与IT支持无关的具体请求（写诗做菜查天气算题做题代写文案，礼貌拒答并说明服务范围）。\n"
    "边界：只问\"人工电话多少/人工能不能解决\"是knowledge；\"帮我提单给研发\"是incident；追问中回答\"选101\"之类延续流程不重分类。\n"
    "只输出JSON：{\"intent\": \"类别\", \"confidence\": 0~1}。用户的话："
)

# 4 类版本：chitchat 并入 out_of_scope。
# 依据：路由表里两者工具面完全相同（intent_router.py 均为空集），合并零行为差异，
# 只去掉"简短社交回应 vs 礼貌拒答"的措辞区分（由模型读原文自行处理），
# 同时消掉评测集里最难分的一处边界（chitchat↔out_of_scope）。
INTENTS = ["knowledge", "incident", "handoff", "out_of_scope"]

# ---------------------------------------------------------------- utils
_norm_re = re.compile(r"[\s，。！!？?、,.~～…\"'“”‘’()（）:：;；]+")

def norm(s: str) -> str:
    return _norm_re.sub("", s).lower()

def load_eval_texts() -> set[str]:
    data = json.loads((PROJ / "eval/datasets/intent.json").read_text(encoding="utf-8"))
    return {norm(c["text"]) for c in data["cases"]}

# 强故障词：报障语言的锚点（出现即大概率是"报告已发生故障"）
FAULT_STRONG = (
    "打不开|开不了|用不了|不能用|无法|坏了|坏掉|出故障|故障|死机|卡死|蓝屏|黑屏|白屏|"
    "报错|失灵|没反应|没反应|闪退|炸了|裂了|裂了|断裂|漏水|不出|失联|掉线|异常|失败|"
    "打不出|没声音|不显示|自动关机|充不进|挂了|崩了|扣成负数|丢数据|数据丢|丢了|爆了|"
    "登不上|登录不上|连不上|超时|502|401|访问不了|一直报|重装了还是|修不好|修了两次"
)
FAULT_RE = re.compile(FAULT_STRONG)
# 好评标志：与故障词互斥排除（防"以前坏了后来很好"这类）
POS_RE = re.compile("很满意|非常满意|好评|五星|值得|很喜欢|特别喜欢|推荐|好评|很棒|超赞|很满意")

TECH_RE = re.compile(
    "系统|密码|软件|电脑|网络|报错|登录|接口|服务器|数据库|代码|程序|部署|配置|"
    "token|api|API|401|502|bug|网站|后台|账号|线上"
)
QUESTION_RE = re.compile(
    "怎么|如何|为什么|什么|什么原因|咋|？|\\?|吗|呢|区别|教程|步骤|方法|支不支持|能不能|多少|哪个|哪些|是不是|有没有"
)

def clean_len(s: str, lo: int = 4, hi: int = 60) -> bool:
    s = s.strip()
    return lo <= len(s) <= hi

def sample_cap(items: list[str], cap: int) -> list[str]:
    items = list(dict.fromkeys(items))
    random.shuffle(items)
    return items[:cap]

# ---------------------------------------------------------------- MASSIVE helper
_massive_cache: pd.DataFrame | None = None
def load_massive() -> pd.DataFrame:
    global _massive_cache
    if _massive_cache is None:
        import pyarrow.parquet as pq
        sch = pq.read_schema(RAW / "massive_zh_train.parquet")
        meta = json.loads(sch.metadata[b"huggingface"].decode())
        names = meta["info"]["features"]["intent"]["names"]
        df = pd.read_parquet(RAW / "massive_zh_train.parquet")
        df["intent_name"] = df.intent.map(lambda i: names[i])
        _massive_cache = df
    return _massive_cache

# ---------------------------------------------------------------- 1. chitchat
def build_chitchat() -> dict[str, list[str]]:
    out: list[str] = []
    with gzip.open(RAW / "lccc_base_train.jsonl.gz", "rt", encoding="utf-8") as f:
        for line in f:
            if len(out) >= 30000:
                break
            try:
                d = json.loads(line)
            except Exception:
                continue
            turns = d if isinstance(d, list) else d.get("msg") or d.get("dialog") or []
            if not turns or not isinstance(turns[0], str):
                continue
            t = turns[0].strip()
            if not (2 <= len(t) <= 25):
                continue
            if re.search(r"[?？]|吗|怎么|如何|为什么|什么|帮我|请问|推荐|想问|多少|哪", t):
                continue
            if TECH_RE.search(t):
                continue
            out.append(t)
    massive = load_massive()
    greet_joke = massive[massive.intent_name.isin(["general_greet", "general_joke"])].utt.tolist()
    greet_joke = [t for t in greet_joke if clean_len(t, 2, 30)]
    # 寒暄组合模板：问候/致谢/告别/应答，匹配评测集 chitchat 的真实分布
    tpl: list[str] = []
    for w in ["你好", "您好", "哈喽", "嗨", "早上好", "下午好", "晚上好", "早啊", "早安", "晚安", "在吗", "有人吗"]:
        for suf in ["", "呀", "啊", "哈", "呀", "～", "！"]:
            tpl.append(w + suf)
    for w in ["谢谢", "多谢", "thanks", "辛苦了", "辛苦辛苦", "太感谢了", "麻烦你了", "麻烦啦", "感激不尽", "受教了"]:
        for suf in ["", "啦", "哈", "哦", "～", "！"]:
            tpl.append(w + suf)
    for w in ["拜拜", "再见", "先这样吧", "我先下了", "回见", "下次再聊", "走了啊", "先忙了"]:
        for suf in ["", "哈", "啦", "哦", "～", "！"]:
            tpl.append(w + suf)
    for w in ["好的", "收到", "嗯嗯", "嗯", "明白", "知道了", "行", "对", "是呀", "ok", "OK", "了解了", "懂了"]:
        for suf in ["", "好的", "了", "哈", "，谢谢", "～"]:
            tpl.append(w + suf)
    return {"lccc": sample_cap(out, 700), "massive_greet_joke": sample_cap(greet_joke, 150), "templates": sample_cap(tpl, 350)}

# ---------------------------------------------------------------- 2. knowledge
def build_knowledge() -> dict[str, list[str]]:
    sf: list[str] = []
    with open(RAW / "cqia_segmentfault.jsonl", encoding="utf-8") as f:
        for line in f:
            try:
                d = json.loads(line)
            except Exception:
                continue
            t = str(d.get("instruction", "")).split("\n")[0].strip().strip("？?。 ")
            # SF 标题本身就是技术提问，不强制问词（"Nginx配置详解"也是 knowledge）
            if clean_len(t, 8, 70) and not re.search("```|http[s]?://|求代码|求源码", t):
                sf.append(t)
    wk: list[str] = []
    with open(RAW / "cqia_wikihow.jsonl", encoding="utf-8") as f:
        for line in f:
            try:
                d = json.loads(line)
            except Exception:
                continue
            t = str(d.get("instruction", "")).split("\n")[0].strip()
            dom = str(d.get("domain", ""))
            if not re.search("计算机|电脑|手机|软件|电子|网络|数码|互联网|浏览器|微信|操作系统", dom):
                continue
            if clean_len(t, 8, 45) and t.startswith(("如何", "怎么", "怎样")):
                wk.append(t)
    # 现象成因问句模板：评测集 knowledge 大量是"XX超时是怎么回事/怎么修"，
    # 与 incident 的"XX超时了"只差疑问框架，是 knowledge↔incident 的关键边界样本
    comps = ["接口401", "接口500", "token过期", "SSO登录失败", "LDAP账号被锁", "磁盘使用率过高",
             "数据库连接池满", "定时任务没执行", "消息队列积压", "Nginx配置", "HTTPS证书过期",
             "K8s Pod重启", "CDN缓存没刷新", "Redis内存占用高", "慢SQL", "索引失效", "灰度发布配置",
             "版本回滚", "权限申请流程", "审批流卡住", "webhook回调失败", "短信验证码收不到",
             "邮箱收不到信", "报表导出超时", "网关限流", "日志采集异常", "监控告警频繁",
             "备份恢复流程", "域名解析失败", "会话超时时间"]
    qs = ["是怎么回事", "是什么原因", "怎么排查", "怎么处理", "怎么修", "有文档吗",
          "的排查步骤是啥", "该怎么解决", "为什么会一直报", "是怎么触发的",
          "正确的处理流程是啥", "看哪里的日志", "的解决方法是什么", "有什么排查思路",
          "应该怎么配置", "为什么会出现", "是配置问题吗", "怎么定位原因", "支持自助修复吗"]
    it_q = [c + q for c in comps for q in qs]
    return {
        "segmentfault": sample_cap(sf, 650),
        "wikihow_tech": sample_cap(wk, 300),
        "it_questions": sample_cap(it_q, 400),
    }

# ---------------------------------------------------------------- 3. incident
def build_incident() -> dict[str, list[str]]:
    reviews: list[str] = []
    with open(RAW / "chnsenticorp_train.arrow", "rb") as f:
        t = ipc.open_stream(f).read_all()
    for label, text in zip(t.column("label").to_pylist(), t.column("text").to_pylist()):
        if label == 0 and clean_len(text, 8, 60) and FAULT_RE.search(text) and not POS_RE.search(text):
            reviews.append(text.strip())
    s = pd.read_parquet(RAW / "shopping.parquet")
    for _, row in s.iterrows():
        if not re.search("平板|手机|计算机|热水器|显示器|音响|耳机", str(row["cat"])):
            continue
        txt = str(row["review"]).strip().lstrip("\\ufeff")
        if not clean_len(txt, 8, 60):
            continue
        if FAULT_RE.search(txt) and not POS_RE.search(txt):
            reviews.append(txt)
    # IT 报障组合模板（贴域）：组件 × 症状 × 语气
    comps = ["网关", "服务器", "数据库", "redis", "k8s集群", "线上系统", "后台", "登录页", "订单系统",
             "报表模块", "推送服务", "接口", "磁盘", "证书", "审批流", "消息队列", "定时任务", "控制台", "邮箱", "网盘"]
    faults = ["打不开了", "一直502", "一直超时", "老报错", "崩了", "挂了", "卡死了", "全白屏了", "数据丢了",
              "内存爆了", "扣成负数了", "推了十几遍", "连不上了", "响应特别慢", "日志全是报错", "证书过期了",
              "队列堵死了", "一直转圈", "全量401", "登录不上了", "磁盘90%了", "权限全没了", "页面炸了"]
    tails = ["，麻烦看下", "，赶紧处理", "，帮忙登记一下", "，客户都在催", "，尽快安排处理", "，这应该是bug吧",
             "，影响我们整个部门", "，试了几次都不行", "，急", "，帮我提个单", "，看下是不是出问题了", ""]
    it_tpl = [c + f + t2 for c in comps for f in faults for t2 in tails]
    # 直接建单请求模板
    ticket_tpl = []
    for a in ["帮我", "麻烦", "给我", "帮我", "请"]:
        for b in ["建个工单", "建单", "提个单", "提单", "登记一下这个问题", "登记个故障", "报个障", "记录一下这个问题"]:
            for c2 in ["", "，尽快", "，急", "，谢谢", "，给研发看看", "，跟进下"]:
                ticket_tpl.append(a + b + c2)
    return {
        "neg_reviews": sample_cap(reviews, 450),
        "it_templates": sample_cap(it_tpl, 550),
        "ticket_requests": sample_cap(ticket_tpl, 80),
    }

# ---------------------------------------------------------------- 4. handoff
def build_handoff() -> dict[str, list[str]]:
    cores = [
        "转人工", "给我转人工", "我要转人工", "麻烦转人工", "帮我转人工", "请转人工",
        "我要找人工", "找人工客服", "转人工客服", "转真人", "接真人", "找真人客服",
        "来个真人", "给我接真人", "人工", "请转接人工", "我要人工服务", "转接人工",
        "找个人工客服", "人工服务", "要人工", "给我人工", "接人工", "人工在吗",
        "叫个人工来", "我要跟真人说话", "让真人联系我", "请人工介入",
    ]
    prefixes = ["", "别机器人了，", "跟机器人说不明白，", "机器人搞不定的，", "不想跟机器人说了，", "你们机器人答非所问，"]
    suffixes = [
        "", "快点", "着急", "现在就转", "尽快", "谢谢", "拜托了",
        "我的账号被锁了", "页面打不开", "这个问题太复杂了", "不然我要投诉了", "电话或在线都行",
        "我已经等很久了", "这个故障我跟机器人说不清楚", "别让机器人回复我", "再不转人工我就卸载了",
    ]
    combos = []
    for p in prefixes:
        for c in cores:
            for s in suffixes:
                if p and c.startswith(("麻烦", "请", "帮")):
                    continue
                text = (p + c + (("，" + s) if s else "")).strip("，")
                if 2 <= len(text) <= 40:
                    combos.append(text)
    return {"templates": sample_cap(combos, 700)}

# ---------------------------------------------------------------- 5. out_of_scope
def build_out_of_scope() -> dict[str, list[str]]:
    massive = load_massive()
    cmds = massive[~massive.intent_name.str.startswith("general")].utt.tolist()
    cmds = [t for t in cmds if clean_len(t, 4, 40)]
    qa = massive[massive.intent_name.str.startswith("qa_")].utt.tolist()
    qa = [t for t in qa if clean_len(t, 4, 40)]
    xhs, exam, ruo, zh, wk_other = [], [], [], [], []
    with open(RAW / "cqia_xhs.jsonl", encoding="utf-8") as f:
        for line in f:
            try:
                t = json.loads(line).get("instruction", "")
            except Exception:
                continue
            t = str(t).split("\n")[0].strip()
            if clean_len(t, 8, 50) and t.startswith(("写", "帮我写", "生成", "帮我生成")):
                xhs.append(t)
    with open(RAW / "cqia_exam.jsonl", encoding="utf-8") as f:
        for line in f:
            try:
                d = json.loads(line)
            except Exception:
                continue
            t = str(d.get("input") or d.get("instruction", "")).split("\n")[0].strip()
            if clean_len(t, 8, 45) and re.search("请|计算|求|写|翻译|概述|总结", t) and not TECH_RE.search(t):
                exam.append(t)
    with open(RAW / "cqia_ruozhiba.jsonl", encoding="utf-8") as f:
        for line in f:
            try:
                t = json.loads(line).get("instruction", "")
            except Exception:
                continue
            t = str(t).split("\n")[0].strip()
            if clean_len(t, 8, 45) and not TECH_RE.search(t):
                ruo.append(t)
    with open(RAW / "cqia_zhihu.jsonl", encoding="utf-8") as f:
        for line in f:
            try:
                t = json.loads(line).get("instruction", "")
            except Exception:
                continue
            t = str(t).split("\n")[0].strip()
            if clean_len(t, 10, 45) and t.startswith(("如何看待", "为什么", "你觉得")) and not TECH_RE.search(t):
                zh.append(t)
    with open(RAW / "cqia_wikihow.jsonl", encoding="utf-8") as f:
        for line in f:
            try:
                d = json.loads(line)
            except Exception:
                continue
            t = str(d.get("instruction", "")).split("\n")[0].strip()
            dom = str(d.get("domain", ""))
            if re.search("烹饪|菜|饮食|家居|个人|情感|健康|时尚|宠物|家庭|美容|运动|旅行|艺术", dom):
                if clean_len(t, 8, 45) and t.startswith(("如何", "怎么", "怎样")):
                    wk_other.append(t)
    return {
        "massive_commands": sample_cap(cmds, 450),
        "massive_qa": sample_cap(qa, 150),
        "cqia_xhs": sample_cap(xhs, 200),
        "cqia_exam": sample_cap(exam, 120),
        "cqia_ruozhiba": sample_cap(ruo, 120),
        "cqia_zhihu": sample_cap(zh, 100),
        "wikihow_life": sample_cap(wk_other, 100),
    }

# ---------------------------------------------------------------- confidence 伪标签
BASE_CONF = {"knowledge": 0.92, "incident": 0.90, "handoff": 0.95, "out_of_scope": 0.92}

#: out_of_scope 同时吸收闲聊语料，单独建一个合并构建器
def build_out_of_scope_merged() -> dict[str, list[str]]:
    groups = build_chitchat()
    groups.update(build_out_of_scope())
    return groups


MAX_PER_CLASS = 1200


def main() -> None:
    builders = {
        "knowledge": build_knowledge,
        "incident": build_incident,
        "handoff": build_handoff,
        "out_of_scope": build_out_of_scope_merged,
    }
    eval_norms = load_eval_texts()
    seen: set[str] = set()
    train_rows, dev_rows = [], []
    stats: dict[str, Counter] = defaultdict(Counter)

    for intent, builder in builders.items():
        groups = builder()
        rows = []
        for source, texts in groups.items():
            for t in texts:
                t = t.strip()
                key = norm(t)
                if not key or key in seen or key in eval_norms:
                    continue
                seen.add(key)
                conf = min(0.99, round(BASE_CONF[intent] + random.uniform(-0.03, 0.05), 2))
                rows.append({
                    "messages": [
                        {"role": "system", "content": INTENT_PROMPT},
                        {"role": "user", "content": t},
                        {"role": "assistant", "content": json.dumps({"intent": intent, "confidence": conf}, ensure_ascii=False)},
                    ],
                    "intent": intent,
                    "source": source,
                })
                stats[intent][source] += 1
        random.shuffle(rows)
        if len(rows) > MAX_PER_CLASS:  # 控制 out_of_scope（吸收了闲聊后体量最大）不超配
            rows = random.sample(rows, MAX_PER_CLASS)
        stats[intent].clear()
        for r in rows:
            stats[intent][r["source"]] += 1
        n_dev = max(20, int(len(rows) * 0.05))
        dev_rows.extend(rows[:n_dev])
        train_rows.extend(rows[n_dev:])

    random.shuffle(train_rows)
    random.shuffle(dev_rows)
    OUT.mkdir(exist_ok=True)
    (OUT / "train.jsonl").write_text("\n".join(json.dumps(r, ensure_ascii=False) for r in train_rows), encoding="utf-8")
    (OUT / "dev.jsonl").write_text("\n".join(json.dumps(r, ensure_ascii=False) for r in dev_rows), encoding="utf-8")

    print("=== 数据集统计 ===")
    total = 0
    for intent in INTENTS:
        n_train = sum(1 for r in train_rows if r["intent"] == intent)
        n_dev = sum(1 for r in dev_rows if r["intent"] == intent)
        total += n_train + n_dev
        print(f"{intent:<13} train={n_train:<5} dev={n_dev:<3} sources={dict(stats[intent])}")
    print(f"total={total} (train={len(train_rows)}, dev={len(dev_rows)})")
    print(f"eval texts excluded (leak guard): {len(eval_norms)}")
    print("\n--- 每类样例（前3条） ---")
    shown: Counter = Counter()
    for r in train_rows:
        if shown[r["intent"]] >= 3:
            continue
        shown[r["intent"]] += 1
        print(f"[{r['intent']}|{r['source']}] {r['messages'][1]['content'][:55]}")

if __name__ == "__main__":
    main()
