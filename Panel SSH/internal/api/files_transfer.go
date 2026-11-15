package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"

	"panelssh/internal/store"
)

// 跨主机复制 / 移动。
//
// 数据只有两条路可走：
//
//  1. **直连**：源机已经对目标机配好免密 SSH 信任时，源机直接
//     `tar | ssh 目标机 tar -x`。不经过面板，速度取决于两台机器之间的链路。
//     代价是拿不到字节级进度，也拿不到逐文件明细。
//  2. **中转**：面板同时开两个 SFTP 会话，边读边写、不落盘。任何时候都能用，
//     有字节级进度与逐文件明细。这也是直连失败后的兜底。
//
// 直连的前提只能靠探测：面板不能把自己的凭据临时塞给源机 —— 那等于在别人的
// 机器上留一把钥匙。所以"能不能直连"就是一条 `ssh -o BatchMode=yes` 的试跑。

const (
	// transferBufferSize 是中转时的读写缓冲。64 KiB 与 SSH 通道的窗口大小同量级，
	// 再大只是在面板里多占内存，对吞吐没有帮助。
	transferBufferSize = 64 << 10
	// directProbeTimeout 是直连探测的上限。探测只是 `ssh 目标机 true`，
	// 正常一秒内就回来；设 20 秒是为了容忍慢网络下的 SSH 握手。
	directProbeTimeout = 20 * time.Second
	// directTransferTimeout 覆盖一次直连搬运。这里刻意不设短超时：
	// 数据量决定耗时，而真正需要防的是"链路死了还一直等"，那由 SSH 自己的
	// 连接超时兜底。给到 12 小时是为了不误杀大批量搬运。
	directTransferTimeout = 12 * time.Hour
	// maxRelayEnumErrors 是枚举阶段允许的连续失败次数。
	//
	// 个别子目录读不了（权限、坏挂载）时跳过它比整批作废好；但如果是连接断了，
	// 每个目录都会失败 —— 攒到这么多次就说明不是个别问题，该停了。
	maxRelayEnumErrors = 20
)

// crossTransferRequest 是跨主机搬运的入参：把 Paths 搬进目标主机上的 Target 目录。
type crossTransferRequest struct {
	ToHostID int64    `json:"toHostId"`
	Paths    []string `json:"paths"`
	Target   string   `json:"target"`
	Move     bool     `json:"move"`
}

// transferEvent 是搬运过程中下发给界面的一帧。
//
// 契约与压缩 / 解压一致：一旦开始下发就恒为 200，此后的失败放在 error 帧里；
// 只有流开始前的校验（鉴权、目标已存在、参数非法）才是普通 JSON 错误。
type transferEvent struct {
	// Type 取 prepare / mode / progress / done / error。
	Type string `json:"type"`
	// Mode 取 direct / relay，只在 mode 帧里有值。
	Mode string `json:"mode,omitempty"`
	// Done / Total 是根条目进度（直连只有这个），Path 是当前条目名。
	Done  int    `json:"done,omitempty"`
	Total int    `json:"total,omitempty"`
	Path  string `json:"path,omitempty"`
	// Bytes / BytesTotal 只在中转时有值，界面据此画字节级进度。
	Bytes      int64 `json:"bytes,omitempty"`
	BytesTotal int64 `json:"bytesTotal,omitempty"`
	// Failed 是搬运失败的文件数，随 done 帧一起给出。
	Failed int `json:"failed,omitempty"`
	// Message 是 error 帧里的失败原因。
	Message string `json:"message,omitempty"`
}

// transferStream 按 NDJSON 逐帧写响应，并对 progress 帧节流。
type transferStream struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
	last    time.Time
}

