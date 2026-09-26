// Package api 提供 HTTP 接口。
//
// 设计取舍：用标准库 net/http 而非 Web 框架。一期只有 6 个端点，
// 引入框架换来的是路由糖和中间件生态，代价是多一套依赖与隐式行为。
// Go 1.22+ 的 ServeMux 已支持方法与路径参数，够用。
//
// 分层约定：handler 只做参数解析、调用服务、写响应。
// 业务规则一律不在这一层，否则无法被单元测试覆盖。
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mac/helpdesk-agent/internal/agent"
	"github.com/mac/helpdesk-agent/internal/assign"
	"github.com/mac/helpdesk-agent/internal/conversation"
	"github.com/mac/helpdesk-agent/internal/domain"
	"github.com/mac/helpdesk-agent/internal/eval"
	"github.com/mac/helpdesk-agent/internal/seed"
	"github.com/mac/helpdesk-agent/internal/store"
	"github.com/mac/helpdesk-agent/internal/ticket"
)

// Server 组装 HTTP 接口。
type Server struct {
	conversations *conversation.Service
	tickets       *ticket.Service
	store         store.Store
	// Mode 说明当前后端模式（真实模型 / 离线脚本），会在健康检查中返回，
	// 避免使用者误把离线演示当作真实模型效果。
	Mode string
}

// New 构造 HTTP 服务。
func New(st store.Store, conversations *conversation.Service, tickets *ticket.Service, mode string) *Server {
	return &Server{
		conversations: conversations,
		tickets:       tickets,
		store:         st,
		Mode:          mode,
	}
}

// Handler 返回已注册全部路由的 http.Handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.handleHealth)

	mux.HandleFunc("POST /api/conversations", s.handleCreateConversation)
	mux.HandleFunc("GET /api/conversations", s.handleListConversations)
	mux.HandleFunc("GET /api/conversations/{id}", s.handleGetConversation)
	mux.HandleFunc("POST /api/conversations/{id}/messages", s.handleSendMessage)
	mux.HandleFunc("POST /api/conversations/{id}/close", s.handleCloseConversation)

	mux.HandleFunc("GET /api/tickets", s.handleListTickets)
	mux.HandleFunc("GET /api/tickets/{id}", s.handleGetTicket)
	mux.HandleFunc("GET /api/employees", s.handleListEmployees)

	// 派单评测（离线跑 v2 数据集，返回机读报告供前端可视化与事后审查）。
	mux.HandleFunc("POST /api/eval/assign-v2", s.handleRunEval)

	return jsonifyStdlibErrors(mux)
}

// jsonifyStdlibErrors 把标准库自动产生的 404 / 405 纯文本响应统一改成 JSON。
//
// 为什么需要它：ServeMux 在路径或方法未匹配时会自行写纯文本
// （"404 page not found" / "Method Not Allowed"），而本服务其余响应都是 JSON。
// 客户端按统一格式解析会在这些情况下解析失败，且拿到的信息不可读。
//
// 实现方式：先缓冲完整响应，再统一决定输出。
// 不要试图在 Write 里边写边改写——业务 handler 自己写 404 后，
// ServeMux 还会追加一段纯文本，逐次改写会输出两段 JSON（实测踩过）。
func jsonifyStdlibErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// SSE 流式请求必须透传：不能缓冲（否则事件要等整轮跑完才发出，失去实时性），
		// 也不能包装 ResponseWriter（否则丢失 http.Flusher，SSE 无法逐段推送）。
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			next.ServeHTTP(w, r)
			return
		}
		buffer := &bufferedResponse{header: make(http.Header), status: http.StatusOK}
		next.ServeHTTP(buffer, r)

		body := bytes.TrimSpace(buffer.body.Bytes())
		// 业务代码已给出 JSON（或其它结构化内容）时原样放行。
		if len(body) > 0 && (body[0] == '{' || body[0] == '[') {
			copyHeaders(w.Header(), buffer.header)
			w.WriteHeader(buffer.status)
			_, _ = w.Write(buffer.body.Bytes())
			return
		}

		// 到这里说明响应是标准库的纯文本错误，改写为统一 JSON。
		status := buffer.status
		if status == http.StatusOK {
			// 完全没有匹配到任何处理逻辑。
			status = http.StatusNotFound
		}
		message := strings.TrimSpace(string(body))
		if message == "" || strings.HasPrefix(message, "<") {
			message = http.StatusText(status)
		}
		writeError(w, status, errors.New(message))
	})
}

// bufferedResponse 收集完整响应以便统一改写。
type bufferedResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
	wrote  bool
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(status int) {
	if b.wrote {
		return
	}
	b.status = status
	b.wrote = true
}

