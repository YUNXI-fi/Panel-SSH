package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"panelssh/internal/ai"
	"panelssh/internal/store"
	"panelssh/internal/sysinfo"
)

const (
	metaAIBaseURL  = "ai.base_url"
	metaAIModel    = "ai.model"
	metaAIAPIKey   = "ai.api_key" // 以 secretbox 加密后落库
	metaAIMaxSteps = "ai.max_steps"
	// defaultMaxAgentSteps 是一次提问内工具调用轮数的默认上限。
	// 没有上限的话，模型可能陷入"调用—观察—再调用"的循环把成本烧光。
	// 复杂问题常需要连续取很多轮证据，默认给得宽松一些；
	// 成本敏感的场景可以在模型设置里调小。
	defaultMaxAgentSteps = 200
	// minMaxAgentSteps / maxMaxAgentSteps 是配置可接受的取值范围。
	// 下限取 1 保证至少能走一轮；上限防止误填一个天文数字把成本彻底放开。
	minMaxAgentSteps = 1
	maxMaxAgentSteps = 1000
	// confirmTimeout 是等待用户确认危险命令的时限。
	confirmTimeout = 3 * time.Minute
	// maxProcessesForModel 限制交给模型的进程条数。
	maxProcessesForModel = 30
	// 上下文预算。没有上限时，一个长时间开着的窗口会把每轮的工具输出
	// （单条可达 256 KiB）永久累积下去：请求体越来越大、token 成本无界，
	// 最终必然撞上模型的上下文上限而整段对话失效。
	maxHistoryMessages = 40
	maxHistoryBytes    = 256 << 10
	maxMessageBytes    = 32 << 10
	// compressionTimeout 是单次上下文压缩调用的时限。
	// 压缩是对话热路径上的额外请求，超时后降级为丢弃历史即可；
	// 给太长会让用户误以为面板卡死，给太短则复杂对话压不完。
	compressionTimeout = 60 * time.Second
)

// systemPrompt 是运维助手的系统提示。
//
// 重点写清三件事：先取事实再下结论、命令会被审计、危险命令需要用户确认。
// 不写这些的话，模型很容易凭经验直接给出结论，而那正是运维场景里最危险的。
const systemPrompt = `你是 Panel SSH 的服务器运维助手，通过工具在真实主机上获取信息并执行操作。

工作要求：
1. 先取证再下结论。需要事实时必须调用工具，不要凭经验猜测主机的状态、版本或配置。
2. 需要定位问题时，典型的取证顺序是：list_hosts 找到主机 → get_metrics 看整体负载 → list_processes 看谁在占用 → run_command 看具体日志或配置。
3. 命令尽量用只读命令（如 cat、ps、df、ss、journalctl）。确实需要变更时，先说明目的与影响。
4. 部分命令会被安全护栏拦截并要求用户确认；如果用户拒绝，不要重复尝试，改为给出替代方案或说明风险。
5. 回答用简体中文，简洁直接。给出结论时附上依据（关键命令输出片段），不要把原始输出整段贴回来。
6. 如果工具返回的是错误，如实说明失败原因，不要编造结果。`

type aiInMessage struct {
	Type     string `json:"type"`
	Content  string `json:"content"`
	CallID   string `json:"callId"`
	Approved bool   `json:"approved"`
}

