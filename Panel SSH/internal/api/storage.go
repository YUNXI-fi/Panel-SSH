package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"panelssh/internal/sysinfo"
)

const (
	// storageTTL 是存储信息的最短复用窗口，与网络采集同理：
	// 多个窗口同时打开时共享一次采集，轮询不会变成对目标机的压力。
	storageTTL = 3 * time.Second

	// storageStepTimeout 是准备步骤（建目录、写凭据、改 fstab）的上限。
	storageStepTimeout = 30 * time.Second

	// storageMountTimeout 比其它步骤宽松：挂载网络存储要等协议协商与认证，
	// 跨地域线路下十几秒是常态，用 30 秒会把它误判成失败。
	storageMountTimeout = 60 * time.Second

	// storageSmartTimeout 要覆盖"把休眠的盘唤醒"这件事：
	// 机械盘从待机转起来要几秒，挂在 USB 桥接后面的更慢。
	storageSmartTimeout = 20 * time.Second

	// storageUsageTimeout 明显更宽：du 是真的在遍历目录树，
	// 一个几百万文件的数据目录跑上一分钟并不稀奇。
	storageUsageTimeout = 90 * time.Second

	// storageFormatTimeout 给得最宽：mkfs 要把元数据铺满整块盘，
	// 1 TB 的机械盘上跑几分钟是正常的，而"超时"并不会中断远端进程。
	storageFormatTimeout = 300 * time.Second

	// storagePartTimeout 覆盖 parted 同步内核分区表与等待 udev 的时间。
	storagePartTimeout = 120 * time.Second
)

// fetchStorage 采集一次最新的存储快照。
func (s *Server) fetchStorage(hostID int64) (*sysinfo.StorageSnapshot, error) {
	output, err := s.runOnHost(hostID, sysinfo.StorageCommand())
	if err != nil {
		return nil, err
	}
	return sysinfo.CollectStorage(output, time.Now())
}

func (s *Server) freshStorage(hostID int64) (*sysinfo.StorageSnapshot, error) {
	return s.storage.getFresh(hostID, func() (*sysinfo.StorageSnapshot, error) {
		return s.fetchStorage(hostID)
	})
}

// handleStorage 返回目标主机的存储现状：块设备、挂载点、fstab。
func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}

	var (
		snapshot *sysinfo.StorageSnapshot
		err      error
	)
	if r.URL.Query().Get("fresh") == "1" {
		snapshot, err = s.freshStorage(hostID)
	} else {
		snapshot, err = s.storage.get(hostID, func() (*sysinfo.StorageSnapshot, error) {
			return s.fetchStorage(hostID)
		})
	}
	if err != nil {
		s.writeHostError(w, err, "读取存储信息失败")
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

// handleStorageApply 执行一次存储操作：挂载、卸载，以及随之而来的 fstab 变更。
//
// 与网络变更不同，这里不需要"确认回滚"那套机制：挂载与卸载都不会切断
// 面板赖以操作的 SSH，出错的代价是"这次操作没成功"，而不是"再也连不上"。
// 因此改为单次同步执行 + 执行后复核，用户在同一个请求里就能看到结果。
func (s *Server) handleStorageApply(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	if s.rejectLocalHostChange(w, hostID, "存储变更") {
		return
	}

	var req sysinfo.StorageRequest
	if err := decodeJSONBody(w, r, &req, 1<<16); err != nil {
		return
	}

	snapshot, err := s.freshStorage(hostID)
	if err != nil {
		s.writeHostError(w, err, "读取存储现状失败")
		return
	}

	plan, err := sysinfo.PlanStorageChange(req, snapshot)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// 准备 → 主操作 → 持久化，任何一步失败都先把已做的事情收拾掉再报错，
	// 不给目标机留下"目录建好了、凭据写完了，但什么都没挂上"的中间状态。
	for _, command := range plan.Prepare {
		if err := s.runStorageStep(r.Context(), hostID, command); err != nil {
			s.runStorageCleanup(r.Context(), hostID, plan.Cleanup)
			writeErr(w, http.StatusBadGateway, "准备步骤失败："+err.Error())
			return
		}
	}

	// 有些操作没有"主命令"：单独删一条 fstab 条目时，全部动作都在 Persist 里，
	// 这里不能拿一个空字符串去开 SSH 会话。
	if plan.Mount != "" {
		if err := s.runStorageStep(r.Context(), hostID, plan.Mount); err != nil {
			s.runStorageCleanup(r.Context(), hostID, plan.Cleanup)
			writeErr(w, http.StatusBadGateway, storageFailureMessage(req.Action, err))
			return
		}
	}

	for _, command := range plan.Persist {
		if err := s.runStorageStep(r.Context(), hostID, command); err != nil {
			s.runStorageCleanup(r.Context(), hostID, plan.Cleanup)
			writeErr(w, http.StatusBadGateway, "写入 /etc/fstab 失败（已还原到修改前）："+err.Error())
			return
		}
	}

	// 收尾（删除一次性凭据文件）失败不影响操作结果，静默处理。
	// 同样要提权：这些临时文件是刚才以 root 执行的那几步创建的，普通用户删不掉。
	for _, command := range plan.Finish {
		_, _ = s.execEscalated(r.Context(), hostID, command, storageStepTimeout, escalateRequired)
	}

	s.storage.forget(hostID)
	s.network.forget(hostID)

	after, err := s.fetchStorage(hostID)
	if err != nil {
		// 操作已经执行了，只是复核读不到 —— 如实说明，不要让用户以为白做了。
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"summary":    plan.Summary,
			"verified":   false,
			"verifyNote": "操作已执行，但复核时读不到存储状态：" + err.Error(),
		})
		return
	}

	if verifyErr := plan.Expect.Verify(after); verifyErr != nil {
		// 复核不通过意味着"命令报成功但状态不对"，这是最需要让用户看见的情况。
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"summary":    plan.Summary,
			"verified":   false,
			"verifyNote": "命令已执行，但复核发现：" + verifyErr.Error(),
			"snapshot":   after,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"summary":  plan.Summary,
		"verified": true,
		"snapshot": after,
	})
}