func (b *bufferedResponse) Write(data []byte) (int, error) {
	b.wrote = true
	return b.body.Write(data)
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

// ---- 响应结构 ----

type errorResponse struct {
	Error string `json:"error"`
}

type healthResponse struct {
	Status string `json:"status"`
	Mode   string `json:"mode"`
	Counts struct {
		Conversations int `json:"conversations"`
		Tickets       int `json:"tickets"`
		Employees     int `json:"employees"`
	} `json:"counts"`
}

type createConversationRequest struct {
	Title         string `json:"title"`
	SourceChannel string `json:"sourceChannel"`
	CustomerRef   string `json:"customerRef"`
}

type sendMessageRequest struct {
	Content string `json:"content"`
	// RequestID 为幂等键。客户端重试时应复用同一个值，
	// 否则重试会变成两条消息。
	RequestID string `json:"requestId"`
}

type sendMessageResponse struct {
	Duplicate bool         `json:"duplicate"`
	RequestID string       `json:"requestId,omitempty"`
	Reply     string       `json:"reply,omitempty"`
	Messages  []messageDTO `json:"messages,omitempty"`
	Turn      *turnDTO     `json:"turn,omitempty"`
}

type turnDTO struct {
	Interrupted bool  `json:"interrupted"`
	TicketID    int64 `json:"ticketId,omitempty"`
}

type messageDTO struct {
	ID        int64  `json:"id"`
	Sender    string `json:"sender"`
	Content   string `json:"content"`
	CreatedAt string `json:"createdAt"`
}

type conversationDTO struct {
	ID            int64  `json:"id"`
	Title         string `json:"title"`
	Status        string `json:"status"`
	SourceChannel string `json:"sourceChannel"`
	CustomerRef   string `json:"customerRef,omitempty"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
}

type conversationDetailDTO struct {
	Conversation conversationDTO `json:"conversation"`
	Messages     []messageDTO    `json:"messages"`
}

type ticketDTO struct {
	ID           int64    `json:"id"`
	Title        string   `json:"title"`
	Description  string   `json:"description,omitempty"`
	Category     string   `json:"category"`
	Priority     string   `json:"priority"`
	Status       string   `json:"status"`
	AssigneeID   int64    `json:"assigneeId,omitempty"`
	AssigneeName string   `json:"assigneeName,omitempty"`
	MissingInfo  []string `json:"missingInfo,omitempty"`
	Conversation int64    `json:"conversationId,omitempty"`
}

type ticketDetailDTO struct {
	Ticket      ticketDTO     `json:"ticket"`
	Assignments []string      `json:"assignments,omitempty"`
	Progress    []progressDTO `json:"progress,omitempty"`
}

type progressDTO struct {
	Kind    string `json:"kind"`
	Content string `json:"content"`
	At      string `json:"at"`
}

type employeeDTO struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Active        bool   `json:"active"`
	CurrentLoad   int    `json:"currentLoad"`
	MaxConcurrent int    `json:"maxConcurrent"`
}

// ---- handlers ----

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	var resp healthResponse
	resp.Status = "ok"
	resp.Mode = s.Mode
	resp.Counts.Conversations = len(s.store.ListConversations())
	resp.Counts.Tickets = len(s.store.ListTickets())
	resp.Counts.Employees = len(s.store.ListEmployees())
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleCreateConversation(w http.ResponseWriter, r *http.Request) {
	var req createConversationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	conv, err := s.conversations.Start(conversation.StartInput{
		Title:         req.Title,
		SourceChannel: req.SourceChannel,
		CustomerRef:   req.CustomerRef,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, toConversationDTO(conv))
}

func (s *Server) handleListConversations(w http.ResponseWriter, _ *http.Request) {
	items := s.conversations.List()
	ret := make([]conversationDTO, 0, len(items))
	for _, item := range items {
		ret = append(ret, toConversationDTO(item))
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": ret, "total": len(ret)})
}

func (s *Server) handleGetConversation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	detail, err := s.conversations.Get(id)
	if err != nil {
		writeLookupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, conversationDetailDTO{
		Conversation: toConversationDTO(detail.Conversation),
		Messages:     toMessageDTOs(detail.Messages),
	})
}

func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req sendMessageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// RequestID 未提供时按内容+会话生成，保证客户端不传也不会重复建单。
	requestID := strings.TrimSpace(req.RequestID)
	if requestID == "" {
		requestID = defaultRequestID(id, req.Content)
	}

	// SSE 流式：客户端显式声明 Accept: text/event-stream 时走流式返回。
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.handleSendMessageSSE(w, r, id, req, requestID)
		return
	}

	result, err := s.conversations.Send(r.Context(), id, req.Content, requestID)
	if err != nil {
		// 会话已关闭与不存在都是客户端可纠正的问题，用 4xx 而非 500。
		if errors.Is(err, domain.ErrConversationClosed) {
			writeError(w, http.StatusConflict, err)
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}

	resp := sendMessageResponse{Duplicate: result.Duplicate, RequestID: requestID}
	if result.Duplicate {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Messages = toMessageDTOs([]domain.Message{result.CustomerMessage})
	if result.Reply != nil {
		resp.Reply = result.Reply.Content
		resp.Messages = append(resp.Messages, toMessageDTO(*result.Reply))
	}
	if result.Turn != nil {
		resp.Turn = &turnDTO{
			Interrupted: result.Turn.Interrupted,
			TicketID:    result.Turn.TicketID,
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleSendMessageSSE 以 Server-Sent Events 流式返回一次消息处理：
//   - event: event  回合事件（round / tool / confirm），数据为 agent.TurnEvent JSON
//   - event: delta  回复文本增量，数据为 {"text":"..."}
//   - event: done   回合结束，数据含 duplicate / interrupted / ticketId
//   - event: error  出错信息
//
// 事件与文本增量在 agent 运行过程中实时推送（经 agent.WithStreamSink 注入 context），
// 使前端能在工具执行的同时逐步展开步骤、逐字滚出回复。
func (s *Server) handleSendMessageSSE(w http.ResponseWriter, r *http.Request, id int64, req sendMessageRequest, requestID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("当前 HTTP 实现不支持 SSE"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 关掉反向代理缓冲，让事件即时到达

	sink := &agent.StreamSink{
		OnDelta: func(delta string) {
			writeSSEEvent(w, flusher, "delta", map[string]any{"text": delta})
		},
		OnEvent: func(e agent.TurnEvent) {
			writeSSEEvent(w, flusher, "event", e)
		},
	}
	ctx := agent.WithStreamSink(r.Context(), sink)
	// 派单过程事件（服务解析 → 候选排序 → LLM 决策 → 完成）也走 SSE。
	ctx = assign.WithDispatchObserver(ctx, func(e assign.DispatchEvent) {
		writeSSEEvent(w, flusher, "dispatch", e)
	})

	result, err := s.conversations.Send(ctx, id, req.Content, requestID)
	if err != nil {
		writeSSEEvent(w, flusher, "error", map[string]any{"error": err.Error()})
		return
	}

	done := map[string]any{"duplicate": result.Duplicate}
	if result.Reply != nil {
		done["reply"] = result.Reply.Content
	}
	if result.Turn != nil {
		done["interrupted"] = result.Turn.Interrupted
		done["ticketId"] = result.Turn.TicketID
	}
	writeSSEEvent(w, flusher, "done", done)
}

// writeSSEEvent 序列化并 flush 一条 SSE 事件。
func writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, event string, data any) {
	payload, err := json.Marshal(data)
	if err != nil {
		payload = []byte(`{}`)
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
	flusher.Flush()
}

func (s *Server) handleCloseConversation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	conv, err := s.conversations.Close(id)
	if err != nil {
		writeLookupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toConversationDTO(conv))
}

func (s *Server) handleListTickets(w http.ResponseWriter, _ *http.Request) {
	items := s.store.ListTickets()
	ret := make([]ticketDTO, 0, len(items))
	for _, item := range items {
		ret = append(ret, s.toTicketDTO(item))
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": ret, "total": len(ret)})
}

func (s *Server) handleGetTicket(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	detail, err := s.tickets.Detail(id)
	if err != nil {
		writeLookupError(w, err)
		return
	}
	resp := ticketDetailDTO{Ticket: s.toTicketDTO(detail.Ticket)}
	// 指派理由对业务方复核「为什么派给他」是必需的，一并返回。
	for _, item := range detail.Assignments {
		resp.Assignments = append(resp.Assignments, item.Reason)
	}
	for _, item := range detail.Progress {
		resp.Progress = append(resp.Progress, progressDTO{
			Kind:    string(item.Kind),
			Content: item.Content,
			At:      item.CreatedAt.Format("2006-01-02 15:04:05"),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListEmployees(w http.ResponseWriter, _ *http.Request) {
	items := s.store.ListEmployees()
	ret := make([]employeeDTO, 0, len(items))
	for _, item := range items {
		ret = append(ret, employeeDTO{
			ID: item.ID, Name: item.Name, Active: item.Active,
			CurrentLoad:   len(s.store.FindOpenTicketsByAssignee(item.ID)),
			MaxConcurrent: item.MaxConcurrent,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": ret, "total": len(ret)})
}

// handleRunEval 跑一遍派单 v2 评测并返回机读报告。
//
// 与 CLI（make eval）同一套逻辑（eval.RunPipelineV2Eval），差别只在这里返回 JSON
// 供前端渲染：三轴通过率、Stage 漏斗、以及逐条失败样本（事后审查的依据）。
//
// 评测姿态固定为离线：不注入 Stage 2 chooser、员工负载归零。否则同一条 case
// 两次跑会因为负载漂移落到不同员工，指标不可复现。
func (s *Server) handleRunEval(w http.ResponseWriter, _ *http.Request) {
	path := locateEvalDataset()
	ds, err := eval.LoadPipelineV2Dataset(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("加载评测集 %s: %w", path, err))
		return
	}
	pipeline := assign.NewPipeline(
		assign.NewBM25ServiceResolver(3),
		assign.NoopSimilarIndex{},
		nil,
	)
	rep := eval.RunPipelineV2Eval(ds, pipeline, evalDirectory())
	rep.Dataset = filepath.Base(path)
	writeJSON(w, http.StatusOK, rep)
}

// locateEvalDataset 相对工作目录定位 v2 评测集。serve 从仓库根启动时可直接命中，
// 其余情况依次向上回退，避免换目录就 500。
func locateEvalDataset() string {
	candidates := []string{
		"eval/datasets/assignment_v2.json",
		"../eval/datasets/assignment_v2.json",
		"../../eval/datasets/assignment_v2.json",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return candidates[0]
}

// evalDirectory 构造评测专用目录：员工负载全部归零，保证可复现。
// 生产路径的实时负载由 ticket.Service.candidates 计算，不走这里。
func evalDirectory() assign.Directory {
	emps := seed.Employees()
	for i := range emps {
		emps[i].CurrentLoad = 0
	}
	return assign.Directory{
		Employees:  emps,
		Extensions: seed.ExtensionsByEmployeeID(),
		Services:   seed.ServicesByID(),
	}
}

// ---- 辅助 ----

func (s *Server) toTicketDTO(t domain.Ticket) ticketDTO {
	dto := ticketDTO{
		ID:           t.ID,
		Title:        t.Title,
		Description:  t.Description,
		Category:     string(t.Category),
		Priority:     string(t.Priority),
		Status:       string(t.Status),
		AssigneeID:   t.AssigneeID,
		MissingInfo:  t.MissingInfo,
		Conversation: t.ConversationID,
	}
	if t.AssigneeID > 0 {
		if emp, err := s.store.GetEmployee(t.AssigneeID); err == nil {
			dto.AssigneeName = emp.Name
		}
	}
	return dto
}

func toConversationDTO(c domain.Conversation) conversationDTO {
	return conversationDTO{
		ID:            c.ID,
		Title:         c.Title,
		Status:        string(c.Status),
		SourceChannel: c.SourceChannel,
		CustomerRef:   c.CustomerRef,
		CreatedAt:     c.CreatedAt.Format("2006-01-02 15:04:05"),
		UpdatedAt:     c.UpdatedAt.Format("2006-01-02 15:04:05"),
	}
}

func toMessageDTO(item domain.Message) messageDTO {
	return messageDTO{
		ID:        item.ID,
		Sender:    string(item.Sender),
		Content:   item.Content,
		CreatedAt: item.CreatedAt.Format("2006-01-02 15:04:05"),
	}
}

func toMessageDTOs(items []domain.Message) []messageDTO {
	ret := make([]messageDTO, 0, len(items))
	for _, item := range items {
		ret = append(ret, toMessageDTO(item))
	}
	return ret
}

// defaultRequestID 在客户端未提供幂等键时生成一个稳定的替代值。
//
// 用「会话 + 内容」而非随机数：客户端未传幂等键时通常是没意识到重试问题，
// 用内容派生至少能拦住「同一句话被重复提交」这类最常见的重复。
func defaultRequestID(conversationID int64, content string) string {
	normalized := strings.Join(strings.Fields(strings.TrimSpace(content)), "")
	return "auto:" + strconv.FormatInt(conversationID, 10) + ":" + normalized
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, errors.New("路径参数 id 必须是正整数"))
		return 0, false
	}
	return id, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	if r.Body == nil {
		writeError(w, http.StatusBadRequest, errors.New("请求体不能为空"))
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	// 拒绝未知字段：客户端拼错字段名时应立即报错，
	// 而不是被静默忽略后表现为「参数没生效」。
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("请求体解析失败: "+err.Error()))
		return false
	}
	return true
}

func writeLookupError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeError(w, http.StatusBadRequest, err)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, errorResponse{Error: err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(payload)
}
