// 通过 context 注入的回合流式回调，供 HTTP SSE 等"中间层调用方"使用。
//
// 与 TurnInput.Stream / WithEventObserver 的分工：
//   - TurnInput.Stream 与 WithEventObserver 是"调用方直接传参"，适合 chat.go
//     这类能直接拿到 Agent 的调用方；
//   - StreamSink 走 context，适合 conversation.Service.Send 这种中间层——SSE 的
//     事件要穿透中间层到达 Agent，又不想让 Send 的签名背两个回调参数，就用
//     context 承载（HTTP 请求的 ctx 天然贯穿整条调用链）。
package agent

import "context"

// StreamSink 一次回合的流式输出与事件回调集合。
type StreamSink struct {
	// OnDelta 逐段接收最终回复文本；为 nil 时不流式。
	OnDelta func(delta string)
	// OnEvent 接收回合事件（round / tool / confirm）；为 nil 时不外显过程。
	OnEvent func(TurnEvent)
}

type streamSinkKey struct{}

// WithStreamSink 把流式回调塞进 context，供 Agent.Run 在回合内消费。
func WithStreamSink(ctx context.Context, sink *StreamSink) context.Context {
	return context.WithValue(ctx, streamSinkKey{}, sink)
}

func streamSinkFrom(ctx context.Context) *StreamSink {
	sink, _ := ctx.Value(streamSinkKey{}).(*StreamSink)
	return sink
}

// streamDelta 解析本回合的流式文本回调：优先 TurnInput.Stream，其次 ctx 注入的 StreamSink。
func (a *Agent) streamDelta(ctx context.Context, input TurnInput) func(string) {
	if input.Stream != nil {
		return input.Stream
	}
	if sink := streamSinkFrom(ctx); sink != nil {
		return sink.OnDelta
	}
	return nil
}
