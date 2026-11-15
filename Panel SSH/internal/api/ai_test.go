package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"panelssh/internal/ai"
	"panelssh/internal/config"
	"panelssh/internal/store"
)

// TestLoadAIConversation 验证历史恢复。
//
// 恢复出来的这一份同时喂给模型和界面，因此它有两层要求：内容要接得上
// （模型不失忆），第一条必须是**当前的**系统提示（面板升级后新写的提示词
// 要立刻生效，而不是继续沿用存档里那份旧的）。
func TestLoadAIConversation(t *testing.T) {
	st := newTestStore(t)
	s := &Server{store: st}

	session, err := st.CreateAISession("")
	if err != nil {
		t.Fatalf("创建会话失败：%v", err)
	}

	history := s.loadAIConversation(session.ID)
	if len(history) != 1 || history[0].Role != "system" || history[0].Content != systemPrompt {
		t.Fatalf("空库应当只返回系统提示：%+v", history)
	}

	stale := `[
		{"role":"system","content":"过时的提示词"},
		{"role":"user","content":"你好"},
		{"role":"assistant","content":"在"}
	]`
	if err := st.SaveAIConversation(session.ID, stale, "{}", ""); err != nil {
		t.Fatalf("写入失败：%v", err)
	}

	history = s.loadAIConversation(session.ID)
	if len(history) != 3 {
		t.Fatalf("应当恢复出 1 条系统提示 + 2 条对话：%+v", history)
	}
	if history[0].Content != systemPrompt {
		t.Fatalf("第一条必须是当前的系统提示：%q", history[0].Content)
	}
	for _, msg := range history {
		if msg.Content == "过时的提示词" {
			t.Fatal("过时的系统提示应当丢掉，否则面板升级后新写的提示词完全不生效")
		}
	}
	if history[1].Role != "user" || history[1].Content != "你好" {
		t.Fatalf("用户消息没有正确恢复：%+v", history[1])
	}

	// 存档损坏时退回一段新对话，而不是让助手整个不可用。
	if err := st.SaveAIConversation(session.ID, "{不是 JSON", "{}", ""); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	history = s.loadAIConversation(session.ID)
	if len(history) != 1 || history[0].Role != "system" {
		t.Fatalf("损坏的存档应当退回新对话：%+v", history)
	}
}

// TestAIGetHistoryToleratesCorruptArchive 验证存档损坏时接口仍然可用。
//
// 存档坏掉能造成的最坏后果只是"看不到历史"；如果为此让接口报错，用户看到的
// 是助手窗口一打开就弹错 —— 那比丢历史严重得多。
func TestAIGetHistoryToleratesCorruptArchive(t *testing.T) {
	st := newTestStore(t)
	s := &Server{cfg: &config.Config{DevMode: true}, sessions: newTestSessionManager(t), store: st}

	session, err := st.CreateAISession("")
	if err != nil {
		t.Fatalf("创建会话失败：%v", err)
	}
	if err := st.SaveAIConversation(session.ID, "{不是 JSON", "也是坏的", ""); err != nil {
		t.Fatalf("写入失败：%v", err)
	}

	issued := httptest.NewRecorder()
	s.sessions.Issue(issued, httptest.NewRequest(http.MethodPost, "/api/auth/login", nil), 1)

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/ai/history?session=%d", session.ID), nil)
	for _, c := range issued.Result().Cookies() {
		req.AddCookie(c)
	}

	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("存档损坏时仍应返回 200，得到 %d：%s", w.Code, w.Body.String())
	}
	// 损坏的内容直接透传会让整个响应编码失败，因此必须退化成空值。
	if body := w.Body.String(); !strings.Contains(body, `"history":[]`) {
		t.Fatalf("损坏的历史应当退化成空数组：%s", body)
	} else if !strings.Contains(body, `"toolStatus":{}`) {
		t.Fatalf("损坏的工具状态应当退化成空对象：%s", body)
	}
}

// newAITestServer 造一个带真实库与登录会话的 Server，供会话相关接口测试使用。
func newAITestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	s := &Server{cfg: &config.Config{DevMode: true}, sessions: newTestSessionManager(t), store: st}
	return s, st
}

