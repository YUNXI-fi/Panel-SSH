package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"panelssh/internal/env"
	"panelssh/internal/store"
	"panelssh/internal/sysinfo"
)

const (
	// commandTimeout 是单条采集命令的时长上限。
	// /proc 读取本身是毫秒级，但 df 卡在无响应的网络挂载点、ps 撞上 D 状态进程时
	// 都会长时间不返回 —— 没有这道上限，请求会永久挂起。
	commandTimeout = 20 * time.Second
	// metricsTTL / processTTL 是采样结果的最短复用窗口。
	// 前端按 3 秒轮询，取值略小于它：既能让多个监控窗口共享同一次采样，
	// 又不会让曲线看起来"卡住不动"。
	metricsTTL = 2 * time.Second
	processTTL = 3 * time.Second
)

// errExecTimeout 表示远端在规定时间内没有返回。
var errExecTimeout = errors.New("目标主机未在规定时间内返回")

// LocalHostID 是「宿主机」这一内置监控目标的保留 ID。
//
// 面板自己就跑在宿主机上，把它的负载常驻在界面上是最省事的"这台机器还活着吗"，
// 用户不必为此再配一台 SSH 到自己的主机。真实主机的 ID 由 SQLite 从 1 开始分配，
// 0 不会被占用。
const LocalHostID int64 = 0

// errLocalHostUnsupported 表示该操作需要一条 SSH 会话，而宿主机是本地直采的。
var errLocalHostUnsupported = errors.New("宿主机目标不支持该操作")

// processSnapshot 是进程列表的一次采样结果。
type processSnapshot struct {
	Processes []sysinfo.Process `json:"processes"`
	Total     int               `json:"total"`
}

// runOnHost 在目标主机执行一次命令并返回标准输出。
//
// 每次采集只新建一个 SSH channel（session），这在已建立的连接上很轻量，
// 不需要重新握手。但必须带上超时：超时后关闭本次 session 即可让阻塞中的
// Run 立刻返回，而不会牵动连接池里被其它窗口共用的那条连接。
func (s *Server) runOnHost(hostID int64, command string) (string, error) {
	if hostID == LocalHostID {
		return runLocal(command)
	}

	lease, err := s.leaseHost(hostID)
	if err != nil {
		return "", err
	}
	defer lease.Release()

	session, err := lease.Client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()

	select {
	case runErr := <-done:
		if runErr != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = runErr.Error()
			}
			return stdout.String(), errors.New(msg)
		}
		return stdout.String(), nil
	case <-time.After(commandTimeout):
		_ = session.Close()
		return "", errExecTimeout
	}
}

// runLocal 在面板所在的这台机器上执行一次采集命令。
//
// 命令与远端用的是同一份 sysinfo 脚本，因此解析路径完全不需要分支。
// 刻意不走"SSH 到 127.0.0.1"：面板本来就跑在这台机器上，本地执行不依赖
// sshd 与凭据，也不会因为目标机只放行内网地址而失败。
func runLocal(command string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// 超时与"命令自己失败"必须分开：前者是 504（上游没响应），
		// 后者是 502（执行出错），混在一起会把本地采集的偶发慢当成故障。
		if ctx.Err() != nil {
			return "", errExecTimeout
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), errors.New(msg)
	}
	return stdout.String(), nil
}

// writeHostError 把主机相关错误映射为合适的状态码：
// 主机不存在是 404，采集超时是 504（上游无响应），其余按上游故障返回 502。
func (s *Server) writeHostError(w http.ResponseWriter, err error, prefix string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "主机不存在")
	case errors.Is(err, errLocalHostUnsupported):
		writeErr(w, http.StatusBadRequest, "宿主机只提供实时指标查看，不支持该操作")
	case errors.Is(err, errExecTimeout):
		writeErr(w, http.StatusGatewayTimeout, prefix+"：目标主机 "+commandTimeout.String()+" 内未返回，请检查该主机负载与网络")
	case errors.Is(err, env.ErrNoPrivilege):
		// 提不了权是目标机的账号配置问题，不是"网关出错"，因此与领域错误一样给 400。
		writeErr(w, http.StatusBadRequest, prefix+"："+err.Error())
	default:
		writeErr(w, http.StatusBadGateway, prefix+"："+err.Error())
	}
}

// rejectLocalHostChange 挡下宿主机上的系统级变更，被挡时已写好响应。
//
// 宿主机就是面板自己所在的那台机器：存储与网络变更动的是它赖以运行的系统本身
// —— 格式化它的一块盘、改掉它自己的网络配置，代价不是"连不上某台目标机"，
// 而是"面板连同它管着的一切一起失联"，且没有任何外部通道能把它救回来。
// 所以这两类写操作对宿主机关闭；只读采集、命令执行不受影响。
func (s *Server) rejectLocalHostChange(w http.ResponseWriter, hostID int64, what string) bool {
	if hostID != LocalHostID {
		return false
	}
	writeErr(w, http.StatusBadRequest, "宿主机不支持"+what+"：这会改到面板自己所在的系统")
	return true
}

