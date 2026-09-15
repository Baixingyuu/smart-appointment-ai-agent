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
	"net/http"
	"strconv"
	"strings"

	"github.com/mac/helpdesk-agent/internal/conversation"
	"github.com/mac/helpdesk-agent/internal/domain"
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
	mux.HandleFunc("GET /api/skills", s.handleListSkills)
	mux.HandleFunc("GET /api/employees", s.handleListEmployees)

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
	ID            int64    `json:"id"`
	Title         string   `json:"title"`
	Description   string   `json:"description,omitempty"`
	Category      string   `json:"category"`
	Priority      string   `json:"priority"`
	Status        string   `json:"status"`
	AssigneeID    int64    `json:"assigneeId,omitempty"`
	AssigneeName  string   `json:"assigneeName,omitempty"`
	RequiredSkill []int64  `json:"requiredSkills,omitempty"`
	MissingInfo   []string `json:"missingInfo,omitempty"`
	Conversation  int64    `json:"conversationId,omitempty"`
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

type skillDTO struct {
	ID       int64  `json:"id"`
	ParentID int64  `json:"parentId,omitempty"`
	Name     string `json:"name"`
}

type employeeDTO struct {
	ID            int64   `json:"id"`
	Name          string  `json:"name"`
	Active        bool    `json:"active"`
	Skills        []int64 `json:"skills"`
	CurrentLoad   int     `json:"currentLoad"`
	MaxConcurrent int     `json:"maxConcurrent"`
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

	result, err := s.conversations.Send(id, req.Content, requestID)
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

func (s *Server) handleListSkills(w http.ResponseWriter, _ *http.Request) {
	items := s.store.ListSkills()
	ret := make([]skillDTO, 0, len(items))
	for _, item := range items {
		ret = append(ret, skillDTO{ID: item.ID, ParentID: item.ParentID, Name: item.Name})
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": ret, "total": len(ret)})
}

func (s *Server) handleListEmployees(w http.ResponseWriter, _ *http.Request) {
	items := s.store.ListEmployees()
	ret := make([]employeeDTO, 0, len(items))
	for _, item := range items {
		ret = append(ret, employeeDTO{
			ID: item.ID, Name: item.Name, Active: item.Active,
			Skills:        item.Skills.IDs(),
			CurrentLoad:   len(s.store.FindOpenTicketsBySkills(item.ID)),
			MaxConcurrent: item.MaxConcurrent,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": ret, "total": len(ret)})
}

// ---- 辅助 ----

func (s *Server) toTicketDTO(t domain.Ticket) ticketDTO {
	dto := ticketDTO{
		ID:            t.ID,
		Title:         t.Title,
		Description:   t.Description,
		Category:      string(t.Category),
		Priority:      string(t.Priority),
		Status:        string(t.Status),
		AssigneeID:    t.AssigneeID,
		RequiredSkill: t.RequiredSkill.IDs(),
		MissingInfo:   t.MissingInfo,
		Conversation:  t.ConversationID,
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
