package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 文件扫描：列出选中的文件 / 目录里会被逐个处理的文件。
//
// 与"计算哈希"拆成两个请求，是因为二者的时机不同：用户打开哈希弹窗时就要看清
// "这次到底要算哪些文件"，而计算要等他确认算法之后才动手。代价是两个请求各遍历
// 一次目录，换来的是"动手之前先看清范围"——大目录上这一点尤其重要。
//
// 遍历交给目标机的 find：一次连接、一次遍历，比面板侧 SFTP 逐层列目录快得多。

// fileScanTimeout 给 find 留的时间。
//
// 它只列路径、不读文件内容，几万条也用不到这个数；给足是为了不误杀机械盘上、
// 或者挂载在网络存储上的大目录 —— 扫描超时的表现是"清单列不出来"，而那正是
// 这个功能唯一要做的事。等待期间界面可以关掉弹窗，届时连接断开会终止远端 find。
const fileScanTimeout = 5 * time.Minute

// scanBatchSize 是每帧携带的路径条数。
//
// 逐条下发会让几万文件的目录推几万帧，界面光处理帧就卡住；整批一帧则首批要等到
// 扫完才出现，用户看不到"它动起来了"。
const scanBatchSize = 200

// scanEvent 是扫描过程中下发给界面的一帧。
type scanEvent struct {
	// Type 取 entries / done / error。
	Type string `json:"type"`
	// Paths 是这一批文件的相对路径 —— 与哈希结果帧里的 path 同口径，
	// 界面靠它把"等待中"的行与随后到来的结果一一对上。
	Paths []string `json:"paths,omitempty"`

	// Sizes 与 Paths 一一对应，-1 表示这条路拿不到大小。
	//
	// 没有 omitempty：夹带路径的那两帧必须把大小一起送到，缺了它大小列整列是"—"。
	// 一个空数组只多两个字节，而"字段被省略"与"大小未知"在界面上是同一种结果 ——
	// 让它们无法区分，就是为了省两个字节而埋一个查不出来的坑。
	Sizes []int64 `json:"sizes"`
	// Total 是已列出的文件总数。
	Total int64 `json:"total,omitempty"`
	// Message 是整批失败的原因。
	Message string `json:"message,omitempty"`
}

// scanBatches 把路径切成若干批。
//
// 只看长度，不看内容：分批的耗时都花在"每条路径发一趟 stat"上，与路径本身有多长无关，
// 所以长度就是这批有多重的度量。
func scanBatches(paths []string, size int) [][]string {
	if size <= 0 {
		size = scanBatchSize
	}
	var out [][]string
	for len(paths) > 0 {
		n := size
		if n > len(paths) {
			n = len(paths)
		}
		out = append(out, paths[:n])
		paths = paths[n:]
	}
	return out
}

