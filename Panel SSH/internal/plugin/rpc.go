package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"
)

// 与插件进程通信的两条约束。
//
// 请求体上限与安装包（maxPluginArchive）同量级：插件可能一次返回一整张表的数据。
// 超时给得比普通接口宽松：插件自己要去连数据库、跑查询。
const (
	maxRPCPayload     = 32 << 20
	defaultRPCTimeout = 60 * time.Second
)

// ErrPluginNotRunning 表示插件进程没在跑：socket 不存在，或存在但已无人监听。
//
// 单独成一个哨兵错误，是为了让调用方能区分"进程要拉起"和"插件自己报错了" ——
// 前者面板可以尝试拉起，后者只能如实转给前端。
var ErrPluginNotRunning = errors.New("插件进程未运行")

// RPCError 是插件返回的 JSON-RPC 错误。
//
// 面板不解释它的内容，原样转给前端：method 的内容是插件的私有约定，
// 面板替它改写错误信息只会让排查更困难。
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return e.Message }

// rpcRequest / rpcResponse 是 JSON-RPC 2.0 的最小可用子集。
//
// id 固定为 1：插件进程与面板之间每次调用都是一次一答，
// 不存在的"乱序响应"不需要靠 id 去对齐。
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// rpcCall 在插件进程的 Unix Socket 上做一次 JSON-RPC 调用。
//
// 走的是 HTTP over Unix Socket（POST /rpc），而不是裸 socket 上的分帧协议：
// 插件只要起一个普通 HTTP 服务监听 socket，Go、Node、Python 都是一行代码；
// 反过来说，让插件作者手写 JSON-RPC 分帧是纯粹的门槛。面板侧也因此能直接用
// http.Client 的超时、并发与 body 上限，不必自己实现一遍。
func rpcCall(ctx context.Context, socketPath, method string, params json.RawMessage) (json.RawMessage, error) {
	body, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return nil, fmt.Errorf("构造请求失败：%w", err)
	}

	client := &http.Client{
		Timeout: defaultRPCTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socketPath)
			},
			// 每次转发都是一次独立事务，不留空闲连接：进程随时可能被回收或重启，
			// 复用一条指向已消失 socket 的连接只会让下一次调用莫名其妙地失败。
			DisableKeepAlives: true,
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://plugin/rpc", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		if socketGone(err) {
			return nil, ErrPluginNotRunning
		}
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("插件返回了 HTTP %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRPCPayload))
	if err != nil {
		return nil, fmt.Errorf("读取插件响应失败：%w", err)
	}

	var decoded rpcResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("插件响应不是合法的 JSON-RPC：%w", err)
	}
	if decoded.Error != nil {
		return nil, decoded.Error
	}
	return decoded.Result, nil
}

// socketGone 判断错误是不是"socket 没了"，也就是进程没在跑。
//
// 两种情况都要算：socket 文件不存在（进程从未启动或已被清理），
// 以及文件还在但无人监听（进程已退出，socket 文件是残留）。
func socketGone(err error) bool {
	return errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, syscall.ENOENT) ||
		errors.Is(err, syscall.ECONNREFUSED)
}