// storageFailureMessage 为不同操作给出贴合的失败前缀。
func storageFailureMessage(action string, err error) string {
	switch action {
	case sysinfo.StorageUnmount:
		// 卸载失败十有八九是"有进程在用"，把排查方向直接写出来，
		// 比让用户对着 "target is busy" 猜要省事得多。
		return "卸载失败：" + err.Error() + "。若提示设备忙，说明有进程正在使用该目录"
	case sysinfo.StorageMountNFS, sysinfo.StorageMountCIFS:
		return "挂载失败：" + err.Error() + "。请检查服务器地址、共享名与凭据"
	case sysinfo.StorageFormat:
		return "格式化失败：" + err.Error()
	case sysinfo.StoragePartLabel, sysinfo.StoragePartCreate, sysinfo.StoragePartDelete:
		return "分区操作失败：" + err.Error()
	case sysinfo.StorageFstabRemove:
		return "修改 /etc/fstab 失败（已还原到修改前）：" + err.Error()
	default:
		return "挂载失败：" + err.Error()
	}
}

// runStorageStep 执行一步存储操作，把非零退出码也当作错误。
//
// execOnHost 把"退出码非 0"视为正常返回（这是执行命令的通用语义），
// 但对存储操作来说它就意味着这一步没成功，必须升级成错误。
func (s *Server) runStorageStep(ctx context.Context, hostID int64, command string) error {
	// 超时要按命令的实际耗时给。给短了的后果不是"白等一会"，而是
	// "面板报失败、远端命令还在跑"—— mkfs 被打断在写元数据的中途，
	// 留下的是一块状态不明的盘，这比慢一点糟糕得多。
	timeout := storageStepTimeout
	switch trimmed := strings.TrimSpace(command); {
	case strings.HasPrefix(trimmed, "mount "):
		timeout = storageMountTimeout
	case strings.HasPrefix(trimmed, "mkfs."):
		// mkfs 要把元数据铺满整块盘，容量越大越久。
		timeout = storageFormatTimeout
	case strings.HasPrefix(trimmed, "parted "):
		// parted 会先同步内核的分区表视图，写新表时还要等 udev。
		timeout = storagePartTimeout
	}

	result, err := s.execEscalated(ctx, hostID, command, timeout, escalateRequired)
	if err != nil {
		return err
	}
	if result.ExitCode == 0 {
		return nil
	}

	message := strings.TrimSpace(result.Stderr)
	if message == "" {
		message = strings.TrimSpace(result.Stdout)
	}
	if message == "" {
		message = fmt.Sprintf("命令以退出码 %d 结束", result.ExitCode)
	}
	if result.StderrClipped || result.StdoutClipped {
		message += "（输出过长已截断）"
	}
	return errors.New(message)
}

// runStorageCleanup 按顺序执行清理，失败也不上报。
//
// 调用它的位置都已经在错误处理路径上：那里的首要任务是把目标机恢复干净，
// 再叠一层错误既改变不了结果，也会盖掉真正要报给用户的原因。
func (s *Server) runStorageCleanup(ctx context.Context, hostID int64, commands []string) {
	for _, command := range commands {
		_, _ = s.execEscalated(ctx, hostID, command, storageStepTimeout, escalateRequired)
	}
}