// authedGet 以已登录身份请求一个 GET 接口。
func authedGet(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	issued := httptest.NewRecorder()
	s.sessions.Issue(issued, httptest.NewRequest(http.MethodPost, "/api/auth/login", nil), 1)

	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range issued.Result().Cookies() {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, req)
	return w
}

// TestAIHistoryIsPerSession 验证历史接口按会话隔离。
//
// 这是多会话最容易出错的地方：接口若忽略 session 参数、退回"最近一个会话"，
// 用户切过去的第一屏就会看到另一个会话的内容，接着提问时模型也会带着
// 那段上下文回答。
func TestAIHistoryIsPerSession(t *testing.T) {
	s, st := newAITestServer(t)

	first, _ := st.CreateAISession("")
	second, _ := st.CreateAISession("")
	if err := st.SaveAIConversation(first.ID, `[{"role":"user","content":"第一个会话"}]`, "{}", "第一个会话"); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	if err := st.SaveAIConversation(second.ID, `[{"role":"user","content":"第二个会话"}]`, "{}", "第二个会话"); err != nil {
		t.Fatalf("写入失败：%v", err)
	}

	for _, tc := range []struct {
		id      int64
		content string
	}{
		{first.ID, "第一个会话"},
		{second.ID, "第二个会话"},
	} {
		w := authedGet(t, s, fmt.Sprintf("/api/ai/history?session=%d", tc.id))
		if w.Code != http.StatusOK {
			t.Fatalf("会话 %d 读取历史失败：%d %s", tc.id, w.Code, w.Body.String())
		}
		if body := w.Body.String(); !strings.Contains(body, tc.content) {
			t.Fatalf("会话 %d 读到了别人的内容：%s", tc.id, body)
		}
	}
}

// 会话 ID 缺失或不存在时不能退化成"随便给一个"，否则界面会显示错内容。
func TestAIHistoryRequiresValidSession(t *testing.T) {
	s, _ := newAITestServer(t)

	if w := authedGet(t, s, "/api/ai/history"); w.Code != http.StatusBadRequest {
		t.Fatalf("缺少会话参数应当返回 400，得到 %d：%s", w.Code, w.Body.String())
	}
	if w := authedGet(t, s, "/api/ai/history?session=9999"); w.Code != http.StatusNotFound {
		t.Fatalf("会话不存在应当返回 404，得到 %d：%s", w.Code, w.Body.String())
	}
}

// TestAISessionTitle 验证自动标题：取第一句用户提问、折叠空白、按字符截断。
func TestAISessionTitle(t *testing.T) {
	history := []ai.Message{
		{Role: "system", Content: "提示"},
		{Role: "user", Content: "  帮我   看看\n磁盘  "},
	}
	if got := aiSessionTitle(history); got != "帮我 看看 磁盘" {
		t.Fatalf("标题没有正确折叠空白：%q", got)
	}

	long := strings.Repeat("中", aiSessionTitleMaxRunes+10)
	got := aiSessionTitle([]ai.Message{{Role: "user", Content: long}})
	runes := []rune(got)
	if len(runes) != aiSessionTitleMaxRunes+1 || runes[len(runes)-1] != '…' {
		t.Fatalf("超长标题应当按字符截断并加省略号：%q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("截断产生了非法 UTF-8：%q", got)
	}

	if got := aiSessionTitle([]ai.Message{{Role: "system", Content: "只有系统提示"}}); got != "" {
		t.Fatalf("没有用户提问时不该编出标题：%q", got)
	}
}

// 清空对话是持久化数据上的写操作，必须先过会话这一关。
func TestAIClearHistoryRequiresAuth(t *testing.T) {
	s := &Server{cfg: &config.Config{DevMode: true}, sessions: newTestSessionManager(t)}

	req := httptest.NewRequest(http.MethodDelete, "/api/ai/history", nil)
	req.Header.Set(csrfHeader, "1")

	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应被挡下（401），得到 %d：%s", w.Code, w.Body.String())
	}
}

// 造一轮对话：user -> assistant(带 tool_calls) -> tool 结果。
func turn(index int) []ai.Message {
	return []ai.Message{
		{Role: "user", Content: strings.Repeat("问题", index+1)},
		{Role: "assistant", ToolCalls: []ai.ToolCall{{ID: "call-1"}}},
		{Role: "tool", ToolCallID: "call-1", Content: "工具输出"},
	}
}