// scanSizesCommand 让目标机列出每条路径的字节数。
//
// 大小必须由目标机来报：面板侧要拿到它只有逐条 SFTP stat 一条路 ——
// 与"路径由一次 find 列出来"的做法自相矛盾（那正是为了避免几千次往返才改的）。
//
// 命令形态必须是"一行一条、只看有输出的行"，把逐条独立的语句用换行串起来；
// 三个都要注意的地方：
//   - 不能把路径交给 find 的 -printf：那条路必须写成 "find <路径>"，而路径一旦可能是
//     选项（`-weird` 之类）就会被当成谓词解析；`--` 也不管用，GNU find 不认它，
//     整条命令会以"未知的谓词"退出、一条都报不出来。stat 同样是这个道理。
//   - 不能改成"一次带上多条路径"的 wc：它会在末尾多打一行"总计"，还会给每条路径
//     垫上对齐空格。多出来的那行一旦混进结果，就成了一个名叫"总计"的假文件 ——
//     它不会是任何一条已知路径，所以只是白解析一行，但把这种行留给解析器去挡，
//     不如从命令上就不让它出现。
//   - 不能因为单条路径失败就整条命令失败。文件可以在 find 列出它之后、问大小的这一瞬
//     被删掉，也可能没有 stat 权限；那都不是整批的失败，能报几条算几条。
//
// 逐条独立执行天然满足后两点：每条自己成败，且每条只有一行输出。代价是每条一次
// 进程启动 —— 一批 200 条就是 200 次 fork，只有在目标机内核与内核之间挂着网络文件
// 系统时才看得出来。真实场景里这里连着的是 ssh 连接，不是文件系统。
//
// 用 stat -c 而不是 wc -c：两者都认路径在选项之后（"stat -c %s <路径>"，
// 与手工敲的那条一模一样），但 stat 报的是目录项的字节数，与 find/stat 的 Size()
// 同一口径，而 wc -c 要真的把文件读一遍 —— 几百 MB 的一个文件，为了知道它多大
// 就要多读几百 MB。拿不到大小的路径由它自己把错误写进 stderr，我们只收 stdout。
func scanSizesCommand(paths []string) string {
	lines := make([]string, 0, len(paths))
	for _, p := range paths {
		lines = append(lines, "stat -c %s "+shellQuotePath(p))
	}
	return strings.Join(lines, "\n")
}

// resolveScanSizes 逐条问出这批路径的大小；拿不到的一律记 -1（界面显示"—"）。
//
// 结果按输入顺序返回：界面靠下标把大小配到对应的路径上，顺序错了就会张冠李戴。
//
// 输出之所以不带路径，是因为带路径的形态都得额外防三件事（路径被当成选项、
// wc 多打"总计"、个别文件失败带崩整条命令），而"第几条语句"就足以定位 ——
// 每条 stat 要么打印一行数字，要么什么都不打印，顺序与入参严格一致。
func (s *Server) resolveScanSizes(ctx context.Context, hostID int64, paths []string) []int64 {
	sizes := make([]int64, len(paths))
	for i := range sizes {
		sizes[i] = -1
	}
	if len(paths) == 0 {
		return sizes
	}

	var mu sync.Mutex
	seen := 0
	record := func(line string) {
		text := strings.TrimSpace(line)
		// 报错走 stderr、空行是没写成功的语句（它的错在前面一行输出里），都跳过；
		// 跳过不等于这条就没了 —— 数字一定紧挨着它的语句，按顺序取下一个即可。
		if text == "" {
			return
		}
		size, err := strconv.ParseInt(text, 10, 64)
		if err != nil || size < 0 {
			return
		}
		mu.Lock()
		if seen < len(sizes) {
			sizes[seen] = size
			seen++
		}
		mu.Unlock()
	}

	res, err := s.execOnHostStream(ctx, hostID, scanSizesCommand(paths), fileScanTimeout, record)
	// err 非 nil 时输出可能已经收到一半：下面照样用它们 —— 有大小好过整列"—"。
	// 只把结果整段再解析一遍补上流式回调漏掉的部分（最后一行没有换行符时回调拿不到）。
	if res != nil {
		for _, line := range strings.Split(res.Stdout, "\n") {
			record(line)
		}
	}
	_ = err
	return sizes
}

// parseSizeLine 解析 "<字节数><一个分隔符><路径>"，仅供知道"输出里带路径"的地方使用。
//
// scanSizesCommand 的输出里只有裸数字，走不到这儿；它是留给"哪天命令又改回带路径的形态"
// 的那条路 —— 那时要认的两种分隔符都得在：制表符（路径里可以有空格）与空格（路径前会
// 有对齐空格，字节数一定是纯数字，从左边切一刀就够）。
//
// 认不出的行返回 false，调用方跳过 —— 命令的报错就由这一条挡在外面。
func parseSizeLine(text string) (int64, string, bool) {
	line := strings.TrimRight(text, "\r")
	sep := strings.IndexAny(line, "\t ")
	if sep <= 0 {
		return 0, "", false
	}
	sizeText, name := line[:sep], line[sep+1:]
	// wc 在路径前会多留一些空格对齐；剥掉它们，剩下的就是原样的路径。
	name = strings.TrimLeft(name, " ")
	size, err := strconv.ParseInt(sizeText, 10, 64)
	if err != nil || size < 0 || name == "" {
		return 0, "", false
	}
	return size, name, true
}