func (t *transferStream) send(ev transferEvent, throttle bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if throttle && time.Since(t.last) < archiveProgressInterval {
		return
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	t.last = time.Now()
	_, _ = t.w.Write(append(data, '\n'))
	if t.flusher != nil {
		t.flusher.Flush()
	}
}

// transferPair 是一个根条目的源路径与目标路径。
type transferPair struct {
	src string
	dst string
}

func (s *Server) handleTransferFiles(w http.ResponseWriter, r *http.Request) {
	srcHostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req crossTransferRequest
	if err := decodeJSONBody(w, r, &req, 1<<20); err != nil {
		return
	}
	if len(req.Paths) == 0 {
		writeErr(w, http.StatusBadRequest, "未选择要传输的条目")
		return
	}
	if req.ToHostID == srcHostID {
		writeErr(w, http.StatusBadRequest, "源与目标是同一台主机，请使用复制 / 移动")
		return
	}

	// 宿主机不在主机表里（ID 是保留的 0），但它是合法的传输目标：
	// 给它一个只有名字的临时目标，后面的中转复制只用到名字与 openSFTP。
	dstHost := &store.Host{ID: LocalHostID, Name: "宿主机"}
	if req.ToHostID != LocalHostID {
		host, err := s.store.GetHost(req.ToHostID)
		if err != nil {
			writeErr(w, http.StatusNotFound, "目标主机不存在")
			return
		}
		dstHost = host
	}

	srcSess, src, ok := s.openSFTP(w, srcHostID)
	if !ok {
		return
	}
	defer srcSess.Close()

	dstSess, dst, ok := s.openSFTP(w, req.ToHostID)
	if !ok {
		return
	}
	defer dstSess.Close()

	dir, ok := s.requireTargetDir(w, dst, req.Target)
	if !ok {
		return
	}

	// 先把所有根条目的来源与落点都算清并校验完，再开始下发进度帧：
	// 一旦流开始，状态码就固定是 200，冲突只能放在 error 帧里说，用户会以为
	// 传输已经进行了一段。
	sources := make([]string, 0, len(req.Paths))
	for _, raw := range req.Paths {
		srcPath, ok := requireAbsPath(w, raw)
		if !ok {
			return
		}
		if srcPath == "/" {
			writeErr(w, http.StatusBadRequest, "不允许传输根目录")
			return
		}
		if _, err := src.Lstat(srcPath); err != nil {
			if errors.Is(err, errSFTPTimeout) {
				s.writeSFTPError(w, err, "读取源端条目失败")
				return
			}
			writeErr(w, http.StatusNotFound, "对象不存在："+srcPath)
			return
		}
		sources = append(sources, srcPath)
	}

	// 选中项互相嵌套（同时选了 /a 和 /a/b）时只搬外层：搬 /a 已经把它带过去了，
	// 再搬一次会在目标上重写同一份内容，移动时还会因为源已被删而报失败。
	sources = dropNestedPaths(sources)

	pairs := make([]transferPair, 0, len(sources))
	for _, srcPath := range sources {
		dstPath := path.Join(dir, path.Base(srcPath))
		if _, err := dst.Lstat(dstPath); err == nil {
			writeErr(w, http.StatusConflict, "目标已存在："+dstPath)
			return
		} else if errors.Is(err, errSFTPTimeout) {
			s.writeSFTPError(w, err, "检查目标路径失败")
			return
		}
		pairs = append(pairs, transferPair{src: srcPath, dst: dstPath})
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}

	action := "复制"
	if req.Move {
		action = "移动"
	}
	task := s.beginFileTask(srcHostID, store.TaskKindTransfer,
		action+"到 "+dstHost.Name+"："+dir, "")
	var failure string
	defer func() {
		switch {
		case r.Context().Err() != nil:
			task.abort()
		case failure != "":
			task.finish(failure)
		default:
			task.finish("")
		}
	}()

	stream := &transferStream{w: w, flusher: flusher}

	// 直连探测与执行都在源机上跑，失败时才退回中转。
	if s.canDirectTransfer(r.Context(), srcHostID, dstHost) {
		stream.send(transferEvent{Type: "mode", Mode: "direct"}, false)
		done, directErr := s.runDirectTransfer(r.Context(), src, srcHostID, dstHost, pairs, dir, req.Move, stream, task)
		if directErr == nil {
			stream.send(transferEvent{Type: "done", Done: len(pairs), Total: len(pairs)}, false)
			return
		}
		// 客户端已经走了：既不该去清理目标，也不必再起一趟中转 ——
		// 那只会让目标机白忙一场，而结果没人看得到。
		if r.Context().Err() != nil {
			return
		}

		// 回退之前必须把这次直连在目标上解出来的半套数据清掉：
		// 否则中转会撞上"目标已存在"，或者留下一个残缺目录。
		// 只清失败的那一个根条目 —— 前面已经成功的根条目（含 move 时已删掉源）必须保留。
		if cleanupErr := removeTreeSFTP(dst, pairs[done].dst); cleanupErr != nil {
			failure = fmt.Sprintf("直连传输失败（%v），且无法清理目标上的残留（%v）", directErr, cleanupErr)
			stream.send(transferEvent{Type: "error", Message: failure}, false)
			return
		}
		stream.send(transferEvent{
			Type:    "mode",
			Mode:    "relay",
			Message: "直连未成功（" + tailOutput(directErr.Error()) + "），已改走面板中转",
		}, false)
		pairs = pairs[done:]
	} else {
		stream.send(transferEvent{Type: "mode", Mode: "relay"}, false)
	}

	// 中转先枚举源端树才算得出进度分母，这一小段没有输出，得说清在做什么。
	stream.send(transferEvent{Type: "prepare"}, false)

	failed, relayErr := s.runRelayTransfer(r.Context(), src, dst, pairs, req.Move, stream, task)
	if relayErr != nil {
		failure = "跨主机传输失败：" + relayErr.Error()
		stream.send(transferEvent{Type: "error", Message: failure}, false)
		return
	}
	stream.send(transferEvent{Type: "done", Done: len(pairs) - failed, Total: len(pairs), Failed: failed}, false)
}

// dropNestedPaths 去掉被另一个选中路径包含的条目，并去掉重复项。
//
// 只做字符串前缀比较，不碰文件系统：调用方已经确认这些路径都存在。
// 选 /a 与 /a/b 时把 /a/b 丢掉 —— 搬 /a 已经把它带过去了。
func dropNestedPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for i, candidate := range paths {
		if seen[candidate] {
			continue
		}
		seen[candidate] = true

		nested := false
		for j, other := range paths {
			if i == j || other == "/" {
				continue
			}
			if strings.HasPrefix(candidate, other+"/") {
				nested = true
				break
			}
		}
		if !nested {
			out = append(out, candidate)
		}
	}
	return out
}

