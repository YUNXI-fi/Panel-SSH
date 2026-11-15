// Package ai 提供 OpenAI 兼容的大模型客户端，以及执行命令前的安全护栏。
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Message 是一条对话消息。
//
// 同一结构同时承载 user / assistant / tool 三种角色，与 OpenAI 的传输格式对齐：
// assistant 消息可以只带 tool_calls，tool 消息用 ToolCallID 关联到具体调用。
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall 是模型提出的一次工具调用。
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ToolFunction 是工具的函数描述。
type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// Tool 是向模型声明的可用工具。
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// NewTool 构造一个 function 类型的工具声明。
func NewTool(name, description string, parameters map[string]any) Tool {
	return Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        name,
			Description: description,
			Parameters:  parameters,
		},
	}
}

// Config 是模型接入配置。
//
// 只依赖 baseURL + apiKey + model 三要素，是为了同时兼容 OpenAI、DeepSeek、
// 通义、以及自建的 vLLM / Ollama —— 它们都提供 /chat/completions 接口。
type Config struct {
	BaseURL string
	APIKey  string
	Model   string
}

// Client 是 OpenAI 兼容的对话客户端。
type Client struct {
	// mu 保护 config：管理员可以在会话进行中修改模型配置，
	// 正在进行的对话需要安全地读到新配置。
	mu     sync.RWMutex
	config Config
	http   *http.Client
}

// NewClient 构造客户端。
func NewClient(config Config) *Client {
	return &Client{
		config: config,
		// 大模型的响应可能很慢，这里给一个宽松上限；
		// 真正的截止时间由调用方的 context 控制。
		http: &http.Client{Timeout: 5 * time.Minute},
	}
}

// UpdateConfig 热更新配置。
//
// 复用同一个 Client（而不是每次重建）是为了保住 http.Client 的连接池：
// 每轮对话都新建客户端会让每次请求都重新建 TLS 连接。
func (c *Client) UpdateConfig(config Config) {
	c.mu.Lock()
	c.config = config
	c.mu.Unlock()
}

func (c *Client) currentConfig() Config {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.config
}

type chatRequest struct {
	Model      string    `json:"model"`
	Messages   []Message `json:"messages"`
	Tools      []Tool    `json:"tools,omitempty"`
	ToolChoice string    `json:"tool_choice,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Chat 发起一次对话请求。
//
// 刻意不做流式：本项目的工具循环是「一步一停」的（模型要么给结论，要么给工具调用），
// 真正的实时感来自面板把每一步的进展即时推给前端；而模型侧的流式还要拼接
// tool_calls 的分片参数，复杂度高、收益低。
func (c *Client) Chat(ctx context.Context, messages []Message, tools []Tool) (*Message, error) {
	config := c.currentConfig()
	if config.BaseURL == "" || config.Model == "" {
		return nil, errors.New("尚未配置模型，请先在 AI 助手设置中填写接口地址与模型名")
	}

	payload, err := json.Marshal(chatRequest{
		Model:      config.Model,
		Messages:   messages,
		Tools:      tools,
		ToolChoice: "auto",
	})
	if err != nil {
		return nil, err
	}

	endpoint := strings.TrimSuffix(config.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+config.APIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求模型接口失败：%w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("模型接口返回 %d：%s", resp.StatusCode, clip(string(body), 300))
	}

	var parsed chatResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("模型响应无法解析：%s", clip(string(body), 200))
	}
	if parsed.Error != nil {
		return nil, errors.New("模型返回错误：" + parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return nil, errors.New("模型没有返回任何结果")
	}

	message := parsed.Choices[0].Message
	// 部分实现不回传 role，补上以免下一轮构造请求时缺字段。
	if message.Role == "" {
		message.Role = "assistant"
	}
	return &message, nil
}

// Ping 用一次最小请求验证配置是否可用。
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if _, err := c.Chat(ctx, []Message{{Role: "user", Content: "ping"}}, nil); err != nil {
		return err
	}
	return nil
}

func clip(text string, limit int) string {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) <= limit {
		return trimmed
	}
	return trimmed[:limit] + "…"
}
