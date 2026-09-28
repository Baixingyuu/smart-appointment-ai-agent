"use client";

import { HttpAgent } from "@ag-ui/client";
import {
  useAgUiRuntime,
  useAgUiInterrupts,
  useAgUiSubmitInterruptResponses,
} from "@assistant-ui/react-ag-ui";
import {
  AssistantRuntimeProvider,
  ThreadPrimitive,
  MessagePrimitive,
  ComposerPrimitive,
} from "@assistant-ui/react";
import { useState } from "react";

const agent = new HttpAgent({
  url: process.env.NEXT_PUBLIC_AGUI_URL ?? "http://127.0.0.1:8000/ag-ui",
});

function parseArgs(raw: unknown): Record<string, unknown> {
  if (raw == null) return {};
  if (typeof raw === "string") {
    try {
      return JSON.parse(raw);
    } catch {
      return {};
    }
  }
  return raw as Record<string, unknown>;
}

const toolStyle = {
  box: {
    margin: "8px 0",
    border: "1px solid #e5e5ea",
    borderRadius: 8,
    padding: "8px 12px",
    background: "#f7f7f8",
    fontSize: 13,
  },
  name: { fontWeight: 600, marginBottom: 4 },
  args: {
    margin: "4px 0 0",
    whiteSpace: "pre-wrap" as const,
    wordBreak: "break-all" as const,
    fontSize: 12,
    color: "#666",
    maxHeight: 140,
    overflow: "auto" as const,
    fontFamily: "ui-monospace, monospace",
  },
  result: { marginTop: 4, fontSize: 12, color: "#333" },
};

/** 工具调用：assistant-ui 原生工具调用 part 的渲染（toolName + 参数 + 结果）。 */
function ToolCallUI(props: {
  toolName: string;
  argsText?: string;
  result?: unknown;
  isError?: boolean;
  isPreliminary?: boolean;
}) {
  return (
    <div style={toolStyle.box}>
      <div style={toolStyle.name}>🔧 {props.toolName}</div>
      {props.argsText ? <pre style={toolStyle.args}>{props.argsText}</pre> : null}
      {props.isPreliminary ? (
        <div style={{ fontSize: 12, color: "#999" }}>执行中…</div>
      ) : props.result != null ? (
        <div style={{ ...toolStyle.result, color: props.isError ? "#c00" : "#333" }}>
          {String(props.result)}
        </div>
      ) : null}
    </div>
  );
}

function Message() {
  return (
    <MessagePrimitive.Root style={{ marginBottom: 12 }}>
      <MessagePrimitive.Parts
        components={{ tools: { Fallback: ToolCallUI as never } }}
      />
    </MessagePrimitive.Root>
  );
}