// canDirectTransfer 判断源机能否直接 ssh 到目标机并把 tar 用起来。
//
// 探测失败（连不上源机、没有 ssh 命令）一律当作"不能直连"：这条路只是优化，
// 判断不出来就走一定可用的中转，不该让用户看到一个探测错误。
func (s *Server) canDirectTransfer(ctx context.Context, srcHostID int64, dstHost *store.Host) bool {
	// 宿主机的地址是面板自己，没有可用的 ssh 目标；也只能走中转。
	if dstHost.ID == LocalHostID {
		return false
	}
	res, err := s.execOnHost(ctx, srcHostID, directProbeCommand(dstHost), directProbeTimeout)
	if err != nil {
		return false
	}
	return res.ExitCode == 0
}

// directProbeCommand 是直连探测的命令。
//
// BatchMode=yes 保证"要口令就失败"而不是挂在交互提示上；StrictHostKeyChecking=yes
// 不接受未知 host key —— 只在管理员已经把信任配好时才走直连，不替用户做
// "首次连接自动信任"这种决定。
func directProbeCommand(dstHost *store.Host) string {
	return "command -v tar >/dev/null 2>&1 && command -v ssh >/dev/null 2>&1 && " +
		"ssh -n -o BatchMode=yes -o StrictHostKeyChecking=yes -o ConnectTimeout=8 -p " +
		strconv.Itoa(hostPort(dstHost)) + " " +
		shellQuotePath(dstHost.Username+"@"+dstHost.Addr) +
		" command -v tar >/dev/null 2>&1"
}

// directTransferCommand 是单个根条目的直连搬运命令。
//
// 用 tar 流而不是 scp/rsync：无需在源机判断目标机装没装 rsync，且目录天然递归。
// 打包在源条目的上级目录里做，解包直接落到目标目录 —— 两边都不需要额外的路径映射。
//
// 远端命令必须**整段再引一次**：ssh 会把参数用空格拼成一条远程命令交给远端 shell，
// 本地 shell 先剥掉一层引号，目标路径里带空格时就断成了两个参数。
func directTransferCommand(srcPath string, dstHost *store.Host, target string) string {
	return "tar -cf - -C " + shellQuotePath(path.Dir(srcPath)) +
		" -- " + shellQuotePath(path.Base(srcPath)) +
		" | ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -p " +
		strconv.Itoa(hostPort(dstHost)) + " " +
		shellQuotePath(dstHost.Username+"@"+dstHost.Addr) +
		" " + shellQuotePath(directRemoteExtract(target))
}

