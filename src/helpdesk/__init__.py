"""业务包。

这个 `__init__` 只做一件事，且它必须是这一件事：把回环地址从系统代理手里摘出来。

httpx 的代理设置来自 `urllib.request.getproxies()`，macOS 下它读系统配置，
而系统配置里那份 bypass 名单（`scutil --proxy` 的 ExceptionsList 明明有 127.0.0.1）
它**不认** —— `proxy_bypass('http://127.0.0.1:11434')` 实测返回 False。
于是本机一旦开着 Clash 一类代理，这个进程里每一个 httpx 客户端都会收到一个空正文的
502：调 `ollama:11434` 的模型客户端与嵌入客户端、以及桥调自己 `/credential/` 的那一个。
uvicorn 侧连请求都没见过，看上去就像"模型自己坏了"。

放在这里而不是某个入口模块里，是因为早先它只在 `service.py`：托管路径修好了，
2026-09-27 20:13 一个只走 console 路径的探针（`probes/p6_specialists_cost.py`）
又在同一个 502 上撞了一次 —— 客户端由 `agent_factory` / `knowledge` 建，谁都可能单独被 import。
"""
from __future__ import annotations

import os

os.environ.setdefault("NO_PROXY", "127.0.0.1,localhost,::1")
os.environ.setdefault("no_proxy", "127.0.0.1,localhost,::1")