func (s *Server) handleFileScan(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req hashRequest
	if err := decodeJSONBody(w, r, &req, 1<<20); err != nil {
		return
	}
	if len(req.Paths) == 0 {
		writeErr(w, http.StatusBadRequest, "未选择要处理的条目")
		return
	}

	roots := make([]hashRoot, 0, len(req.Paths))
	for _, raw := range req.Paths {
		target, ok := requireAbsPath(w, raw)
		if !ok {
			return
		}
		roots = append(roots, hashRoot{path: target})
	}

	sess, client, ok := s.openSFTP(w, hostID)
	if !ok {
		return
	}
	defer sess.Close()

	if err := classifyHashRoots(client, roots); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}

	// 从这里开始下发帧，此后一律 200，失败改由 error 帧表达。
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}

	send := func(ev scanEvent) {
		data, err := json.Marshal(ev)
		if err != nil {
			return
		}
		_, _ = w.Write(append(data, '\n'))
		if flusher != nil {
			flusher.Flush()
		}
	}

	// 回调来自两条拷贝协程（stdout / stderr），攒批与计数共用一把锁。
	var mu sync.Mutex
	var batch []string
	var total int64

	// 路径按批下发，每批临下发前问一次大小 —— find 只给路径，而大小要另外问。
	// 跟着这一批现问而不是等 find 跑完再统一问：这里是顺序执行，统一问会让界面
	// 在扫描结束后再空等一段时间（几万文件的目录上要好几秒），而那时进度条已经走满，
	// 看起来就是"卡住了"。
	flush := func(chunk []string, done int64) {
		send(scanEvent{
			Type:  "entries",
			Paths: chunk,
			Sizes: s.resolveScanSizes(r.Context(), hostID, chunk),
			Total: done,
		})
	}

	collect := func(text string) {
		line := strings.TrimRight(text, "\r")
		// find 只会输出命令行上给出的路径，或它下面的路径。不属于任何根的行因此不是
		// 文件：find 自己的报错、会话层的提示（以及任何我们没预料到的噪音）都在这里
		// 被挡掉 —— 放过去的话，清单里会多出一个名字是那句话的"文件"，用户还会以为
		// 它真的存在于目标机上。
		rel, ok := relativeUnder(roots, line)
		if !ok {
			return
		}

		mu.Lock()
		batch = append(batch, rel)
		total++
		if len(batch) < scanBatchSize {
			mu.Unlock()
			return
		}
		chunk := batch
		batch = nil
		done := total
		mu.Unlock()

		flush(chunk, done)
	}

	script := "find -H " + quotedRoots(roots) + " -type f"
	res, err := s.execOnHostStream(r.Context(), hostID, script, fileScanTimeout, collect)

	mu.Lock()
	rest := batch
	batch = nil
	totalFinal := total
	mu.Unlock()

	if len(rest) > 0 {
		flush(rest, totalFinal)
	}

	switch {
	case err != nil:
		send(scanEvent{Type: "error", Message: "列出文件失败：" + err.Error()})
	case totalFinal == 0 && res.ExitCode != 0:
		// 一条都没列出来才值得报错。只列出一部分（权限所限，find 会边报错边继续）
		// 与哈希计算的取舍一致：少几个文件好过整批作废。
		detail := tailOutput(strings.TrimSpace(res.Stderr))
		if detail == "" {
			detail = fmt.Sprintf("命令以退出码 %d 结束", res.ExitCode)
		}
		send(scanEvent{Type: "error", Message: "列出文件失败：" + detail})
	default:
		send(scanEvent{Type: "done", Total: totalFinal})
	}
}