// directRemoteExtract 是交给远端 shell 的解包命令。
//
// 单独抽出来是为了能被测试直接拿去 `sh -c` 跑一遍：路径里的空格、单引号
// 一旦在"本地引号 → ssh 拼接 → 远端引号"这三层里的任何一层出问题，
// 表现都是"文件解到了别的目录"，而不是报错。
func directRemoteExtract(target string) string {
	return "tar -xf - -C " + shellQuotePath(target)
}

func hostPort(host *store.Host) int {
	if host.Port > 0 {
		return host.Port
	}
	return 22
}

// runDirectTransfer 逐个根条目走直连，返回已完成的根条目数。
//
// 返回值同时用于"回退时从哪里接上"：已完成的根条目可能已经把源删掉了（move），
// 回退再搬一次会直接失败。
func (s *Server) runDirectTransfer(
	ctx context.Context,
	src *sftpGuard,
	srcHostID int64,
	dstHost *store.Host,
	pairs []transferPair,
	target string,
	move bool,
	stream *transferStream,
	task *fileTask,
) (int, error) {
	for i, pair := range pairs {
		if ctx.Err() != nil {
			return i, ctx.Err()
		}

		res, err := s.execOnHostStream(ctx, srcHostID, directTransferCommand(pair.src, dstHost, target), directTransferTimeout, nil)
		if err != nil {
			return i, err
		}
		if res.ExitCode != 0 {
			detail := strings.TrimSpace(res.Stderr)
			if detail == "" {
				detail = strings.TrimSpace(res.Stdout)
			}
			if detail == "" {
				detail = fmt.Sprintf("命令以退出码 %d 结束", res.ExitCode)
			}
			return i, errors.New(tailOutput(detail))
		}

		if move {
			if err := removeTreeSFTP(src, pair.src); err != nil {
				// 拷贝已经成功、源没删掉：这次 move 没完成。回退会把目标上的
				// 副本清掉再走中转，源还在，因此不会丢数据。
				return i, errors.New("删除源端失败：" + err.Error())
			}
		}
		// 直连拿不到大小，记 -1（与哈希那条"拿不到大小"的口径一致）。
		task.addItem(pair.src, -1, "", "")
		stream.send(transferEvent{Type: "progress", Done: i + 1, Total: len(pairs), Path: path.Base(pair.src)}, false)
	}
	return len(pairs), nil
}

// relayEntry 是中转要复制的一个文件。
type relayEntry struct {
	root  int
	src   string
	dst   string
	size  int64
	mode  os.FileMode
	atime time.Time
	mtime time.Time
}

// relayDir 是中转要创建的一个目录，权限与时间戳在内容写完后再还原。
type relayDir struct {
	root  int
	dst   string
	mode  os.FileMode
	atime time.Time
	mtime time.Time
}

// relayLink 是一个按原样重建的符号链接。
type relayLink struct {
	root   int
	dst    string
	target string
}

// relayRoot 跟踪一个根条目在本次搬运里有没有失败。
//
// 移动的删除判据就是它：只要有一处没搬成功，源目录就不能删 ——
// 半套数据的移动比"没动"糟得多。
type relayRoot struct {
	src    string
	files  int
	failed int
}

