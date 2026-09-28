"use client";

import { useState } from "react";
import { CopilotChat, CopilotKit, useInterrupt } from "@copilotkit/react-core/v2";

const AGENT_ID = "helpdesk";

type PendingCall = {
  toolName?: string;
  args?: string;
};

type Args = {
  title?: string;
  description?: string;
  category?: string;
  priority?: string;
  assignee_id?: string | null;
  missing_info?: string[];
};

type SuggestArgs = {
  ticket_id?: number;
  candidate_ids?: string[];
  time_slots?: string[];
  need?: string;
  duration_minutes?: number;
};

function parseArgs(raw: unknown): Args {
  if (typeof raw !== "string") return (raw ?? {}) as Args;
  try {
    return JSON.parse(raw) as Args;
  } catch {
    return {};
  }
}

/**
 * 待确认卡片：桥把框架原生的 require_user_confirm 落成 AG-UI interrupt，
 * 这里的 resolve/cancel 就是 POST /chat 的 confirmed=true/false。
 */
function ConfirmCard() {
  const [busy, setBusy] = useState(false);

  useInterrupt({
    // 挂在 <CopilotChat> 外面时没有"当前 chat agent"可继承，不写就去找 default 这个 agent。
    agentId: AGENT_ID,
    enabled: ({ value }) => (value as { reason?: string } | undefined)?.reason === "tool_call",
    render: ({ interrupt, resolve, cancel }) => {
      const meta = (interrupt?.metadata ?? {}) as PendingCall;
      const args = parseArgs(meta.args);
      const missing = args.missing_info ?? [];
      const answer = (fn: () => Promise<unknown>) => async () => {
        setBusy(true);
        try {
          await fn();
        } finally {
          setBusy(false);
        }
      };
      return (
        <div className="card">
          <h3>等您对「{meta.toolName ?? interrupt?.reason}」表个态</h3>
          <dl>
            {args.title && (
              <>
                <dt>标题</dt>
                <dd>{args.title}</dd>
              </>
            )}
            {args.description && (
              <>
                <dt>描述</dt>
                <dd>{args.description}</dd>
              </>
            )}
            {(args.category || args.priority) && (
              <>
                <dt>分类/优先级</dt>
                <dd>
                  {args.category ?? "-"} / {args.priority ?? "-"}
                </dd>
              </>
            )}
            {args.assignee_id !== undefined && (
              <>
                <dt>指派</dt>
                <dd>{args.assignee_id ?? "转人工待认领"}</dd>
              </>
            )}
            <dt>缺的信息</dt>
            <dd>{missing.length ? missing.join("、") : "无"}</dd>
          </dl>
          <div className="row">
            <button
              className="primary"
              disabled={busy}
              onClick={answer(() => resolve(undefined, interrupt?.id as string))}
              type="button"
            >
              确认执行
            </button>
            <button disabled={busy} onClick={answer(() => cancel(interrupt?.id as string))} type="button">
              不执行
            </button>
          </div>
        </div>
      );
    },
  });

  return null;
}

/**
 * 推荐卡片：LLM 用 suggest_assignment 把候选工程师 + 可选时段弹出来，
 * 用户点选工程师（可选时段）后，resolve 把选择放进 AG-UI 的 payload 回传。
 */
function SuggestCard() {
  const [busy, setBusy] = useState(false);
  const [engineerId, setEngineerId] = useState<string | null>(null);
  const [timeSlot, setTimeSlot] = useState<string | null>(null);

  useInterrupt({
    agentId: AGENT_ID,
    enabled: ({ value }) =>
      ((value as { metadata?: { toolName?: string } } | undefined)?.metadata?.toolName) ===
      "suggest_assignment",
    render: ({ interrupt, resolve, cancel }) => {
      const meta = (interrupt?.metadata ?? {}) as { toolName?: string; args?: string };
      const args = parseArgs(meta.args) as SuggestArgs;
      const candidateIds = args.candidate_ids ?? [];
      const timeSlots = args.time_slots ?? [];
      const submit = async () => {
        if (!engineerId) return;
        setBusy(true);
        try {
          await resolve({ engineer_id: engineerId, time_slot: timeSlot }, interrupt?.id as string);
          setEngineerId(null);
          setTimeSlot(null);
        } finally {
          setBusy(false);
        }
      };
      return (
        <div className="card">
          <h3>推荐工程师，请选择一位</h3>
          <div className="options">
            {candidateIds.map((id) => (
              <button
                key={id}
                type="button"
                disabled={busy}
                className={engineerId === id ? "option selected" : "option"}
                onClick={() => setEngineerId(id)}
              >
                <span className="opt-name">员工 {id}</span>
              </button>
            ))}
          </div>
          {timeSlots.length > 0 && (
            <>
              <h4 className="opt-group">可选上门时段（可跳过）</h4>
              <div className="options">
                {timeSlots.map((t) => (
                  <button
                    key={t}
                    type="button"
                    disabled={busy}
                    className={timeSlot === t ? "option selected" : "option"}
                    onClick={() => setTimeSlot(t)}
                  >
                    {t}
                  </button>
                ))}
              </div>
            </>
          )}
          <div className="row">
            <button className="primary" disabled={busy || !engineerId} onClick={submit} type="button">
              确定指派{timeSlot ? `（${timeSlot}）` : ""}
            </button>
            <button
              disabled={busy}
              onClick={async () => {
                setBusy(true);
                try {
                  await cancel(interrupt?.id as string);
                } finally {
                  setBusy(false);
                }
              }}
              type="button"
            >
              取消
            </button>
          </div>
        </div>
      );
    },
  });

  return null;
}

export default function Page() {
  return (
    <CopilotKit runtimeUrl="/api/copilotkit">
      <ConfirmCard />
      <SuggestCard />
      <main>
        <h1>IT 服务台受理</h1>
        <p className="hint">
          说一句您的问题。缺信息时助手会通过 ask_user 追问；建单前会停下来等您表态。
        </p>
        <div className="chat">
          <CopilotChat agentId={AGENT_ID} labels={{ chatInputPlaceholder: "说说您的 IT 问题…" }} />
        </div>
      </main>
    </CopilotKit>
  );
}
