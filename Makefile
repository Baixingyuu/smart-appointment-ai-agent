PY := .venv/bin/python
PYTEST := .venv/bin/python -m pytest

.PHONY: probe probe-p3 index test annotate annotate-recheck validate freeze eval-extract eval-assign eval-retrieval eval-trajectory eval chat

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

index:
	$(PY) -m helpdesk.knowledge

test:
	$(PYTEST) -q

annotate:
	$(PY) eval/annotation/gen_sheet.py

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
	$(PY) -m helpdesk.app