func (s *Server) runRelayTransfer(
	ctx context.Context,
	src, dst *sftpGuard,
	pairs []transferPair,
	move bool,
	stream *transferStream,
	task *fileTask,
) (int, error) {
	roots, files, dirs, links, err := planRelayTransfer(ctx, src, pairs)
	if err != nil {
		return len(pairs), err
	}

	// 建目录必须在写文件之前：plan 的前序遍历天然保证父目录先于子目录。
	for _, dir := range dirs {
		if ctx.Err() != nil {
			return len(pairs), ctx.Err()
		}
		if mkErr := dst.MkdirAll(dir.dst); mkErr != nil {
			roots[dir.root].failed++
			task.addItem(dir.dst, 0, "", "创建目录失败："+mkErr.Error())
		}
	}

	for _, link := range links {
		if linkErr := dst.Symlink(link.target, link.dst); linkErr != nil {
			roots[link.root].failed++
			task.addItem(link.dst, 0, "", "创建符号链接失败："+linkErr.Error())
		}
	}

	progress := &relayProgress{stream: stream, total: len(files)}
	for _, file := range files {
		progress.bytesTotal += file.size
	}

	// 一次传输同时压着两条链路，任一条停滞都要能判死：两个客户端一起关，
	// 阻塞中的读写会立刻返回错误。
	guard := newStallGuard(sftpStallTimeout, func() {
		if src.client != nil {
			_ = src.client.Close()
		}
		if dst.client != nil {
			_ = dst.client.Close()
		}
	})
	defer guard.Release()

	for _, file := range files {
		if ctx.Err() != nil {
			return len(pairs), ctx.Err()
		}
		written, copyErr := copyFileBetween(src, dst, file.src, file.dst, guard)
		if copyErr != nil {
			roots[file.root].failed++
			task.addItem(file.src, file.size, "", copyErr.Error())
			progress.complete(file.dst, 0)
			continue
		}
		// 权限与时间戳尽力还原：这两步失败不该让已经传过去的内容作废，
		// 但要写在文件内容之后 —— 写入本身会刷新 mtime。
		_ = dst.Chmod(file.dst, permBits(file.mode))
		_ = dst.Chtimes(file.dst, file.atime, file.mtime)
		task.addItem(file.src, written, "", "")
		progress.complete(file.dst, written)
	}
	progress.flush()

	// 目录的时间戳必须最后还原：往里写文件会改掉它的 mtime。倒序保证
	// 子目录先于父目录落定。
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = dst.Chmod(dirs[i].dst, permBits(dirs[i].mode))
		_ = dst.Chtimes(dirs[i].dst, dirs[i].atime, dirs[i].mtime)
	}

	failed := 0
	for i, root := range roots {
		if root.failed > 0 {
			failed++
			continue
		}
		if !move {
			continue
		}
		if rmErr := removeTreeSFTP(src, pairs[i].src); rmErr != nil {
			failed++
			task.addItem(pairs[i].src, -1, "", "删除源端失败："+rmErr.Error())
		}
	}
	return failed, nil
}

// relayLister 是枚举阶段要用到的只读 SFTP 能力。
//
// 抽成接口只是为了让目录展开这段逻辑能被单测覆盖：递归、符号链接与失败容忍
// 都在这里，而它们一旦错了不会报错，只会少搬几个文件。
type relayLister interface {
	Lstat(p string) (os.FileInfo, error)
	ReadDir(p string) ([]os.FileInfo, error)
	ReadLink(p string) (string, error)
}

// planRelayTransfer 把源端树展开成"要建哪些目录、要复制哪些文件、要重建哪些链接"。
//
// 枚举与复制分成两趟：进度条的分母（文件数、总字节数）只有先走一遍才拿得到。
// 代价是目录树要读两次元数据，换来的是整个搬运过程有确定的百分比。
func planRelayTransfer(
	ctx context.Context,
	src relayLister,
	pairs []transferPair,
) ([]relayRoot, []relayEntry, []relayDir, []relayLink, error) {
	roots := make([]relayRoot, 0, len(pairs))
	files := make([]relayEntry, 0, len(pairs))
	dirs := make([]relayDir, 0, len(pairs))
	links := make([]relayLink, 0, len(pairs))
	enumErrors := 0

	for i, pair := range pairs {
		root := relayRoot{src: pair.src}

		var walk func(string, string) bool
		walk = func(srcPath, dstPath string) bool {
			if ctx.Err() != nil {
				return false
			}
			info, err := src.Lstat(srcPath)
			if err != nil {
				root.failed++
				enumErrors++
				return enumErrors < maxRelayEnumErrors
			}
			atime, mtime := entryTimes(info)

			switch {
			case info.Mode()&os.ModeSymlink != 0:
				// 链接按链接搬，不跟进去：跨主机跟随会得到一个指向目标机上
				// 不存在路径的普通文件，也可能被自指链接带进死循环。
				target, err := src.ReadLink(srcPath)
				if err != nil {
					root.failed++
					enumErrors++
					return enumErrors < maxRelayEnumErrors
				}
				links = append(links, relayLink{root: i, dst: dstPath, target: target})

			case info.IsDir():
				dirs = append(dirs, relayDir{root: i, dst: dstPath, mode: info.Mode(), atime: atime, mtime: mtime})
				entries, err := src.ReadDir(srcPath)
				if err != nil {
					root.failed++
					enumErrors++
					return enumErrors < maxRelayEnumErrors
				}
				for _, entry := range entries {
					if !walk(path.Join(srcPath, entry.Name()), path.Join(dstPath, entry.Name())) {
						return false
					}
				}

			default:
				files = append(files, relayEntry{
					root:  i,
					src:   srcPath,
					dst:   dstPath,
					size:  info.Size(),
					mode:  info.Mode(),
					atime: atime,
					mtime: mtime,
				})
				root.files++
			}
			return true
		}

		walk(pair.src, pair.dst)
		roots = append(roots, root)
	}

	if ctx.Err() != nil {
		return nil, nil, nil, nil, ctx.Err()
	}
	if enumErrors >= maxRelayEnumErrors {
		return nil, nil, nil, nil, errors.New("枚举源目录时连续失败，已中止（可能是连接中断或权限不足）")
	}
	return roots, files, dirs, links, nil
}