// sampleMetrics 取一台主机的指标：缓存窗口内复用，真实采样则记入历史。
//
// 采样与"记历史"必须绑在一起，否则两条调用路径（监控窗口、AI 助手的
// get_metrics）会各记一半，历史里就会出现莫名其妙的空洞。
func (s *Server) sampleMetrics(hostID int64, fresh bool) (*sysinfo.Metrics, error) {
	fetch := func() (*sysinfo.Metrics, error) {
		output, err := s.runOnHost(hostID, sysinfo.MetricsCommand())
		if err != nil {
			return nil, err
		}
		at := time.Now()
		metrics, err := s.collector.Collect(hostID, output, at)
		if err != nil {
			return nil, err
		}
		// 只有真的跑了一次采集才会到这里（缓存命中不经过 fetch）。
		s.history.record(hostID, metrics, at)
		return metrics, nil
	}

	if fresh {
		return s.metrics.getFresh(hostID, fetch)
	}
	return s.metrics.get(hostID, fetch)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}

	// 采样经过去重与短时缓存：多个监控窗口、以及 AI 助手的 get_metrics
	// 工具同时看一台机器时，只在目标机上跑一次命令。
	// fresh=1 表示用户主动点的"立即采样"，跳过缓存窗口。
	metrics, err := s.sampleMetrics(hostID, r.URL.Query().Get("fresh") == "1")
	if err != nil {
		s.writeHostError(w, err, "采集指标失败")
		return
	}
	writeJSON(w, http.StatusOK, metrics)
}

// handleMetricsHistory 返回一台主机最近若干个采样点，供监控窗口开窗时回填曲线。
//
// 它是纯内存数据、不触发任何目标机操作：窗口打开的那一刻多跑一次采集只会让
// 曲线从"空"变成"一个点"，而回填能把过去几分钟的趋势直接摆在眼前。
func (s *Server) handleMetricsHistory(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	// 主机不存在时也要能分辨：list 对未知主机返回空，和"刚启动还没采过样"
	// 无法区分，因此先确认主机存在（宿主机是本地直采，本来就不在库里）。
	if hostID != LocalHostID {
		if _, err := s.store.GetHost(hostID); err != nil {
			s.writeHostError(w, err, "读取指标历史失败")
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"points": s.history.list(hostID)})
}

func (s *Server) handleProcesses(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}

	fetch := func() (processSnapshot, error) {
		output, err := s.runOnHost(hostID, sysinfo.ProcessesCommand())
		if err != nil {
			return processSnapshot{}, err
		}
		processes, total := sysinfo.ParseProcesses(output)
		return processSnapshot{Processes: processes, Total: total}, nil
	}

	var (
		snapshot processSnapshot
		err      error
	)
	if r.URL.Query().Get("fresh") == "1" {
		snapshot, err = s.procs.getFresh(hostID, fetch)
	} else {
		snapshot, err = s.procs.get(hostID, fetch)
	}
	if err != nil {
		s.writeHostError(w, err, "读取进程列表失败")
		return
	}
	if snapshot.Total == 0 {
		// 命令成功但解析不出任何进程，说明目标系统的 ps 不支持这里的语法，
		// 应当明确报错而不是返回空列表让用户以为机器上没进程。
		writeErr(w, http.StatusBadGateway, "进程列表为空，目标系统的 ps 可能不支持该语法")
		return
	}

	writeJSON(w, http.StatusOK, snapshot)
}

type killProcessRequest struct {
	PID    int    `json:"pid"`
	Signal string `json:"signal"`
}

func (s *Server) handleKillProcess(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	// 结束进程不额外设限：宿主机在这个面板里和远端主机是同一类目标，
	// 用户看得见它的进程，就该能结束它。真正需要防的是"改到系统本身"的操作
	// （存储、网络变更），那些由 rejectLocalHostChange 挡下。
	var req killProcessRequest
	if err := decodeJSONBody(w, r, &req, 1<<16); err != nil {
		return
	}
	if req.Signal == "" {
		req.Signal = sysinfo.KillTerm
	}

	command, err := sysinfo.KillCommand(req.PID, req.Signal)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// 结束别人的进程需要 root，结束自己的不需要 —— 因此是"能提权就提权"，
	// 目标机提不了权时仍旧按普通用户执行，不砍掉今天已经能用的场景。
	if _, err := s.runEscalated(r.Context(), hostID, command, commandTimeout, escalatePreferred); err != nil {
		s.writeHostError(w, err, "结束进程失败")
		return
	}

	// 进程状态已经变了，让下一次读进程表必须重新采样，否则用户会看到
	// 刚被结束的进程还挂在列表里。
	s.procs.forget(hostID)

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pid": req.PID, "signal": req.Signal})
}