// handleStorageSmart 读取一块磁盘的 SMART 健康信息。
//
// 刻意不走快照：smartctl 会把休眠中的盘叫醒，本身也有明显耗时。
// 让它跟着 5 秒一轮的轮询跑，等于拿目标机的硬盘寿命换一个没人看的数字。
func (s *Server) handleStorageSmart(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}

	requested := strings.TrimSpace(r.URL.Query().Get("device"))
	if requested == "" {
		writeErr(w, http.StatusBadRequest, "缺少 device 参数")
		return
	}

	snapshot, err := s.storage.get(hostID, func() (*sysinfo.StorageSnapshot, error) {
		return s.fetchStorage(hostID)
	})
	if err != nil {
		s.writeHostError(w, err, "读取存储信息失败")
		return
	}

	// 用快照里的路径去拼命令，而不是请求里的那个字符串：
	// 查询接口也不该成为把用户输入送进远端命令行的入口。
	device := sysinfo.FindDevicePath(snapshot, requested)
	if device == "" {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("主机上不存在设备 %s", requested))
		return
	}

	// 这里只看 stdout、不看退出码：smartctl 用位掩码表达状态，其中 bit3
	// 正是"磁盘即将故障"—— 那是最该报给用户的结果，绝不能因为它非零
	// 就当成"命令失败"而丢掉。
	result, err := s.execOnHost(r.Context(), hostID, sysinfo.SmartCommand(device), storageSmartTimeout)
	if err != nil {
		s.writeHostError(w, err, "读取 SMART 信息失败")
		return
	}

	report := sysinfo.CollectSmart(device, result.Stdout)
	// 解析不出内容时，stdout 里没有可用的解释，退回 smartctl 的 stderr：
	// "权限不足"和"设备不支持"要给用户完全不同的行动指引。
	if !report.Available {
		if message := strings.TrimSpace(result.Stderr); message != "" {
			report.Note = message
		}
	}
	writeJSON(w, http.StatusOK, report)
}

// handleStorageUsage 统计某个已挂载目录下各子目录的占用。
//
// 同样不走快照：du 的结果只对"此刻"有意义，缓存下来既占内存又容易误导。
func (s *Server) handleStorageUsage(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}

	target := strings.TrimSpace(r.URL.Query().Get("target"))
	if target == "" {
		writeErr(w, http.StatusBadRequest, "缺少 target 参数")
		return
	}

	depth := 1
	if raw := strings.TrimSpace(r.URL.Query().Get("depth")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		// 上限 3：每多一层，du 要走的目录就多一个数量级，
		// 而"是哪一层吃掉了空间"在一两层内已经足够回答。
		if err != nil || parsed < 1 || parsed > 3 {
			writeErr(w, http.StatusBadRequest, "depth 只能是 1 到 3")
			return
		}
		depth = parsed
	}

	snapshot, err := s.storage.get(hostID, func() (*sysinfo.StorageSnapshot, error) {
		return s.fetchStorage(hostID)
	})
	if err != nil {
		s.writeHostError(w, err, "读取存储信息失败")
		return
	}

	// 只统计"确实挂着的目录"。放开任意路径，这个接口就变成了一个能遍历
	// 整台机器文件系统的入口，而用户真正要回答的问题只是"这个盘被什么占满了"。
	if !isMountedTarget(snapshot, target) {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("%s 不是当前已挂载的目录", target))
		return
	}

	result, err := s.execOnHost(r.Context(), hostID, sysinfo.DiskUsageCommand(target, depth), storageUsageTimeout)
	if err != nil {
		s.writeHostError(w, err, "统计目录用量失败")
		return
	}

	usage := sysinfo.CollectDiskUsage(target, result.Stdout)
	// du 读不到的目录会往 stderr 写警告。那种情况下它只统计到目录本身（几 KB），
	// 面板上看起来就像"这个盘是空的" —— 这个前提必须一起说出来。
	usage.Warning = summarizeStderr(result.Stderr)
	writeJSON(w, http.StatusOK, usage)
}

// summarizeStderr 把命令的警告压成一行。
//
// du 会为每一个读不到的子目录各写一行，一个受限目录能刷出几百行。
// 原样回传既占带宽也没人看，用户需要知道的只是"有些地方没读到，数字不全"。
func summarizeStderr(raw string) string {
	first := ""
	count := 0
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		count++
		if first == "" {
			first = line
		}
	}
	if count == 0 {
		return ""
	}
	if count == 1 {
		return first
	}
	return fmt.Sprintf("%s（共 %d 处）", first, count)
}

// isMountedTarget 判断路径是否是一个已挂载的实体目录。
//
// 排除虚拟文件系统：对 /proc、/sys 这类目录做用量统计没有意义，
// 它们的大小不反映任何真实占用。
func isMountedTarget(snapshot *sysinfo.StorageSnapshot, target string) bool {
	for _, mount := range snapshot.Mounts {
		if mount.Target == target && !mount.Virtual {
			return true
		}
	}
	return false
}
