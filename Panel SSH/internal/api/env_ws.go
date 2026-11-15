package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"panelssh/internal/env"
	"panelssh/internal/store"
)

const (
	// envConcurrency 是环境变更的同时在飞主机数。
	// 比批量命令的 16 低：装软件是重操作（吃 CPU/磁盘/带宽），
	// 四台并发已经能让一个小集群排队装完，又不会把每台的下载速度拖穿。
	envConcurrency = 4
	// maxEnvHosts 单次环境操作的主机上限，与批量执行同一量级。
	maxEnvHosts = 200
)

// envWSStart 是客户端的启动消息。
type envWSStart struct {
	Type    string         `json:"type"`
	HostIDs []int64        `json:"hostIds"`
	Request env.RunRequest `json:"request"`
}

// envWSMsg 是服务端流式回传的消息。
type envWSMsg struct {
	Type        string `json:"type"`
	Total       int    `json:"total,omitempty"`
	HostID      int64  `json:"hostId,omitempty"`
	HostName    string `json:"hostName,omitempty"`
	Seq         int    `json:"seq,omitempty"`
	Name        string `json:"name,omitempty"`
	Stream      string `json:"stream,omitempty"`
	Data        string `json:"data,omitempty"`
	Status      string `json:"status,omitempty"`
	ExitCode    int    `json:"exitCode,omitempty"`
	DurationMS  int64  `json:"durationMs,omitempty"`
	OpID        int64  `json:"opId,omitempty"`
	Compensated bool   `json:"compensated,omitempty"`
	Verified    bool   `json:"verified,omitempty"`
	VerifyNote  string `json:"verifyNote,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Error       string `json:"error,omitempty"`
}

// envWSWriter 串行化写出，写失败即取消整批（同 batchWriter 的取舍）。
type envWSWriter struct {
	ws     *wsWriter
	ctx    context.Context
	cancel context.CancelFunc
	failed bool
	mu     sync.Mutex
}

func (w *envWSWriter) write(msg envWSMsg) {
	w.mu.Lock()
	if w.failed {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()
	if err := w.ws.write(w.ctx, msg); err != nil {
		w.mu.Lock()
		w.failed = true
		w.mu.Unlock()
		w.cancel()
	}
}

func (s *Server) handleEnvRun(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(1 << 20)
	defer conn.CloseNow()

	if !s.registerSocket(conn) {
		_ = conn.Close(websocket.StatusGoingAway, "服务正在关闭")
		return
	}
	defer s.unregisterSocket(conn)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var start envWSStart
	if err := wsjson.Read(ctx, conn, &start); err != nil {
		return
	}
	runCtx, cancelRun := context.WithCancel(conn.CloseRead(ctx))
	defer cancelRun()

	out := &envWSWriter{ws: &wsWriter{conn: conn}, ctx: runCtx, cancel: cancelRun}

	if start.Type != "start" {
		out.write(envWSMsg{Type: "error", Error: "首条消息必须是 start"})
		return
	}
	if len(start.HostIDs) == 0 {
		out.write(envWSMsg{Type: "error", Error: "请至少选择一台主机"})
		return
	}
	if len(start.HostIDs) > maxEnvHosts {
		out.write(envWSMsg{Type: "error", Error: fmt.Sprintf("单次最多 %d 台主机", maxEnvHosts)})
		return
	}
	if !validEnvAction(start.Request.Action) {
		out.write(envWSMsg{Type: "error", Error: "不支持的操作类型：" + start.Request.Action})
		return
	}

	// 解析目标：本机（0）直接放行，其余必须是库中存在的主机。
	type target struct {
		id   int64
		name string
	}
	targets := make([]target, 0, len(start.HostIDs))
	seen := map[int64]bool{}
	missing := []int64{}
	for _, id := range start.HostIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		if id == LocalHostID {
			targets = append(targets, target{id: id, name: localHostName})
			continue
		}
		host, err := s.store.GetHost(id)
		if err != nil {
			missing = append(missing, id)
			continue
		}
		targets = append(targets, target{id: id, name: host.Name})
	}

	total := len(targets) + len(missing)
	out.write(envWSMsg{Type: "begin", Total: total})

	var (
		wg       sync.WaitGroup
		sem      = make(chan struct{}, envConcurrency)
		successN int
		failedN  int
		countMu  sync.Mutex
	)

	for _, id := range missing {
		out.write(envWSMsg{Type: "hostEnd", HostID: id, Error: "主机不存在或已被删除"})
	}

	for _, t := range targets {
		wg.Add(1)
		go func(tg target) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-runCtx.Done():
				return
			}

			out.write(envWSMsg{Type: "hostStart", HostID: tg.id, HostName: tg.name})

			ex, _, err := s.envTarget(tg.id)
			events := &envHostEvents{out: out, hostID: tg.id}
			if err != nil {
				events.Finish(nil, err)
				countMu.Lock()
				failedN++
				countMu.Unlock()
				return
			}
			s.envRunner.Run(runCtx, env.Target{HostID: tg.id, HostName: tg.name, Exec: ex}, start.Request, events)

			// 安装成功即可上桌面；卸载/回滚成功后撤掉记录。
			// 只处理声明了 Launch 的目录项，系统环境软件不产生入口。
			if events.lastOK && start.Request.RecipeSlug != "" {
				if recipe := env.GetRecipe(start.Request.RecipeSlug); recipe != nil && recipe.Launch != nil {
					switch start.Request.Action {
					case env.ActionInstall, env.ActionUpgrade:
						_ = s.store.RecordLaunchApp(tg.id, recipe.Slug)
					case env.ActionRemove, env.ActionRollback:
						_ = s.store.RemoveLaunchApp(tg.id, recipe.Slug)
					}
				}
			}

			countMu.Lock()
			if events.lastOK {
				successN++
			} else {
				failedN++
			}
			countMu.Unlock()
		}(t)
	}
	wg.Wait()
	out.write(envWSMsg{Type: "done"})

	// 仅在全部主机都正常跑完时通知；中途断开的批次不发（与批量任务一致）。
	if runCtx.Err() == nil {
		s.notifyEnvFinished(start.Request, total, successN, failedN)
	}
}

func validEnvAction(action string) bool {
	switch action {
	case env.ActionInstall, env.ActionUpgrade, env.ActionRemove,
		env.ActionUpgradeAll, env.ActionMirror, env.ActionRollback:
		return true
	}
	return false
}

// envHostEvents 把 env.Runner 的回调翻译成 WS 消息。
type envHostEvents struct {
	out    *envWSWriter
	hostID int64
	lastOK bool
}

func (e *envHostEvents) StepStart(seq int, name string) {
	e.out.write(envWSMsg{Type: "stepStart", HostID: e.hostID, Seq: seq, Name: name})
}

func (e *envHostEvents) StepLog(seq int, stream, data string) {
	e.out.write(envWSMsg{Type: "stepLog", HostID: e.hostID, Seq: seq, Stream: stream, Data: data})
}

func (e *envHostEvents) StepEnd(r env.StepResult) {
	e.out.write(envWSMsg{
		Type:       "stepEnd",
		HostID:     e.hostID,
		Seq:        r.Seq,
		Name:       r.Name,
		Status:     r.Status,
		ExitCode:   r.ExitCode,
		DurationMS: r.DurationMS,
	})
}

func (e *envHostEvents) Finish(s *env.OpSummary, err error) {
	msg := envWSMsg{Type: "hostEnd", HostID: e.hostID}
	if err != nil {
		msg.Error = err.Error()
		e.lastOK = false
	} else if s != nil {
		e.lastOK = s.Status == store.EnvStatusSuccess
		msg.OpID = s.OpID
		msg.Status = s.Status
		msg.Compensated = s.Compensated
		msg.Verified = s.Verified
		msg.VerifyNote = s.VerifyNote
		msg.Summary = s.Summary
	}
	e.out.write(msg)
}

// notifyEnvFinished 产生一条环境变更汇总通知。
func (s *Server) notifyEnvFinished(req env.RunRequest, total, success, failed int) {
	level := store.LevelSuccess
	title := "环境变更全部成功"
	if failed > 0 {
		level = store.LevelWarning
		title = "环境变更部分失败"
	}
	body := fmt.Sprintf("共 %d 台 · 成功 %d · 失败 %d", total, success, failed)
	if failed > 0 {
		body += "，失败主机已自动尝试补偿，详情见「环境管理-操作记录」"
	}

	actionName := map[string]string{
		env.ActionInstall:    "安装",
		env.ActionUpgrade:    "升级",
		env.ActionRemove:     "卸载",
		env.ActionUpgradeAll: "全部升级",
		env.ActionMirror:     "切换安装源",
		env.ActionRollback:   "回滚",
	}[req.Action]
	if actionName == "" {
		actionName = "环境变更"
	}
	target := ""
	switch {
	case req.RecipeSlug != "":
		target = req.RecipeSlug
		if req.RecipeVersion != "" {
			target += " " + req.RecipeVersion
		}
	case req.Mirror != "":
		target = req.Mirror + " 镜像源"
	case len(req.Specs) > 0:
		names := make([]string, 0, len(req.Specs))
		for _, sp := range req.Specs {
			names = append(names, sp.Name)
		}
		target = strings.Join(names, "、")
	default:
		target = "全部软件包"
	}

	s.notifySecurity(level, fmt.Sprintf("%s：%s", title, actionName+" "+target), body)
}