func TestTrimHistoryKeepsShortHistory(t *testing.T) {
	history := []ai.Message{{Role: "system", Content: "sys"}}
	for i := 0; i < 3; i++ {
		history = append(history, turn(i)...)
	}

	out := trimHistory(history)
	if len(out) != len(history) {
		t.Fatalf("短历史不该被裁剪：%d → %d", len(history), len(out))
	}
}

func TestTrimHistoryNeverOrphansToolMessages(t *testing.T) {
	history := []ai.Message{{Role: "system", Content: "sys"}}
	for i := 0; i < 40; i++ {
		history = append(history, turn(i)...)
	}

	out := trimHistory(history)
	if out[0].Role != "system" {
		t.Fatalf("首条必须是 system，实际 %s", out[0].Role)
	}
	if len(out) > maxHistoryMessages+1 {
		t.Fatalf("裁剪后仍超过条数上限：%d", len(out))
	}
	// 关键断言：裁剪点不能落在孤立的 tool 消息上。
	// 一旦 out[1] 是 tool 消息，说明它前面的 assistant tool_calls 被丢掉了，
	// 这种历史会被服务端直接拒绝。
	if out[1].Role == "tool" {
		t.Fatal("裁剪后出现孤立的 tool 消息")
	}
	// 每一条 tool 消息前面都必须有一条带 tool_calls 的 assistant。
	for i := 1; i < len(out); i++ {
		if out[i].Role != "tool" {
			continue
		}
		paired := false
		for j := i - 1; j >= 0; j-- {
			if out[j].Role == "user" {
				break
			}
			if out[j].Role == "assistant" && len(out[j].ToolCalls) > 0 {
				paired = true
				break
			}
		}
		if !paired {
			t.Fatalf("第 %d 条 tool 消息没有对应的 assistant tool_calls", i)
		}
	}
}

func TestTrimHistoryTruncatesOversizedMessage(t *testing.T) {
	big := strings.Repeat("x", maxMessageBytes*2)
	history := []ai.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "看下磁盘"},
		{Role: "assistant", ToolCalls: []ai.ToolCall{{ID: "c1"}}},
		{Role: "tool", ToolCallID: "c1", Content: big},
	}

	out := trimHistory(history)
	last := out[len(out)-1]
	if len(last.Content) > maxMessageBytes+64 {
		t.Fatalf("超大工具输出未被截断：%d 字节", len(last.Content))
	}
	if !strings.Contains(last.Content, "已截断") {
		t.Fatal("截断后应当留下明确标记，让模型知道自己看到的是片段")
	}
	if out[0].Content != "sys" {
		t.Fatal("system 提示不应被改动")
	}
}

func TestTrimHistoryDropsOldestTurnsWhenOverBudget(t *testing.T) {
	// 每轮都很大，总量必然超预算；应当从最老的一整轮开始丢。
	history := []ai.Message{{Role: "system", Content: "sys"}}
	for i := 0; i < 12; i++ {
		history = append(history, []ai.Message{
			{Role: "user", Content: strings.Repeat("u", maxMessageBytes)},
			{Role: "assistant", ToolCalls: []ai.ToolCall{{ID: "c"}}},
			{Role: "tool", ToolCallID: "c", Content: strings.Repeat("t", maxMessageBytes)},
		}...)
	}

	out := trimHistory(history)
	if totalBytes(out) > maxHistoryBytes {
		t.Fatalf("裁剪后总量仍超预算：%d", totalBytes(out))
	}
	if out[0].Role != "system" {
		t.Fatal("首条必须保留 system")
	}
	if out[1].Role == "tool" {
		t.Fatal("丢弃整轮时破坏了 assistant/tool 配对")
	}
	if len(out) < 2 {
		t.Fatal("不该把最后一轮也丢掉")
	}
}

