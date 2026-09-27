PY := .venv/bin/python
PYTEST := .venv/bin/python -m pytest

.PHONY: probe probe-p3 probe-v4 probe-p5 serve web index test annotate annotate-roster annotate-recheck validate freeze eval-extract eval-assign eval-retrieval eval-trajectory eval chat

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

eval-extract:
	$(PY) -m helpdesk.eval.run extract

eval-assign:
	$(PY) -m helpdesk.eval.run assign

eval-retrieval:
	$(PY) -m helpdesk.eval.run retrieval

eval-trajectory:
	$(PY) -m helpdesk.eval.run trajectory

eval: eval-extract eval-retrieval eval-trajectory

chat:
	$(PY) -m helpdesk.app $(ARGS)