/** 富信息卡片：把 AG-UI 的 interrupt（确认/推荐）渲染成可点选的卡片。 */
function InterruptLayer() {
  const interrupts = useAgUiInterrupts();
  const submit = useAgUiSubmitInterruptResponses();
  const [picks, setPicks] = useState<Record<string, string>>({});
  const [slots, setSlots] = useState<Record<string, string | null>>({});
  const [busy, setBusy] = useState(false);

  if (interrupts.length === 0) return null;

  const btn = {
    border: "1px solid #cfcfcf",
    borderRadius: 8,
    background: "#fff",
    cursor: "pointer",
    fontSize: 13,
    padding: "6px 14px",
  };

  return (
    <div
      style={{
        position: "fixed",
        inset: 0,
        background: "rgba(0,0,0,0.35)",
        display: "flex",
        alignItems: "center",
        justifyContent: "center",
        zIndex: 50,
      }}
    >
      {interrupts.map((it) => {
        const meta = (it.metadata ?? {}) as { toolName?: string; args?: unknown };
        const args = parseArgs(meta.args);
        const resolve = (payload?: unknown) =>
          submit([{ interruptId: it.id, status: "resolved", payload }]);
        const cancel = () => submit([{ interruptId: it.id, status: "cancelled" }]);

        if (meta.toolName === "suggest_assignment") {
          const ids = (args.candidate_ids as string[]) ?? [];
          const timeSlots = (args.time_slots as string[]) ?? [];
          const sel = picks[it.id];
          const ts = slots[it.id] ?? null;
          return (
            <div key={it.id} style={cardStyle}>
              <h3>推荐工程师，请选择一位</h3>
              <div style={optColumn}>
                {ids.map((id) => (
                  <button
                    key={id}
                    type="button"
                    disabled={busy}
                    style={sel === id ? { ...btn, ...optSel } : btn}
                    onClick={() => setPicks({ ...picks, [it.id]: id })}
                  >
                    <span style={{ fontWeight: 600 }}>员工 {id}</span>
                  </button>
                ))}
              </div>
              {timeSlots.length > 0 && (
                <>
                  <h4 style={{ fontSize: 13, margin: "12px 0 6px" }}>
                    可选上门时段（可跳过）
                  </h4>
                  <div style={optColumn}>
                    {timeSlots.map((t) => (
                      <button
                        key={t}
                        type="button"
                        disabled={busy}
                        style={ts === t ? { ...btn, ...optSel } : btn}
                        onClick={() => setSlots({ ...slots, [it.id]: t })}
                      >
                        {t}
                      </button>
                    ))}
                  </div>
                </>
              )}
              <div style={{ display: "flex", gap: 8, marginTop: 12 }}>
                <button
                  disabled={busy || !sel}
                  style={{ ...btn, background: "#1f6feb", borderColor: "#1f6feb", color: "#fff" }}
                  onClick={() => resolve({ engineer_id: sel, time_slot: ts })}
                >
                  确定指派{ts ? `（${ts}）` : ""}
                </button>
                <button disabled={busy} style={btn} onClick={cancel}>
                  取消
                </button>
              </div>
            </div>
          );
        }

        // 确认卡（create_ticket / assign_ticket 等）
        return (
          <div key={it.id} style={cardStyle}>
            <h3>等您对「{meta.toolName}」表个态</h3>
            {args.title != null && (
              <p style={{ margin: "4px 0" }}>
                <b>标题</b>：{String(args.title)}
              </p>
            )}
            {args.description != null && (
              <p style={{ margin: "4px 0" }}>
                <b>描述</b>：{String(args.description)}
              </p>
            )}
            {(args.category != null || args.priority != null) && (
              <p style={{ margin: "4px 0" }}>
                <b>分类/优先级</b>：{String(args.category ?? "")} /{" "}
                {String(args.priority ?? "")}
              </p>
            )}
            <div style={{ display: "flex", gap: 8, marginTop: 12 }}>
              <button
                disabled={busy}
                style={{ ...btn, background: "#1f6feb", borderColor: "#1f6feb", color: "#fff" }}
                onClick={() => resolve()}
              >
                确认执行
              </button>
              <button disabled={busy} style={btn} onClick={cancel}>
                不执行
              </button>
            </div>
          </div>
        );
      })}
    </div>
  );
}

const cardStyle: React.CSSProperties = {
  maxWidth: 520,
  width: "90%",
  background: "#fff",
  border: "1px solid #d9d9d9",
  borderRadius: 10,
  padding: "14px 16px",
};
const optColumn: React.CSSProperties = {
  display: "flex",
  flexDirection: "column",
  gap: 8,
  marginBottom: 4,
};
const optSel: React.CSSProperties = { borderColor: "#1f6feb", background: "#eaf2ff" };

export default function Page() {
  const runtime = useAgUiRuntime({ agent });
  return (
    <AssistantRuntimeProvider runtime={runtime}>
      <InterruptLayer />
      <div
        style={{
          height: "100vh",
          display: "flex",
          flexDirection: "column",
          maxWidth: 720,
          margin: "0 auto",
          padding: 16,
          boxSizing: "border-box",
        }}
      >
        <h1 style={{ fontSize: 17, margin: "0 0 12px" }}>
          IT 服务台受理（assistant-ui）
        </h1>
        <ThreadPrimitive.Root
          style={{
            flex: 1,
            minHeight: 0,
            display: "flex",
            flexDirection: "column",
            border: "1px solid #d9d9d9",
            borderRadius: 10,
            overflow: "hidden",
          }}
        >
          <ThreadPrimitive.Viewport style={{ flex: 1, overflowY: "auto", padding: 12 }}>
            <ThreadPrimitive.Messages>{() => <Message />}</ThreadPrimitive.Messages>
          </ThreadPrimitive.Viewport>
          <ComposerPrimitive.Root
            style={{ display: "flex", gap: 8, padding: 12, borderTop: "1px solid #eee" }}
          >
            <ComposerPrimitive.Input
              placeholder="说说您的 IT 问题…"
              style={{ flex: 1, border: "1px solid #d9d9d9", borderRadius: 8, padding: "8px 12px" }}
            />
            <ComposerPrimitive.Send
              style={{
                border: "1px solid #1f6feb",
                background: "#1f6feb",
                color: "#fff",
                borderRadius: 8,
                padding: "8px 16px",
              }}
            >
              发送
            </ComposerPrimitive.Send>
          </ComposerPrimitive.Root>
        </ThreadPrimitive.Root>
      </div>
    </AssistantRuntimeProvider>
  );
}