// TestTrimHistoryKeepsToolChainIntact 是坏历史的复现用例。
//
// 形态取自真实故障：一次提问（单条 user）后面跟着几十次工具调用，
// 整个历史里只有这一条 user。旧实现按条数取窗口后向后找 user，窗口整段
// 落在工具链里就一条都找不到，兜底退化成"只保留最后一条消息"——那往往是
// 一条 tool，于是切出孤立 tool，落库后每次请求都被服务端拒绝。
//
// 修复后裁剪以"整轮"为单位，只丢最老的整轮，因此必须保住这条唯一的轮次。
func TestTrimHistoryKeepsToolChainIntact(t *testing.T) {
	history := []ai.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "看看这台机器"},
	}
	// 一轮提问里模型调用了 30 次工具，条数远超 maxHistoryMessages。
	for i := 0; i < 30; i++ {
		id := "call-" + string(rune('a'+i%26))
		history = append(history,
			ai.Message{Role: "assistant", ToolCalls: []ai.ToolCall{{ID: id}}},
			ai.Message{Role: "tool", ToolCallID: id, Content: "输出"},
		)
	}

	out := trimHistory(history)
	if out[0].Role != "system" {
		t.Fatalf("首条必须是 system，实际 %s", out[0].Role)
	}
	if len(out) > 1 && out[1].Role == "tool" {
		t.Fatal("裁剪切出了孤立的 tool 消息（这正是导致 400 的原因）")
	}
	if len(out) > 1 && out[1].Role != "user" {
		t.Fatalf("system 之后应当是本轮唯一的 user，实际 %s", out[1].Role)
	}
	// 唯一的这一轮不能被丢：丢了就等于把用户的问题和模型全部动作一起抹掉。
	if len(out) != len(history) {
		t.Fatalf("只有一轮时不该丢弃任何内容：%d → %d", len(history), len(out))
	}
}

// TestSanitizeHistoryDropsOrphanTool 验证已损坏的存档能够自愈。
//
// 现场存档长这样：system -> tool -> user -> user。裸 tool 在前面没有
// assistant tool_calls，直接发出去必然 400，且每次读回都坏，用户只能
// 靠"清空对话"逃生——而那个动作他无从知晓。
func TestSanitizeHistoryDropsOrphanTool(t *testing.T) {
	broken := []ai.Message{
		{Role: "system", Content: "sys"},
		{Role: "tool", ToolCallID: "call_orphan", Content: "一万字节的进程列表"},
		{Role: "user", Content: "继续"},
		{Role: "user", Content: "继续"},
	}

	out := sanitizeHistory(broken)
	if out[0].Role != "system" {
		t.Fatalf("首条必须是 system：%+v", out)
	}
	for i, msg := range out {
		if msg.Role == "tool" {
			t.Fatalf("第 %d 条仍是孤立 tool，没有被清洗掉", i)
		}
	}
	// 用户说过的话必须留下：清洗是减法，不能把用户的提问也一并抹掉。
	users := 0
	for _, msg := range out {
		if msg.Role == "user" && msg.Content == "继续" {
			users++
		}
	}
	if users != 2 {
		t.Fatalf("用户的提问不该被清洗：%+v", out)
	}
}

// TestSanitizeHistoryKeepsValidPairs 确保清洗只做减法、不误伤正常历史。
func TestSanitizeHistoryKeepsValidPairs(t *testing.T) {
	valid := []ai.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "看看进程"},
		{Role: "assistant", ToolCalls: []ai.ToolCall{{ID: "c1"}}},
		{Role: "tool", ToolCallID: "c1", Content: "输出"},
		{Role: "assistant", Content: "机器状态正常"},
	}

	out := sanitizeHistory(valid)
	if len(out) != len(valid) {
		t.Fatalf("正常历史不该被改动：%d → %d", len(valid), len(out))
	}
}

// TestTrimHistoryThenSanitizeIsSafe 覆盖"先裁剪、再清洗"的完整链路，
// 确保两者组合后历史始终可以直接发给模型。
func TestTrimHistoryThenSanitizeIsSafe(t *testing.T) {
	history := []ai.Message{{Role: "system", Content: "sys"}}
	for i := 0; i < 25; i++ {
		history = append(history, turn(i)...)
	}

	out := sanitizeHistory(trimHistory(history))
	if out[0].Role != "system" {
		t.Fatal("首条必须是 system")
	}
	// 每一条 tool 前面都必须能找到带 tool_calls 的 assistant，且中间没有 user。
	for i := 1; i < len(out); i++ {
		if out[i].Role != "tool" {
			continue
		}
		paired := false
		for j := i - 1; j >= 0; j-- {
			if out[j].Role == "user" {
				break
			}
			if out[j].Role == "assistant" && len(out[j].ToolCalls) > 0 {
				paired = true
				break
			}
		}
		if !paired {
			t.Fatalf("裁剪+清洗后第 %d 条 tool 仍是孤立的", i)
		}
	}
}

