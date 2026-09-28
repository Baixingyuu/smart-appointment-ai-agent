PY := .venv/bin/python
PYTEST := .venv/bin/python -m pytest

.PHONY: probe probe-p3 probe-v4 probe-p5 probe-p5-agui probe-p6 probe-p6-offline probe-p6-cron serve web index test annotate annotate-roster annotate-recheck validate freeze eval eval-trajectory eval-runtime chat

probe:
	$(PY) probes/p0_vector_store.py
	$(PY) probes/p0_round_accounting.py
	$(PY) probes/p0_park_resume.py

# 前者离线复现"同名工具重试被静默丢弃"（要真机才会撞的 id 冲突）；
# 后者是真模型端到端，五个场景合起来才覆盖方案 P3 那条验收：检索/查重/追问/待确认/建单/派单。
probe-p3:
	$(PY) probes/p3_tool_call_id_collision.py
	$(PY) probes/p3_live_smoke.py short
	$(PY) probes/p3_live_smoke.py intake
	$(PY) probes/p3_live_smoke.py dedup
	$(PY) probes/p3_live_smoke.py kb
	$(PY) probes/p3_live_smoke.py kb_vague

# 派单 v4 的三源召回：A 段纯检索隔离（不调模型），B 段真机一次 short 实测成本。
probe-v4:
	$(PY) probes/p4_dispatch_v4.py

# P5 前端接缝三段：A 离线（路由/工具面/事件映射/桥的翻译表，加 --offline 只跑它），
# B 真机走框架原生 HITL（证明自研确认桥在这条路上可以不要），
# C 真机只走 POST /ag-ui（前端看到的那一个端点）。B、C 互斥，各花一次模型钱。
probe-p5:
	$(PY) probes/p5_agui_service.py

probe-p5-agui:
	$(PY) probes/p5_agui_service.py agui

# P6 专线拆编排的成本对照：A 段离线（挂载差 + 嵌套账），B 段真机两句话各跑关/开两臂。
# 加 --offline 只跑 A，不花钱。
probe-p6:
	$(PY) probes/p6_specialists_cost.py

probe-p6-offline:
	$(PY) probes/p6_specialists_cost.py --offline

# AutoDream 的 cron 真机验证：把心跳临时调到每分钟，在一次性世界里等它真的醒、
# 真的调一次 run_autodream、第二轮真的被闸住。约 2–3 分钟；--once 只等第一次。
probe-p6-cron:
	$(PY) probes/p6_autodream_cron.py $(ARGS)

# 托管服务：create_app + AG-UI 协议中间件 + /ag-ui 桥，前端只跟它说话。
serve:
	$(PY) -m helpdesk.service $(ARGS)

# CopilotKit 官方 UI（Next.js）：得先起 serve。
web:
	cd web && npm run dev

index:
	$(PY) -m helpdesk.knowledge

test:
	$(PYTEST) -q

annotate:
	$(PY) eval/annotation/gen_sheet.py

# RUBRIC §4 的组织事实表按 catalog 打印，别手抄（名册 9→24 人时就漂过一次）
annotate-roster:
	$(PY) eval/annotation/gen_sheet.py --roster-md

# 隔一轮再填的 10 条复标子表（自一致性；不要先看第一次的答案）
annotate-recheck:
	$(PY) eval/annotation/gen_sheet.py --resample

validate:
	$(PY) eval/annotation/validate.py $(if $(SHEET),--sheet $(SHEET)) $(if $(RECHECK),--recheck $(RECHECK))

freeze:
	$(PY) eval/annotation/validate.py --freeze

# 端到端系统评测：真机跑 trajectory 全套，一次产出四个指标 ——
# ① 回答质量（LLM 评委 relevance/correctness）② 执行轨迹（闸门+步序）
# ③ P95 延迟（wall/model/tool/wait/residual 四段）④ Token 成本（input/output）。
eval:
	$(PY) -m helpdesk.eval.run system

# 离线轨迹：脚本化假模型跑同一套断言，不叫真模型，验证链路与量具（快、不花钱）。
eval-trajectory:
	$(PY) -m helpdesk.eval.run trajectory

# 运行时轴：读托管路径已落库的历史流量，算真实 P95 延迟与 token 分布（不叫模型）。
eval-runtime:
	$(PY) -m helpdesk.eval.run runtime

chat:
	$(PY) -m helpdesk.app $(ARGS)