// permBits 只保留权限位与 setuid/setgid/sticky。
//
// 直接把 FileMode（含"这是目录"这类类型位）交给 Chmod 是不对的：那一位描述的是
// 文件类型，不属于 chmod 能改的东西；剥掉之后 setuid/setgid/sticky 仍能原样保留。
func permBits(mode os.FileMode) os.FileMode {
	return mode.Perm() | mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)
}

// entryTimes 取出条目真实的 atime / mtime。
//
// atime 只有 SFTP 扩展属性里有；拿不到时退回 mtime —— 界面与保留逻辑
// 都把它当成"不知道"，比一个 1970 年更接近事实。
func entryTimes(info os.FileInfo) (time.Time, time.Time) {
	atime := info.ModTime()
	if stat, ok := info.Sys().(*sftp.FileStat); ok && stat.Atime > 0 {
		atime = time.Unix(int64(stat.Atime), 0)
	}
	return atime, info.ModTime()
}

// copyFileBetween 把一个文件从源主机流到目标主机，不落盘。
func copyFileBetween(src, dst *sftpGuard, from, to string, guard *stallGuard) (int64, error) {
	in, err := src.Open(from)
	if err != nil {
		return 0, err
	}
	defer in.Close()

	out, err := dst.Create(to)
	if err != nil {
		return 0, err
	}

	// 自己写循环而不是 io.Copy：进度回调要按固定大小的块推进，
	// 而 io.Copy 会优先用底层类型自带的 ReadFrom/WriteTo，绕过我们的包装。
	buf := make([]byte, transferBufferSize)
	reader := progressReader{r: in, g: guard}
	writer := progressWriter{w: out, g: guard}
	var written int64
	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			sent, writeErr := writer.Write(buf[:n])
			written += int64(sent)
			if writeErr != nil {
				_ = out.Close()
				return written, writeErr
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = out.Close()
			return written, readErr
		}
	}

	if err := out.Close(); err != nil {
		return written, err
	}
	return written, nil
}

// relayProgress 把逐文件复制折算成进度帧。
type relayProgress struct {
	stream     *transferStream
	total      int
	bytesTotal int64
	done       int
	bytes      int64
	path       string
}

func (p *relayProgress) complete(path string, size int64) {
	p.done++
	p.bytes += size
	p.path = path
	p.stream.send(transferEvent{
		Type:       "progress",
		Done:       p.done,
		Total:      p.total,
		Bytes:      p.bytes,
		BytesTotal: p.bytesTotal,
		Path:       path,
	}, true)
}

// flush 在收尾时补发最后一帧：节流窗口内最后几个文件的结果不能被吞掉。
func (p *relayProgress) flush() {
	p.stream.send(transferEvent{
		Type:       "progress",
		Done:       p.done,
		Total:      p.total,
		Bytes:      p.bytes,
		BytesTotal: p.bytesTotal,
		Path:       p.path,
	}, false)
}

// removeTreeSFTP 递归删除一个远端路径。
//
// 文件管理里的"删除"只删空目录（不可逆操作不提供递归），但移动与回退清理
// 必须能整棵删：这两处要么有明确的后续动作（move 已在目标上留下完整副本），
// 要么删的是本次自己刚写出来的半套数据。
func removeTreeSFTP(g *sftpGuard, target string) error {
	info, err := g.Lstat(target)
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, sftp.ErrSSHFxNoSuchFile) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return g.Remove(target)
	}

	entries, err := g.ReadDir(target)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		child := path.Join(target, entry.Name())
		if err := removeTreeSFTP(g, child); err != nil {
			return err
		}
	}
	return g.RemoveDirectory(target)
}