// ---------- 上下文压缩 ----------

// fakeChatServer 起一个假的 OpenAI 兼容接口，返回给定的助手回复。
//
// 用真实的 HTTP 服务而不是打桩 Client，是为了让 compactHistory 走完整
// 链路：构造请求 → 序列化 → 解析响应。摘要逻辑一旦把消息构造错（比如
// 角色写错），只有真实走一遍 JSON 才看得出来。
func fakeChatServer(t *testing.T, reply string) *ai.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("请求路径不对：%s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": reply}},
			},
		})
	}))
	t.Cleanup(srv.Close)
	return ai.NewClient(ai.Config{BaseURL: srv.URL, Model: "test-model"})
}

// bigRound 造一轮体积很大的对话，用来把历史推过预算。
func bigRound(i int) []ai.Message {
	return []ai.Message{
		{Role: "user", Content: strings.Repeat("问", maxMessageBytes/3)},
		{Role: "assistant", ToolCalls: []ai.ToolCall{{ID: "c"}}},
		{Role: "tool", ToolCallID: "c", Content: strings.Repeat("答", maxMessageBytes)},
	}
}

func TestPlanCompactionSkipsWithinBudget(t *testing.T) {
	history := []ai.Message{{Role: "system", Content: "sys"}}
	for i := 0; i < 3; i++ {
		history = append(history, turn(i)...)
	}
	if _, ok := planCompaction(history); ok {
		t.Fatal("没超预算时不该压缩：那是白白多花一次模型调用")
	}
}

func TestPlanCompactionPicksOldestRounds(t *testing.T) {
	history := []ai.Message{{Role: "system", Content: "sys"}}
	for i := 0; i < 12; i++ {
		history = append(history, bigRound(i)...)
	}

	plan, ok := planCompaction(history)
	if !ok {
		t.Fatal("超预算时应当给出压缩方案")
	}
	if len(plan.toSummarize) == 0 {
		t.Fatal("应当有轮次被划入待压缩")
	}
	if len(plan.kept) == 0 {
		t.Fatal("最后一轮必须保留：那是用户当前的问题")
	}
	// 待压缩的必须是最老的轮次，保留的必须是较新的。
	first := plan.toSummarize[0][0].Content
	if !strings.HasPrefix(first, "问") {
		t.Fatalf("待压缩的应当是最老的轮次：%q", first[:20])
	}
}

// 压缩成功后，历史里应当出现一段摘要，并且原有结构依然合法。
func TestCompactHistoryProducesSummary(t *testing.T) {
	client := fakeChatServer(t, "1. 用户想排查磁盘占用\n2. 已确认 / 使用率 92%（事实）")
	s := &Server{}

	history := []ai.Message{{Role: "system", Content: "sys"}}
	for i := 0; i < 12; i++ {
		history = append(history, bigRound(i)...)
	}

	out, ok := s.compactHistory(context.Background(), client, history)
	if !ok {
		t.Fatal("压缩应当成功")
	}
	if out[0].Role != "system" || out[0].Content != "sys" {
		t.Fatalf("系统提示必须原样保留在第一位：%+v", out[0])
	}
	if !isSummary(out[1]) {
		t.Fatalf("摘要应当紧跟在系统提示之后：%+v", out[1])
	}
	if !strings.Contains(out[1].Content, "92%") {
		t.Fatal("摘要内容应当来自模型返回，而不是空壳")
	}
	// 摘要之后必须是完整的轮次，不能有孤立 tool。
	if out[2].Role != "user" {
		t.Fatalf("摘要之后应当从一轮的 user 开始，实际 %s", out[2].Role)
	}
	for i := 2; i < len(out); i++ {
		if out[i].Role != "tool" {
			continue
		}
		paired := false
		for j := i - 1; j >= 2; j-- {
			if out[j].Role == "user" {
				break
			}
			if out[j].Role == "assistant" && len(out[j].ToolCalls) > 0 {
				paired = true
				break
			}
		}
		if !paired {
			t.Fatalf("压缩后第 %d 条 tool 成了孤立消息", i)
		}
	}
	// 压缩的目的就是把体积降回预算内。
	if totalBytes(out) > maxHistoryBytes {
		t.Fatalf("压缩后仍超预算：%d", totalBytes(out))
	}
}

