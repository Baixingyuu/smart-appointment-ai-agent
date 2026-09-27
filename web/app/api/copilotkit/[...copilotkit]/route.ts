import { HttpAgent } from "@ag-ui/client";
import { CopilotRuntime, createCopilotRuntimeHandler } from "@copilotkit/runtime/v2";

/**
 * 运行时只做代理：不接任何 LLM key，模型在服务端那侧。
 *
 * agent 用 `HttpAgent` 指向我们的 AG-UI 桥（`src/helpdesk/agui_bridge.py`），
 * 桥再把这一发落到 AgentScope 托管路径的 `POST /chat` + `GET /sessions/{id}/stream`。
 * 不配 basePath：路由是后缀匹配（`.../agent/<id>/run`、`.../info`），
 * 挂在 catch-all 上正好。
 */
const runtime = new CopilotRuntime({
  agents: {
    helpdesk: new HttpAgent({
      url: process.env.HELPDESK_AGUI_URL ?? "http://127.0.0.1:8000/ag-ui",
    }),
  },
});

const handler = createCopilotRuntimeHandler({ runtime });

export const POST = handler;
export const GET = handler;