type aiOutMessage struct {
	Type    string `json:"type"`
	Content string `json:"content,omitempty"`
	CallID  string `json:"callId,omitempty"`
	Name    string `json:"name,omitempty"`
	Args    string `json:"args,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// Ok 刻意不加 omitempty：false 表示"这次工具调用失败了"，是有效信息，
	// 省略后前端无法区分"失败"与"没有这个字段"。
	Ok      bool   `json:"ok"`
	Output  string `json:"output,omitempty"`
	Message string `json:"message,omitempty"`
}

// ---------- 配置 ----------

type aiConfigView struct {
	BaseURL   string `json:"baseUrl"`
	Model     string `json:"model"`
	HasAPIKey bool   `json:"hasApiKey"`
	MaxSteps  int    `json:"maxSteps"`
}

// loadMaxAgentSteps 读取工具调用轮数上限。
//
// 值缺失或落在合法范围之外时一律退回默认值：这个配置读错的后果是
// "助手多跑或少跑几轮"，不值得因此让整个对话不可用。
func (s *Server) loadMaxAgentSteps() int {
	raw, err := s.store.GetMeta(metaAIMaxSteps)
	if err != nil {
		return defaultMaxAgentSteps
	}
	steps, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || steps < minMaxAgentSteps || steps > maxMaxAgentSteps {
		return defaultMaxAgentSteps
	}
	return steps
}

func (s *Server) loadAIConfig() (ai.Config, error) {
	baseURL, err := s.store.GetMeta(metaAIBaseURL)
	if err != nil {
		return ai.Config{}, err
	}
	model, err := s.store.GetMeta(metaAIModel)
	if err != nil {
		return ai.Config{}, err
	}
	encrypted, err := s.store.GetMeta(metaAIAPIKey)
	if err != nil {
		return ai.Config{}, err
	}

	apiKey := ""
	if encrypted != "" {
		plain, err := s.box.Decrypt(encrypted)
		if err != nil {
			return ai.Config{}, errors.New("已保存的 API Key 无法解密（主密钥可能已更换），请重新填写")
		}
		apiKey = plain
	}

	return ai.Config{BaseURL: strings.TrimSpace(baseURL), Model: strings.TrimSpace(model), APIKey: apiKey}, nil
}

func (s *Server) handleGetAIConfig(w http.ResponseWriter, r *http.Request) {
	config, err := s.loadAIConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// API Key 只回传"是否已配置"，绝不回传明文。
	writeJSON(w, http.StatusOK, aiConfigView{
		BaseURL:   config.BaseURL,
		Model:     config.Model,
		HasAPIKey: config.APIKey != "",
		MaxSteps:  s.loadMaxAgentSteps(),
	})
}

func (s *Server) handleSetAIConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BaseURL  string `json:"baseUrl"`
		Model    string `json:"model"`
		APIKey   string `json:"apiKey"`
		MaxSteps int    `json:"maxSteps"`
	}
	if err := decodeJSONBody(w, r, &req, 1<<16); err != nil {
		return
	}

	baseURL := strings.TrimSpace(req.BaseURL)
	model := strings.TrimSpace(req.Model)
	if baseURL != "" && !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		writeErr(w, http.StatusBadRequest, "接口地址必须以 http:// 或 https:// 开头")
		return
	}
	if baseURL == "" && model == "" {
		writeErr(w, http.StatusBadRequest, "接口地址与模型名不能同时为空")
		return
	}
	if req.MaxSteps < minMaxAgentSteps || req.MaxSteps > maxMaxAgentSteps {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("工具调用轮数需在 %d–%d 之间", minMaxAgentSteps, maxMaxAgentSteps))
		return
	}

	if err := s.store.SetMeta(metaAIBaseURL, baseURL); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存接口地址失败")
		return
	}
	if err := s.store.SetMeta(metaAIModel, model); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存模型名失败")
		return
	}
	if err := s.store.SetMeta(metaAIMaxSteps, strconv.Itoa(req.MaxSteps)); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存工具调用轮数失败")
		return
	}

	// 留空表示"保持原有 Key"，因此用空串覆盖是明确不支持的操作，
	// 需要清除时请重新填写一个占位值或直接改动数据库。
	if strings.TrimSpace(req.APIKey) != "" {
		encrypted, err := s.box.Encrypt(strings.TrimSpace(req.APIKey))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "加密 API Key 失败")
			return
		}
		if err := s.store.SetMeta(metaAIAPIKey, encrypted); err != nil {
			writeErr(w, http.StatusInternalServerError, "保存 API Key 失败")
			return
		}
	}

	config, err := s.loadAIConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, aiConfigView{
		BaseURL:   config.BaseURL,
		Model:     config.Model,
		HasAPIKey: config.APIKey != "",
		MaxSteps:  s.loadMaxAgentSteps(),
	})
}

func (s *Server) handleTestAIConfig(w http.ResponseWriter, r *http.Request) {
	config, err := s.loadAIConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := ai.NewClient(config).Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "模型接口可用"})
}

// ---------- 会话 ----------

// aiSessionTitleMaxRunes 是自动生成的会话标题长度上限。
// 会话列表只有一百多像素宽，再长也只会被省略号截掉。
const aiSessionTitleMaxRunes = 32

// aiSessionLeases 保证一个会话同一时刻只被一条 WebSocket 连接改动。
//
// 会话的历史在连接建立时整份读进内存，每轮问答结束后再整份写回。两条连接
// 同时持有一个会话时，后写的一方会把对方刚产生的对话覆盖掉 —— 用户看到的
// 不是"混进了别人的消息"，而是"自己刚聊的内容凭空消失"。因此第二个连接
// 一律拒绝，并明确告诉他去关掉那个窗口。
//
// 零值可用：Server 在测试里常以字面量构造，不能让这里要求额外的初始化。
type aiSessionLeases struct {
	mu    sync.Mutex
	conns map[int64]*websocket.Conn
}

func (l *aiSessionLeases) acquire(sessionID int64, conn *websocket.Conn) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conns == nil {
		l.conns = map[int64]*websocket.Conn{}
	}
	if _, busy := l.conns[sessionID]; busy {
		return false
	}
	l.conns[sessionID] = conn
	return true
}

func (l *aiSessionLeases) release(sessionID int64, conn *websocket.Conn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// 只释放自己那一份：万一有别的连接已经接管，不能把它一起删掉。
	if l.conns[sessionID] == conn {
		delete(l.conns, sessionID)
	}
}

// close 主动断开占着某个会话的连接，用于会话被删除时。
//
// 只调 Close 回一个关闭帧，不在这里写业务消息：写操作必须走连接自己的
// wsWriter，否则会和正在推消息的 goroutine 并发写到同一条连接上。
func (l *aiSessionLeases) close(sessionID int64, reason string) {
	l.mu.Lock()
	conn := l.conns[sessionID]
	l.mu.Unlock()
	if conn != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, reason)
	}
}

// aiSessionIDFromQuery 读取请求里的会话 ID。
// 会话 ID 是隔离的唯一依据，缺失或非法时必须当场拒绝，不能退回"最近一个会话"
// —— 那等于把用户的提问发到他没在看的会话里。
func aiSessionIDFromQuery(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("session")), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// aiSessionTitle 取第一句用户提问作为会话标题。
//
// 标题只是给人扫列表用的，不需要精确：空白折叠成单个空格，超长按字符
// （而不是字节）截断，避免把一个汉字切成两半。
func aiSessionTitle(history []ai.Message) string {
	for _, msg := range history {
		if msg.Role != "user" {
			continue
		}
		title := strings.Join(strings.Fields(msg.Content), " ")
		if title == "" {
			continue
		}
		runes := []rune(title)
		if len(runes) > aiSessionTitleMaxRunes {
			return string(runes[:aiSessionTitleMaxRunes]) + "…"
		}
		return title
	}
	return ""
}

func (s *Server) handleAIListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.store.ListAISessions()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取会话列表失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

// handleAICreateSession 新建一个空会话。
//
// 不在这里做"复用最近一个空会话"之类的去重：新建动作由用户明确触发，
// 悄悄返回另一个会话会让界面上的"新对话"看起来没生效。
// 空会话由前端避免 —— 它直到用户发出第一句话才创建。
func (s *Server) handleAICreateSession(w http.ResponseWriter, r *http.Request) {
	session, err := s.store.CreateAISession("")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "创建会话失败")
		return
	}
	writeJSON(w, http.StatusOK, session)
}

func (s *Server) handleAIDeleteSession(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "会话 ID 无效")
		return
	}
	// 先断开占着它的连接再删。只删数据不断连接的话，那个窗口手里的内存
	// 历史会在下一轮写回，而会话行已经不在了，用户只会收到一条莫名其妙的
	// 保存失败；更糟的是它还能继续用一段已经不属于任何会话的上下文执行命令。
	s.aiLeases.close(id, "会话已被删除")

	if err := s.store.DeleteAISession(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "会话不存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, "删除会话失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- 对话 ----------

// loadAIConversation 取出指定会话的历史，并保证第一条是当前的系统提示。
//
// 恢复历史不只是为了界面好看：这一份同时就是喂给模型的上下文。不恢复的话，
// 用户刷新页面之后会发现模型把刚才聊过的全忘了 —— 那比界面上看不见记录
// 更让人费解，他会以为模型在装傻。
//
// 读不出来或存档损坏时退回一段全新的对话：最坏的后果是"历史没显示"，
// 不值得为此让整个助手不可用。
func (s *Server) loadAIConversation(sessionID int64) []ai.Message {
	fresh := []ai.Message{{Role: "system", Content: systemPrompt}}

	conv, err := s.store.GetAIConversation(sessionID)
	if err != nil || strings.TrimSpace(conv.HistoryJSON) == "" {
		return fresh
	}
	var restored []ai.Message
	if err := json.Unmarshal([]byte(conv.HistoryJSON), &restored); err != nil {
		return fresh
	}

	// 系统提示始终以当前代码为准，不用存档里的那一份：面板升级后提示词
	// 可能已经改了，继续用旧的会让新写的要求完全不起作用。
	history := fresh
	for _, msg := range restored {
		if msg.Role != "system" {
			history = append(history, msg)
		}
	}
	return sanitizeHistory(history)
}

// sanitizeHistory 清洗结构上非法的历史，使其能够再次发给模型。
//
// 需要清洗是因为更早版本的裁剪逻辑会切出孤立 tool（详见 trimHistory 注释），
// 而坏历史一旦落库就会被反复读回：用户会陷入"每次提问都 400、重试也没用、
// 只能清空对话才能脱身"的困境，而清空这个动作他根本无从知晓。
//
// 这里丢弃三类消息：
//   - 前面没有带 tool_calls 的 assistant 的孤立 tool；
//   - 连续重复的 user（更早的裁剪/并发写入可能留下）；
//   - 不带任何内容、也不带 tool_calls 的 assistant 空壳。
//
// 只做减法会让历史变短，不会让它更坏；最坏情形退化成"少了几句上下文"。
func sanitizeHistory(history []ai.Message) []ai.Message {
	if len(history) == 0 {
		return history
	}
	out := make([]ai.Message, 0, len(history))
	out = append(out, history[0]) // system

	// pendingTools 记录上一条 assistant 带来的、尚未收到结果的 tool_call ID。
	// 只有它非空时才允许出现 tool 消息，这就保证了配对的完整性。
	var pendingTools map[string]bool
	lastRole := ""

	for _, msg := range history[1:] {
		switch msg.Role {
		case "system":
			// 恢复时已经统一换成当前系统提示，这里再出现的直接忽略。
			continue
		case "user":
			// 连续的空 user 没有信息量，丢弃；有内容的相邻 user 协议上允许，
			// 保留（我们的流程不会产生，但没必要因为可疑就把用户的话删掉）。
			if lastRole == "user" && strings.TrimSpace(msg.Content) == "" {
				continue
			}
			pendingTools = nil
		case "assistant":
			pendingTools = nil
			for _, call := range msg.ToolCalls {
				if pendingTools == nil {
					pendingTools = map[string]bool{}
				}
				pendingTools[call.ID] = true
			}
			if len(msg.ToolCalls) == 0 && strings.TrimSpace(msg.Content) == "" {
				continue // 无内容也无工具调用的空壳
			}
		case "tool":
			if pendingTools == nil || !pendingTools[msg.ToolCallID] {
				continue // 孤立 tool：正是导致 400 的元凶
			}
			delete(pendingTools, msg.ToolCallID)
			// tool 与它所属的 assistant 之间不再有其它配对，lastRole 在
			// 下面统一更新即可。
		default:
			continue // 未知角色一律不发，避免再触发协议错误
		}

		out = append(out, msg)
		lastRole = msg.Role
	}
	return out
}

// persistAIConversation 把对话历史写回**它所属的那个会话**。
//
// 失败必须说出来：用户下次打开窗口发现记录没了，却不知道是因为压根没保存，
// 只会以为面板把他的话弄丢了。宁可当场给一条提示。
//
// sessionID 由连接建立时固定下来并一路传到这里，绝不在落库时重新判断
// "当前是哪个会话"：用户完全可能在一轮问答结束前就切走了，那一刻的
// "当前会话"已经不是这段对话的归属了。
func (s *Server) persistAIConversation(
	ctx context.Context,
	out *wsWriter,
	sessionID int64,
	history []ai.Message,
	toolStatus map[string]bool,
) {
	historyJSON, err := json.Marshal(history)
	if err != nil {
		_ = out.write(ctx, aiOutMessage{Type: "error", Message: "对话记录序列化失败，本次对话没能保存：" + err.Error()})
		return
	}
	statusJSON, err := json.Marshal(toolStatus)
	if err != nil {
		// map[string]bool 序列化不了只可能是编码器出了问题。退回空表 ——
		// 丢掉工具的成功/失败标记，总好过整段对话都存不下。
		statusJSON = []byte("{}")
	}
	if err := s.store.SaveAIConversation(sessionID, string(historyJSON), string(statusJSON), aiSessionTitle(history)); err != nil {
		message := "对话记录写入失败，重开窗口后看不到这次对话：" + err.Error()
		if errors.Is(err, store.ErrNotFound) {
			message = "这个会话已被删除，本次对话没有保存"
		}
		_ = out.write(ctx, aiOutMessage{Type: "error", Message: message})
	}
}

// handleAIGetHistory 返回某个会话已保存的对话记录，供前端重建消息流。
//
// 单独开一个 HTTP 接口而不是复用 WebSocket：窗口一打开就要显示历史，
// 而 WS 是在用户发出第一条消息时才连的，那时再推已经太晚。
//
// 会话必须由调用方显式给出：切换会话时前端拉的是另一份记录，接口若默认
// 返回"最近一个会话"，切过去的第一屏就会显示错内容。
func (s *Server) handleAIGetHistory(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := aiSessionIDFromQuery(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "缺少有效的会话 ID")
		return
	}
	conv, err := s.store.GetAIConversation(sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "会话不存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, "读取对话记录失败")
		return
	}

	// 存档内容直接作为 JSON 原样下发，前端拿到的就是结构化的历史而不是
	// 一个需要再解析的字符串。先过一遍 json.Valid：损坏的内容会让整个响应
	// 编码失败，而那能造成的最坏后果也只是"看不到历史"，不值得让接口报错。
	history := json.RawMessage("[]")
	if raw := strings.TrimSpace(conv.HistoryJSON); raw != "" && json.Valid([]byte(raw)) {
		history = json.RawMessage(raw)
	}
	toolStatus := json.RawMessage("{}")
	if raw := strings.TrimSpace(conv.ToolStatusJSON); raw != "" && json.Valid([]byte(raw)) {
		toolStatus = json.RawMessage(raw)
	}

	writeJSON(w, http.StatusOK, map[string]any{"history": history, "toolStatus": toolStatus})
}

// handleAIClearHistory 清空指定会话的对话记录。
//
// 清掉的不只是界面上看到的东西，也包括模型下次会读到的上下文 —— 它们是
// 同一份数据。前端清空后会断开 WebSocket，让下次提问重新建立连接，
// 这样服务端手里的那份内存历史也一并作废。
//
// 只动这一个会话：其他会话的记录与上下文完全不受影响。
func (s *Server) handleAIClearHistory(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := aiSessionIDFromQuery(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "缺少有效的会话 ID")
		return
	}
	if err := s.store.ClearAIConversation(sessionID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "会话不存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, "清空对话记录失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAIChat 承载 AI 助手的 WebSocket 会话。
//
// 对话历史保存在服务端，客户端只发送新的用户消息：工具调用会在历史里留下
// tool 角色的消息，由客户端自行拼装既容易出错也容易被篡改。
//
// 历史在每次建立连接时从库里恢复、每轮问答结束后落库，因此它跨页面刷新、
// 跨面板重启都还在，也不会因为换了张网页面板就把模型聊失忆。
//
// 注意这里的"每轮"是用户的每一次提问，而不是每次工具调用：一次提问可能
// 包含多轮工具调用，它们由 runAgent 一口气跑完再统一落库。
//
// 一条连接只属于一个会话（会话 ID 从查询参数读入），前端切换会话时断开
// 重连。这是多会话隔离的关键：内存里的 history、待确认的危险命令、工具
// 调用状态都挂在连接上，不重新建连就不可能把上个会话的东西带过来。
func (s *Server) handleAIChat(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := aiSessionIDFromQuery(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "缺少有效的会话 ID")
		return
	}
	if _, err := s.store.GetAISession(sessionID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "会话不存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, "读取会话失败")
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(1 << 20)
	defer conn.CloseNow()

	// 登记到活跃连接表：面板关闭时要主动断开它，而不是硬切。
	if !s.registerSocket(conn) {
		_ = conn.Close(websocket.StatusGoingAway, "服务正在关闭")
		return
	}
	defer s.unregisterSocket(conn)

	ctx := r.Context()
	out := &wsWriter{conn: conn}

	// 一个会话同时只能有一条连接在改它，否则两边各持一份内存历史、
	// 各自整份写回，后写的一方会把对方的对话抹掉。
	if !s.aiLeases.acquire(sessionID, conn) {
		_ = out.write(ctx, aiOutMessage{
			Type:    "error",
			Message: "这个会话已在另一个窗口打开，请先关闭那个窗口再试",
		})
		_ = conn.Close(websocket.StatusPolicyViolation, "session in use")
		return
	}
	defer s.aiLeases.release(sessionID, conn)

	// 等正在落库的那一轮真正结束，再释放会话占用。
	// defer 是后进先出：它排在 release 之后注册，因此会先执行 ——
	// 否则新连接可能在旧连接写回之前就读库，读到一份旧历史。
	var inflight sync.WaitGroup
	defer inflight.Wait()

	config, err := s.loadAIConfig()
	if err != nil {
		_ = out.write(ctx, aiOutMessage{Type: "error", Message: err.Error()})
		return
	}
	client := ai.NewClient(config)

	var (
		stateMu   sync.Mutex
		running   bool
		cancel    context.CancelFunc
		confirmMu sync.Mutex
		pending   = map[string]chan bool{}
	)

	history := s.loadAIConversation(sessionID)

	for {
		var msg aiInMessage
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			// 客户端断开时必须显式取消在途任务。
			//
			// WebSocket 是从 HTTP 连接 Hijack 出来的，r.Context() 不会因对端断开而取消；
			// 不主动停的话，agent 会把整条工具调用链跑完 —— 其中可能包含在目标机上执行命令。
			stateMu.Lock()
			stop := cancel
			stateMu.Unlock()
			if stop != nil {
				stop()
			}
			return
		}

		switch msg.Type {
		case "confirmResult":
			confirmMu.Lock()
			waiter, ok := pending[msg.CallID]
			if ok {
				delete(pending, msg.CallID)
			}
			confirmMu.Unlock()
			if ok {
				waiter <- msg.Approved
			}

		case "abort":
			stateMu.Lock()
			stop := cancel
			stateMu.Unlock()
			if stop != nil {
				stop()
			}

		case "chat":
			content := strings.TrimSpace(msg.Content)
			if content == "" {
				continue
			}

			stateMu.Lock()
			if running {
				stateMu.Unlock()
				_ = out.write(ctx, aiOutMessage{Type: "error", Message: "上一条消息还在处理中，请稍候或先点击停止"})
				continue
			}
			running = true
			runCtx, stop := context.WithCancel(ctx)
			cancel = stop
			stateMu.Unlock()

			inflight.Add(1)
			go func() {
				defer func() {
					stop()
					stateMu.Lock()
					running = false
					cancel = nil
					stateMu.Unlock()
					inflight.Done()
				}()
				// toolStatus 在这一轮里逐次累积，最后随历史一起落库。
				toolStatus := map[string]bool{}
				s.runAgent(runCtx, out, client, &history, content, pending, &confirmMu, toolStatus)
				// 不管这一轮是怎么结束的 —— 答完了、报错了、被用户停掉 ——
				// 已经产生的对话都该留下来。
				s.persistAIConversation(ctx, out, sessionID, history, toolStatus)
			}()
		}
	}
}

func (s *Server) runAgent(
	ctx context.Context,
	out *wsWriter,
	client *ai.Client,
	history *[]ai.Message,
	userInput string,
	pending map[string]chan bool,
	confirmMu *sync.Mutex,
	toolStatus map[string]bool,
) {
	*history = append(*history, ai.Message{Role: "user", Content: userInput})

	// 本次提问开始时的上限。与模型配置一样在每轮里重读没有必要：
	// 中途改上限只影响"下一次提问"，不会让已经在跑的这轮突然变长或变短。
	maxSteps := s.loadMaxAgentSteps()

	for step := 0; step < maxSteps; step++ {
		// 每轮发出请求前裁剪上下文，把历史控制在预算内。
		//
		// 裁剪分两步：先试着把最老的轮次压成摘要（不失忆，但要花一次模型
		// 调用），压缩不成功或不需要时再退回"丢整轮"。压成摘要之后历史
		// 通常已经落回预算内，trimHistory 便不再动手。
		if compacted, ok := s.compactHistory(ctx, client, *history); ok {
			*history = compacted
		}
		*history = trimHistory(*history)

		// 每轮重新读取模型配置：管理员在面板里改了接口地址或模型名之后，
		// 已经打开的 AI 窗口也应当立刻用上新配置，而不是等到重开窗口才生效。
		// 复用同一个 Client（只换配置）以保住 HTTP 连接池。
		if fresh, err := s.loadAIConfig(); err == nil {
			client.UpdateConfig(fresh)
		}

		reply, err := client.Chat(ctx, *history, toolDefinitions())
		if err != nil {
			if ctx.Err() != nil {
				_ = out.write(ctx, aiOutMessage{Type: "error", Message: "已停止本次处理"})
			} else {
				_ = out.write(ctx, aiOutMessage{Type: "error", Message: err.Error()})
			}
			_ = out.write(ctx, aiOutMessage{Type: "done"})
			return
		}

		*history = append(*history, *reply)

		// 模型可以边说明边调用工具，说明文字要先显示出来。
		if strings.TrimSpace(reply.Content) != "" {
			_ = out.write(ctx, aiOutMessage{Type: "assistant", Content: reply.Content})
		}

		if len(reply.ToolCalls) == 0 {
			_ = out.write(ctx, aiOutMessage{Type: "done"})
			return
		}

		for _, call := range reply.ToolCalls {
			result, ok := s.runToolCall(ctx, out, call, pending, confirmMu)
			toolStatus[call.ID] = ok
			*history = append(*history, ai.Message{
				Role:       "tool",
				ToolCallID: call.ID,
				Content:    result,
			})
		}
	}

	_ = out.write(ctx, aiOutMessage{
		Type:    "error",
		Message: fmt.Sprintf("已连续调用工具 %d 次仍未得出结论，先停在这里。建议把问题拆得更具体一些再问。", maxSteps),
	})
	_ = out.write(ctx, aiOutMessage{Type: "done"})
}

// runToolCall 执行一次工具调用。
//
// 无论成功失败都必须返回一段文本作为工具结果 —— 否则模型会一直等在那里，
// 拿不到结果的 tool_calls 会让整个对话卡死。
//
// 第二个返回值是这次调用是否成功。它不只是给调用方看的：界面回看历史时
// 必须能分清一条命令是成功了还是失败了，而这个信息不在 ai.Message 里
// （那里只有 role 和 content），因此要在落库时另外记下来。
func (s *Server) runToolCall(
	ctx context.Context,
	out *wsWriter,
	call ai.ToolCall,
	pending map[string]chan bool,
	confirmMu *sync.Mutex,
) (string, bool) {
	var args toolArguments
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		output := "参数无法解析：" + err.Error()
		_ = out.write(ctx, aiOutMessage{Type: "toolResult", CallID: call.ID, Ok: false, Output: output})
		return output, false
	}

	_ = out.write(ctx, aiOutMessage{
		Type:   "tool",
		CallID: call.ID,
		Name:   call.Function.Name,
		Args:   call.Function.Arguments,
	})

	// 护栏：危险命令先经用户确认。确认发生在界面上，而不是由模型自行决定。
	if call.Function.Name == "run_command" {
		if reason, dangerous := ai.CheckCommand(args.Command); dangerous {
			_ = out.write(ctx, aiOutMessage{
				Type:   "confirm",
				CallID: call.ID,
				Name:   call.Function.Name,
				Args:   args.Command,
				Reason: reason,
			})

			approved, expired := waitForConfirm(ctx, call.ID, pending, confirmMu)
			if expired {
				// 超时之后用户再点"允许"已经没有人接收，必须把这个事实告诉界面，
				// 否则按钮看起来还能点、点下去却什么都不发生。
				_ = out.write(ctx, aiOutMessage{
					Type:    "confirmExpired",
					CallID:  call.ID,
					Message: "确认已超时，本次命令未执行。请重新发起一次请求。",
				})
			}
			if !approved {
				output := "用户拒绝执行该命令。请不要重试，改为说明它的风险并给出替代方案。"
				_ = out.write(ctx, aiOutMessage{Type: "toolResult", CallID: call.ID, Ok: false, Output: output})
				return output, false
			}
		}
	}

	output, err := s.executeTool(ctx, call.Function.Name, args)
	if err != nil {
		text := "执行失败：" + err.Error()
		_ = out.write(ctx, aiOutMessage{Type: "toolResult", CallID: call.ID, Ok: false, Output: text})
		return text, false
	}

	_ = out.write(ctx, aiOutMessage{Type: "toolResult", CallID: call.ID, Ok: true, Output: output})
	return output, true
}

// waitForConfirm 等待用户对危险命令的确认。
// 返回 (是否批准, 是否因超时而过期)。两者都会走到这里，但对界面含义不同：
// 拒绝是用户的选择，过期意味着用户再来点也已经没人接收了。
func waitForConfirm(ctx context.Context, callID string, pending map[string]chan bool, confirmMu *sync.Mutex) (approved bool, expired bool) {
	waiter := make(chan bool, 1)

	confirmMu.Lock()
	pending[callID] = waiter
	confirmMu.Unlock()

	release := func() {
		confirmMu.Lock()
		delete(pending, callID)
		confirmMu.Unlock()
	}

	select {
	case approved := <-waiter:
		return approved, false
	case <-time.After(confirmTimeout):
		release()
		return false, true
	case <-ctx.Done():
		release()
		return false, false
	}
}

// splitRounds 把 history[1:]（system 之后的部分）切成若干"轮"。
//
// 一轮从一条 user 开始，一直延伸到下一条 user 之前 —— 也就是"一次提问
// 加上它引发的全部 assistant/tool 往返"。之所以要坚持这个边界，是因为
// assistant(tool_calls) 与紧随其后的 tool 结果是一组不可分割的配对，
// 从中间切开会让服务端直接拒绝请求。
//
// 开头若有不属于任何 user 的消息（例如更早一次裁剪留下的孤立 tool），
// 它们单独成组放在最前面；调用方负责丢弃它们。
func splitRounds(rest []ai.Message) [][]ai.Message {
	var rounds [][]ai.Message
	for _, msg := range rest {
		if len(rounds) == 0 || msg.Role == "user" {
			rounds = append(rounds, []ai.Message{msg})
			continue
		}
		rounds[len(rounds)-1] = append(rounds[len(rounds)-1], msg)
	}
	return rounds
}

// truncateOversized 把单条过大的消息（工具输出是主要来源）就地截断。
//
// 这一步必须独立出来并被 trimHistory 与 compactHistory 共用：两者都要
// 判断"历史还超不超预算"，如果一方按截断前的体积算、另一方按截断后的
// 体积算，结论就会不一致 —— 压缩会把本已落回预算的历史又算成超预算，
// 压掉一批轮次之后仍然超，紧接着被 trimHistory 再丢一遍，压缩白做。
//
// 摘要消息不截断：它本身就是被压缩过的结果，再切会丢要点。
func truncateOversized(history []ai.Message) []ai.Message {
	if len(history) == 0 {
		return history
	}
	out := make([]ai.Message, 0, len(history))
	out = append(out, history[0])
	for _, msg := range history[1:] {
		if !isSummary(msg) && len(msg.Content) > maxMessageBytes {
			msg.Content = msg.Content[:maxMessageBytes] + "\n…（历史内容过长，已截断）"
		}
		out = append(out, msg)
	}
	return out
}

// trimHistory 把对话历史裁剪到上下文预算内。
//
// 关键约束：role=tool 的消息必须紧跟在带 tool_calls 的 assistant 消息之后。
// 因此裁剪的最小单元必须是"完整的一轮"（一个 user 及其后所有往返），
// 拆散配对会让服务端直接拒绝请求 —— 表现为"聊到一定长度后突然一直报错"。
//
// 早先的实现按条数取窗口后再向后找 user，窗口整段落在工具链里时会找不到
// 任何 user，兜底退化成"只保留最后一条消息"——而那往往正是一条 tool，
// 于是切出孤立 tool。一次提问里工具调用越多（maxSteps 调大后尤其明显），
// 这个兜底越容易被踩到，坏历史一旦落库，后续每次请求都会 400。
func trimHistory(history []ai.Message) []ai.Message {
	if len(history) == 0 {
		return history
	}

	// 压缩产生的摘要消息是"背景"，不属于任何一轮：这里先把它们摘出来，
	// 轮次裁剪只处理真实的对话，最后再把摘要原样放回 system 之后。
	// 不摘出来会被下面"丢弃前导非 user"那一步当成脏数据删掉。
	var summaries []ai.Message
	for _, msg := range history[1:] {
		if isSummary(msg) {
			summaries = append(summaries, msg)
		}
	}

	// 第一轮：单条过大的内容（工具输出是主要来源）就地截断。
	// 先做这一步，后面的字节预算才是基于裁剪后的真实体积判断的。
	history = truncateOversized(history)

	// 第二轮：丢弃不属于任何一轮的前导消息（孤立 tool 等）。
	// 它们只可能来自更早版本写坏的历史，留着就是必然的 400。
	start := 1
	for start < len(history) && history[start].Role != "user" {
		start++
	}
	if start >= len(history) {
		// 系统提示之后没有任何 user：只保留系统提示与已有的摘要，
		// 而不是把孤立消息发出去。
		out := []ai.Message{history[0]}
		return append(out, summaries...)
	}
	history = append([]ai.Message{history[0]}, history[start:]...)

	// 第三轮：按条数与字节预算裁剪，逐轮从最老的开始丢弃。
	// 只丢整轮、且永远保留 system 与最后一轮，因此不可能切出孤立 tool。
	//
	// 代价是：若最后一轮自身的条数或体积就超预算（一次提问里工具调用极多），
	// 结果仍会超出预算。这是刻意的取舍 —— 超出预算只是请求变大，
	// 切出孤立 tool 则是请求必然被拒。
	rounds := splitRounds(history[1:])
	total := 1 // system
	bytes := len(history[0].Content)
	for _, msg := range summaries {
		bytes += len(msg.Content)
	}
	for _, round := range rounds {
		total += len(round)
		for _, msg := range round {
			bytes += len(msg.Content)
		}
	}
	for len(rounds) > 1 && (total > maxHistoryMessages || bytes > maxHistoryBytes) {
		dropped := rounds[0]
		rounds = rounds[1:]
		total -= len(dropped)
		for _, msg := range dropped {
			bytes -= len(msg.Content)
		}
	}

	out := make([]ai.Message, 0, total+len(summaries))
	out = append(out, history[0])
	out = append(out, summaries...)
	for _, round := range rounds {
		out = append(out, round...)
	}
	return out
}

func totalBytes(history []ai.Message) int {
	total := 0
	for _, msg := range history {
		total += len(msg.Content)
	}
	return total
}

// ---------- 上下文压缩 ----------

// summaryPrefix 标在摘要消息的最前面，用来把"压缩产生的摘要"和"用户自己
// 说的话"区分开。
//
// 摘要以 system 消息的身份进入历史，而不是 assistant：后者会让模型以为
// 那是自己说过的话，进而把摘要当成既定结论复述给用户；user 则会被界面
// 渲染成用户发的消息。system 既不会显示在对话流里，也能被模型正确理解为
// 背景信息。
const summaryPrefix = "【历史摘要】以下是更早的对话被压缩后的要点，供你回忆上下文；" +
	"其中未验证的推测不要当作事实：\n\n"

// summarySystemPrompt 是让模型压缩对话时用的系统提示。
const summarySystemPrompt = `你是对话压缩器。把用户提供的多轮运维对话压缩成一段要点，供后续对话快速恢复上下文。

要求：
1. 只保留对后续对话有用的信息：用户的目标、已确认的结论、已执行的关键操作及其结果、尚未解决的问题。
2. 明确区分「已确认的事实」与「模型的推测」。推测要标注为不确定。
3. 已经被后续对话推翻的旧结论不要再写，或明确说明它已被推翻。
4. 不要保留工具输出的原始内容，只留结论（例如"磁盘 / 使用率 92%"而不是整个 df 输出）。
5. 用简体中文，条目式，尽量简短。不要写客套话。`

// compactPlan 描述一次压缩该怎么做：把哪些轮次压成摘要、保留哪些。
type compactPlan struct {
	// toSummarize 是被压缩掉的轮次，按时间顺序排列。
	toSummarize [][]ai.Message
	// kept 是压缩后保留的轮次。
	kept [][]ai.Message
}

// planCompaction 判断历史是否超预算，并给出压缩方案。
//
// 压缩的思路与 trimHistory 相反：trimHistory 是"丢掉最老的整轮"，
// 这里则是在丢之前，先把要丢的那部分交给模型压成摘要。只有当历史确实
// 超预算时才需要压缩，因此通常什么都不做 —— 压缩要花一次额外的模型调用，
// 不能在每轮对话里都付这个成本。
//
// 返回的 ok 为 false 表示不需要压缩；此时调用方应当直接走 trimHistory。
func planCompaction(history []ai.Message) (compactPlan, bool) {
	if len(history) < 2 {
		return compactPlan{}, false
	}

	// 第二段（system 之后的部分）之外的摘要消息本身不计入"轮次"，
	// 它们会在压缩后重新拼接，这里先剔除。
	rounds := splitRounds(nonSummary(history[1:]))
	total := 1
	bytes := len(history[0].Content)
	for _, round := range rounds {
		total += len(round)
		for _, msg := range round {
			bytes += len(msg.Content)
		}
	}
	if total <= maxHistoryMessages && bytes <= maxHistoryBytes {
		return compactPlan{}, false
	}

	// 从最老的轮次开始划入"待压缩"，直到剩下的部分落回预算内。
	// 与 trimHistory 一样，永远保留最后一轮：那是用户当前的问题。
	// 同时要求至少要压掉一轮，否则压缩没有意义。
	keepFrom := 0
	for keepFrom < len(rounds)-1 && (total > maxHistoryMessages || bytes > maxHistoryBytes) {
		for _, msg := range rounds[keepFrom] {
			total--
			bytes -= len(msg.Content)
		}
		keepFrom++
	}
	if keepFrom == 0 {
		return compactPlan{}, false
	}

	plan := compactPlan{kept: rounds[keepFrom:]}
	plan.toSummarize = append(plan.toSummarize, rounds[:keepFrom]...)
	return plan, true
}

// nonSummary 过滤掉历史里的摘要消息，只留真实的对话轮次。
//
// 摘要消息和系统提示一样是"背景"，不属于任何一轮；不剔出去的话，
// splitRounds 会把它当成一轮的开头，下一轮的真实内容会被并进摘要那一组。
func nonSummary(messages []ai.Message) []ai.Message {
	out := make([]ai.Message, 0, len(messages))
	for _, msg := range messages {
		if isSummary(msg) {
			continue
		}
		out = append(out, msg)
	}
	return out
}

// isSummary 判断一条消息是不是压缩产生的摘要。
func isSummary(msg ai.Message) bool {
	return msg.Role == "system" && strings.HasPrefix(msg.Content, summaryPrefix)
}

// compactHistory 在历史超预算时，把最老的若干轮压成一段摘要。
//
// 返回值与 ok：ok 为 true 表示历史已经被替换成"压缩后"的版本，调用方
// 应当直接使用它；ok 为 false 表示这次没做任何事（不需要压缩，或者压缩
// 失败了），调用方应当继续走 trimHistory 的丢弃逻辑。
//
// 压缩失败一律降级为"不压缩"，绝不向上报错：压缩只是优化，失败的最坏
// 后果是回到改动前的行为（丢上下文），而让一次网络抖动打断用户正在进行的
// 对话，代价要大得多。
//
// 是否真的压缩由 planCompaction 判断：只有超预算时才动，平时不花这次调用。
func (s *Server) compactHistory(ctx context.Context, client *ai.Client, history []ai.Message) ([]ai.Message, bool) {
	// 先做与 trimHistory 相同的单条截断，让"超没超预算"这个判断基于同一个
	// 体积口径；否则会按虚高的体积压掉本不必压的轮次（见 truncateOversized）。
	history = truncateOversized(history)

	plan, ok := planCompaction(history)
	if !ok {
		return history, false
	}

	// 已有摘要要一并带上：新摘要必须覆盖旧摘要的内容，否则每次压缩都会
	// 丢掉上一次压出来的要点 —— 那些要点指向的原文早已不在历史里了。
	var previous string
	for _, msg := range history[1:] {
		if isSummary(msg) {
			previous = msg.Content
			break
		}
	}

	transcript := renderRoundsForSummary(plan.toSummarize)
	if previous != "" {
		transcript = "已有摘要（请在此基础上合并，不要丢弃仍然有效的要点）：\n" +
			strings.TrimPrefix(previous, summaryPrefix) + "\n\n新增对话：\n" + transcript
	}

	// 摘要请求给一个自己的超时：模型卡住时不能把用户这轮对话一起拖住。
	summaryCtx, cancel := context.WithTimeout(ctx, compressionTimeout)
	defer cancel()

	reply, err := client.Chat(summaryCtx, []ai.Message{
		{Role: "system", Content: summarySystemPrompt},
		{Role: "user", Content: transcript},
	}, nil)
	if err != nil {
		return history, false // 降级：交给 trimHistory 丢弃
	}
	summary := strings.TrimSpace(reply.Content)
	if summary == "" {
		return history, false
	}

	out := make([]ai.Message, 0, 2+len(plan.kept))
	out = append(out, history[0])
	out = append(out, ai.Message{Role: "system", Content: summaryPrefix + summary})
	for _, round := range plan.kept {
		out = append(out, round...)
	}
	return out, true
}

// renderRoundsForSummary 把待压缩的轮次渲染成一段纯文本交给模型。
//
// 这里刻意不保留 tool_calls 的结构：压缩要的是"发生了什么"，
// 而不是可以被回放的调用记录；把 JSON 结构塞进去只会挤占摘要的篇幅。
// 工具输出统一截断，避免一次压缩请求本身就超长。
func renderRoundsForSummary(rounds [][]ai.Message) string {
	const maxToolOutput = 2000
	var b strings.Builder
	for _, round := range rounds {
		for _, msg := range round {
			switch msg.Role {
			case "user":
				b.WriteString("用户：")
				b.WriteString(msg.Content)
				b.WriteString("\n")
			case "assistant":
				if text := strings.TrimSpace(msg.Content); text != "" {
					b.WriteString("助手：")
					b.WriteString(text)
					b.WriteString("\n")
				}
				for _, call := range msg.ToolCalls {
					b.WriteString("（调用工具 ")
					b.WriteString(call.Function.Name)
					b.WriteString(" 参数 ")
					b.WriteString(clipText(call.Function.Arguments, 500))
					b.WriteString("）\n")
				}
			case "tool":
				b.WriteString("工具结果：")
				b.WriteString(clipText(msg.Content, maxToolOutput))
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// clipText 把文本截到 limit 字节以内，超出时留下省略标记。
//
// 按字节截断在多字节字符的中间切断会产生非法 UTF-8，JSON 编码时会被
// 替换成 U+FFFD；这里退到最后一个完整字符边界，避免摘要请求里出现乱码。
func clipText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}

// ---------- 工具层 ----------

type toolArguments struct {
	HostID     int64  `json:"hostId"`
	Command    string `json:"command"`
	TimeoutSec int    `json:"timeoutSec"`
}

func toolDefinitions() []ai.Tool {
	hostIDProperty := map[string]any{
		"type":        "integer",
		"description": "主机 ID，来自 list_hosts 的返回结果",
	}

	return []ai.Tool{
		ai.NewTool(
			"list_hosts",
			"列出面板已纳管的主机（含 ID、地址、用户名、分组）。任何需要操作主机的任务都应先调用它拿到主机 ID。",
			map[string]any{"type": "object", "properties": map[string]any{}},
		),
		ai.NewTool(
			"run_command",
			"在指定主机上执行一条 shell 命令并返回退出码、stdout、stderr。用于读取配置、查看日志、检查服务状态等。",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"hostId":  hostIDProperty,
					"command": map[string]any{"type": "string", "description": "要执行的 shell 命令"},
					"timeoutSec": map[string]any{
						"type":        "integer",
						"description": "超时秒数，默认 60，最大 600",
					},
				},
				"required": []string{"hostId", "command"},
			},
		),
		ai.NewTool(
			"get_metrics",
			"获取指定主机的运行指标：CPU 使用率、负载、内存、交换分区、各挂载点容量、网络收发速率。",
			map[string]any{
				"type":       "object",
				"properties": map[string]any{"hostId": hostIDProperty},
				"required":   []string{"hostId"},
			},
		),
		ai.NewTool(
			"list_processes",
			"获取指定主机上 CPU 占用最高的进程列表（含 PID、用户、CPU%、内存%、命令行）。",
			map[string]any{
				"type":       "object",
				"properties": map[string]any{"hostId": hostIDProperty},
				"required":   []string{"hostId"},
			},
		),
	}
}

func (s *Server) executeTool(ctx context.Context, name string, args toolArguments) (string, error) {
	switch name {
	case "list_hosts":
		hosts, err := s.store.ListHosts()
		if err != nil {
			return "", err
		}
		items := make([]map[string]any, 0, len(hosts))
		for _, host := range hosts {
			items = append(items, map[string]any{
				"id":       host.ID,
				"name":     host.Name,
				"addr":     host.Addr,
				"port":     host.Port,
				"username": host.Username,
				"group":    host.Group,
				"remark":   host.Remark,
			})
		}
		return toJSON(items)

	case "run_command":
		if strings.TrimSpace(args.Command) == "" {
			return "", errors.New("command 不能为空")
		}
		result, err := s.execOnHost(ctx, args.HostID, args.Command, clampExecTimeout(args.TimeoutSec))
		if err != nil {
			return "", err
		}
		return toJSON(map[string]any{
			"exitCode":  result.ExitCode,
			"stdout":    result.Stdout,
			"stderr":    result.Stderr,
			"truncated": result.StdoutClipped || result.StderrClipped,
		})

	case "get_metrics":
		// 与监控窗口共用同一个采样器：AI 提问期间正好开着监控页时，
		// 两边不会再各自去目标机跑一遍采集。走的是同一个入口，
		// 因此这次采样也会进历史缓冲，不会让曲线平白多出一个洞。
		metrics, err := s.sampleMetrics(args.HostID, false)
		if err != nil {
			return "", err
		}
		return toJSON(metrics)

	case "list_processes":
		snapshot, err := s.procs.get(args.HostID, func() (processSnapshot, error) {
			raw, err := s.runOnHost(args.HostID, sysinfo.ProcessesCommand())
			if err != nil {
				return processSnapshot{}, err
			}
			processes, total := sysinfo.ParseProcesses(raw)
			return processSnapshot{Processes: processes, Total: total}, nil
		})
		if err != nil {
			return "", err
		}
		processes := snapshot.Processes
		// 只把占用最高的若干条交给模型：完整进程表会挤爆上下文，
		// 而对"谁在吃资源"这类问题而言头部的信息量才是有效的。
		if len(processes) > maxProcessesForModel {
			processes = processes[:maxProcessesForModel]
		}
		return toJSON(map[string]any{"total": snapshot.Total, "top": processes})

	default:
		return "", fmt.Errorf("未知工具：%s", name)
	}
}

func toJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