// 压缩调用失败时必须降级，不能让对话本身出错。
func TestCompactHistoryDegradesOnFailure(t *testing.T) {
	// 指向一个已经关闭的端口，请求必然失败。
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	client := ai.NewClient(ai.Config{BaseURL: url, Model: "m"})

	s := &Server{}
	history := []ai.Message{{Role: "system", Content: "sys"}}
	for i := 0; i < 12; i++ {
		history = append(history, bigRound(i)...)
	}

	out, ok := s.compactHistory(context.Background(), client, history)
	if ok {
		t.Fatal("接口不可用时不该报告压缩成功")
	}
	if len(out) != len(history) {
		t.Fatalf("降级时应当原样返回历史：%d → %d", len(history), len(out))
	}
}

// 模型返回空内容时同样要降级，否则会留下一段没有信息量的摘要。
func TestCompactHistoryDegradesOnEmptyReply(t *testing.T) {
	client := fakeChatServer(t, "   ")
	s := &Server{}

	history := []ai.Message{{Role: "system", Content: "sys"}}
	for i := 0; i < 12; i++ {
		history = append(history, bigRound(i)...)
	}

	if _, ok := s.compactHistory(context.Background(), client, history); ok {
		t.Fatal("模型返回空摘要时不该当作成功")
	}
}

// 摘要消息必须能被 trimHistory 保留，否则压缩的成果会在下一次裁剪时丢掉。
func TestTrimHistoryPreservesSummary(t *testing.T) {
	history := []ai.Message{
		{Role: "system", Content: "sys"},
		{Role: "system", Content: summaryPrefix + "早前确认过磁盘 92%"},
	}
	for i := 0; i < 20; i++ {
		history = append(history, turn(i)...)
	}

	out := trimHistory(history)
	if out[0].Role != "system" || out[0].Content != "sys" {
		t.Fatalf("系统提示必须还在第一位：%+v", out[0])
	}
	if !isSummary(out[1]) {
		t.Fatalf("摘要不该被裁剪掉：%+v", out[1])
	}
	if !strings.Contains(out[1].Content, "92%") {
		t.Fatal("摘要内容被改动了")
	}
}

// 二次压缩时，旧摘要要合并进新摘要，否则每次压缩都会丢掉更早的要点。
func TestCompactHistoryMergesPreviousSummary(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": "合并后的摘要"}},
			},
		})
	}))
	defer srv.Close()
	client := ai.NewClient(ai.Config{BaseURL: srv.URL, Model: "m"})

	s := &Server{}
	history := []ai.Message{
		{Role: "system", Content: "sys"},
		{Role: "system", Content: summaryPrefix + "早前要点：磁盘 92%"},
	}
	for i := 0; i < 12; i++ {
		history = append(history, bigRound(i)...)
	}

	out, ok := s.compactHistory(context.Background(), client, history)
	if !ok {
		t.Fatal("压缩应当成功")
	}
	if !strings.Contains(gotBody, "早前要点") {
		t.Fatal("旧摘要必须作为输入发给模型，否则更早的要点会永久丢失")
	}
	// 只应有一段摘要：旧的要被新的取代，而不是并排堆着。
	count := 0
	for _, msg := range out {
		if isSummary(msg) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("摘要应当只有一段，实际 %d 段", count)
	}
}

// clipText 必须在字符边界上截断，不能切出非法 UTF-8。
func TestClipTextKeepsValidUTF8(t *testing.T) {
	text := strings.Repeat("中", 10) // 每字 3 字节
	out := clipText(text, 4)        // 4 落在一个字符中间

	if !utf8.ValidString(out) {
		t.Fatalf("截断产生了非法 UTF-8：%q", out)
	}
	if !strings.HasSuffix(out, "…") {
		t.Fatal("截断应当留下省略标记")
	}
	if got := strings.Count(out, "中"); got != 1 {
		t.Fatalf("按 4 字节截断应只留 1 个完整字符，实际 %d 个：%q", got, out)
	}

	if got := clipText("abc", 10); got != "abc" {
		t.Fatalf("未超限时不该改动：%q", got)
	}
}
