package api

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"lukechampine.com/blake3"

	"panelssh/internal/store"
)

// 文件哈希：对选中的文件或目录计算摘要，支持 SHA-256 / SHA-512 / SHA-1 / MD5 / BLAKE3。
//
// 两条实现路径，按目标机的能力分流：
//
//   - 目标机装了对应的摘要命令（sha256sum 等，由 coreutils / busybox 提供）时交给它算。
//     文件字节不必跨网络走一遍面板，大目录上的差别是数量级的。
//   - 目标机没有该命令时（b3sum 基本不会装），面板自己把文件读过来算。
//     五种算法都实现了这一路，因此"某个算法走哪条路"只影响速度，不影响结果与用法。
//
// 结果用 NDJSON 逐帧下发，契约与压缩/解压完全一致（见 runArchiveStream 的说明）：
// 一旦开始下发就恒为 200，真正的失败放在 error 帧里。

// hashTimeout 是算哈希时给目标机命令的时间上限。
//
// 不能沿用 fileEscalateTimeout（2 分钟）——那个数是给"拷贝一个文件"定的。哈希要把
// 文件的每一个字节都读一遍，它的耗时正比于数据总量：一个几百 GB 的数据集在机械盘上
// 要跑十几分钟，2 分钟的超时表现成"一个结果都没有，只留一句执行超时"。
//
// 面板侧那条路（BLAKE3 等）不受这个上限约束：它按文件逐个处理，每个文件一次 SFTP 操作，
// 各自有自己的超时，读得慢只会慢，不会整体作废。
const hashTimeout = 30 * time.Minute

// hashAlgo 是一种摘要算法在两条路径上的实现。
type hashAlgo struct {
	// Key 是接口上用的名字。
	Key string
	// Command 是目标机上的命令名，也用于探测目标机有没有这个算法。
	Command string
	// HexLen 是该算法十六进制摘要的长度，解析命令行输出时用它定位分隔符。
	HexLen int
	// New 是面板侧计算时用的实现。
	New func() hash.Hash
}

// hashAlgos 是全部支持的算法，顺序即界面上的展示顺序。
//
// BLAKE3 用纯 Go 实现而不是目标机的 b3sum：后者几乎不会预装，
// 而"用户选了却被告知没装"是最没必要的一种失败。
var hashAlgos = []hashAlgo{
	{Key: "sha256", Command: "sha256sum", HexLen: 64, New: sha256.New},
	{Key: "sha512", Command: "sha512sum", HexLen: 128, New: sha512.New},
	{Key: "sha1", Command: "sha1sum", HexLen: 40, New: sha1.New},
	{Key: "md5", Command: "md5sum", HexLen: 32, New: md5.New},
	// 32 字节输出，与 b3sum 的默认口径一致。
	{Key: "blake3", Command: "b3sum", HexLen: 64, New: func() hash.Hash { return blake3.New(32, nil) }},
}

func findHashAlgo(key string) (hashAlgo, bool) {
	for _, algo := range hashAlgos {
		if algo.Key == key {
			return algo, true
		}
	}
	return hashAlgo{}, false
}

type hashRequest struct {
	Paths []string `json:"paths"`
	Algo  string   `json:"algo"`
}

// hashEvent 是哈希过程中下发给界面的一帧。
type hashEvent struct {
	// Type 取 prepare / item / bytes / progress / result / escalating / done / error。
	//
	// item 与 bytes 是"这个文件读到哪儿了"：摘要工具要读完整个文件才给结果，
	// 一个几十 GB 的文件会让结果帧长时间不来，界面只能干等。它们让等待期间有东西可看。
	Type string `json:"type"`
	// Total 是待计算的文件总数。为 0 表示还没数出来，界面按不确定进度渲染。
	Total int64 `json:"total,omitempty"`
	// Done 是已出结果的文件数。
	Done int64 `json:"done,omitempty"`
	// Path 是相对选中根的路径 —— 这个形式正是 sha256sum -c 认得的，
	// 导出的清单可以拿到目标机上原样校验。
	Path string `json:"path,omitempty"`
	// Size 是文件字节数。用指针是为了区分"确实是 0 字节"与"这条路拿不到大小"：
	// 走目标机命令时 sha256sum 不输出大小，界面据此显示"—"而不是一个假的 0。
	Size *int64 `json:"size,omitempty"`
	// Bytes / BytesTotal 只出现在 bytes 帧里：当前这个文件已读多少 / 共多少字节。
	// 与 Done/Total 分开命名，避免与"已完成的文件数"混为一谈。
	Bytes      int64 `json:"bytes,omitempty"`
	BytesTotal int64 `json:"bytesTotal,omitempty"`
	// Hash 是十六进制摘要。
	Hash string `json:"hash,omitempty"`
	// Error 是这一个文件的失败原因；整批失败走 Message。
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
}

// hashRoot 是一个选中的根：文件直接算，目录递归。
type hashRoot struct {
	path  string
	isDir bool
}

// hashTarget 是面板侧要算的一个文件。
type hashTarget struct {
	abs  string
	rel  string
	size int64
}

// isHexDigit 判断一个字节是不是十六进制字符。
func isHexDigit(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

// 目标机脚本与面板之间的两个进度标记。
//
// 摘要在读完整个文件之前不会输出任何东西，所以"读了多少"必须由脚本另外报。
// 两者都排在摘要行之外，认不出的行一律忽略 —— 与压缩/解压那边的协议同一种做法。
const (
	hashItemMarker  = "PANELSSH_HASH_ITEM"
	hashBytesMarker = "PANELSSH_HASH_PROGRESS"
)

// hashProgressMinBytes 是"值得盯进度"的文件大小门槛。
//
// 小文件一瞬间就算完了，为它们起一个轮询进程、再推两帧进度，只会白花开销；
// 而大文件正是那个"看起来像卡死"的场景。门槛以下走与以前完全相同的路径。
const hashProgressMinBytes = 32 << 20

// parseHashItem 解析 "PANELSSH_HASH_ITEM <字节数> <路径>"。
//
// 只按**第一个**空格切分：路径里可以有空格，而字节数一定是纯数字，
// 所以从左边切一刀就够，右边的整段都是路径。
func parseHashItem(text string) (int64, string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(text), hashItemMarker)
	if !ok {
		return 0, "", false
	}
	rest = strings.TrimSpace(rest)
	sizeText, path, ok := strings.Cut(rest, " ")
	if !ok {
		return 0, "", false
	}
	size, err := strconv.ParseInt(sizeText, 10, 64)
	if err != nil || size < 0 {
		return 0, "", false
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return 0, "", false
	}
	return size, path, true
}

// parseHashBytes 解析 "PANELSSH_HASH_PROGRESS <已读字节> <总字节>"。
//
// 取不到读位置时脚本会把这一行整行省掉，但要防住它报来一个空字段 ——
// 空的字节数被当成 0 会让进度条突然跳回 0，比不显示更难看。
func parseHashBytes(text string) (int64, int64, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(text), hashBytesMarker)
	if !ok {
		return 0, 0, false
	}
	fields := strings.Fields(rest)
	if len(fields) != 2 {
		return 0, 0, false
	}
	done, err1 := strconv.ParseInt(fields[0], 10, 64)
	total, err2 := strconv.ParseInt(fields[1], 10, 64)
	if err1 != nil || err2 != nil || done < 0 || total <= 0 {
		return 0, 0, false
	}
	if done > total {
		done = total
	}
	return done, total, true
}

// parseHashLine 解析 sha256sum 家族的一行输出：<摘要><两个空格><路径>。
//
// 按固定长度切分而不是"找第一个双空格"：路径里出现连续空格时后者会切错，
// 而摘要在同一种算法下长度是固定的，这个信息是免费的。
//
// 摘要位上的十六进制校验不能省：sha256sum 读不了某个文件时会把
// "sha256sum: /path: Permission denied" 写在 stderr 上，而这里两条流是混在一起的，
// 不校验就会把错误信息的前 64 个字符当成一个摘要。
func parseHashLine(text string, hexLen int) (string, string, bool) {
	line := strings.TrimRight(text, "\r")
	if len(line) < hexLen+2 {
		return "", "", false
	}
	digest := line[:hexLen]
	for i := 0; i < hexLen; i++ {
		if !isHexDigit(digest[i]) {
			return "", "", false
		}
	}
	if line[hexLen] != ' ' {
		return "", "", false
	}
	// 摘要与路径之间隔两个字符：默认是"两个空格"，coreutils 在二进制模式下
	// 写成"空格加星号"。两者之外都不认。
	sep := line[hexLen+1]
	if sep != ' ' && sep != '*' {
		return "", "", false
	}
	name := line[hexLen+2:]
	if name == "" {
		return "", "", false
	}
	return digest, name, true
}

// classifyHashRoots 标注每个根"是不是目录"，同时借此校验路径存在。
//
// 用 Stat（跟随链接）而不是 Lstat：与脚本里的 find -H 口径一致 —— 用户选中一个
// 指向目录的链接时，意图显然是它指向的那个目录。
//
// 报错里带上具体是哪一个路径：一次可以选多个条目，只说"对象不存在"等于没说。
func classifyHashRoots(client *sftpGuard, roots []hashRoot) error {
	for i := range roots {
		info, err := client.Stat(roots[i].path)
		if err != nil {
			return fmt.Errorf("对象不存在：%s", roots[i].path)
		}
		roots[i].isDir = info.IsDir()
	}
	return nil
}

// relativeUnder 把绝对路径换算成相对某个选中根的路径。
//
// 返回的形态与 sha256sum -c 的清单一致：根是目录时剥掉前缀，根是文件时取文件名。
// 找不到归属时返回 false，调用方退回绝对路径 —— 显示一个绝对路径总好过显示空白。
func relativeUnder(roots []hashRoot, abs string) (string, bool) {
	for _, root := range roots {
		if !root.isDir {
			if abs == root.path {
				return path.Base(root.path), true
			}
			continue
		}
		// 根是 "/" 时 TrimSuffix 后前缀就是 "/"，不会拼出 "//"。
		prefix := strings.TrimSuffix(root.path, "/") + "/"
		if rest, ok := strings.CutPrefix(abs, prefix); ok && rest != "" {
			return rest, true
		}
	}
	return "", false
}

// quotedRoots 把根路径拼成命令参数。
func quotedRoots(roots []hashRoot) string {
	parts := make([]string, 0, len(roots))
	for _, root := range roots {
		parts = append(parts, shellQuotePath(root.path))
	}
	return strings.Join(parts, " ")
}

// hashScript 生成在目标机上算哈希的脚本。
//
// 复用 archiveScript 的骨架：同样是"先数出分母、再逐条输出"，于是解析与进度
// 两条链路都能直接沿用 —— 这两个功能的进度口径本来就该一致。
//
// find 加 -H 而不是 -L：-H 只跟随命令行上给出的链接（用户选中的是一个指向目录的
// 链接时，意图显然是算它指向的那个目录），而树内部的链接不跟随，既与面板侧
// 遍历的口径一致，也不会被自指的链接带进死循环。
//
// 逐条计算的命令不再直接交给摘要工具，而是包一层 sh（见 hashPerFileScript）：
// 摘要工具不报"读了多少"，只有包一层才能在读大文件时给出进度。
func hashScript(roots []hashRoot, command string) string {
	list := quotedRoots(roots)
	find := "find -H " + list + " -type f"
	exec := find + " -exec sh -c " + shellQuotePath(hashPerFileScript(command)) + " panelssh {} +"
	return archiveScript(find, exec)
}

// hashPerFileScript 是 find 对每个文件执行的那段 sh 脚本。
//
// 为什么要包这一层：摘要工具在读完整个文件之前不输出任何东西，所以几十 GB 的文件
// 会让界面在"已完成 0"上停很久，用户分不出"在慢慢读"和"早就断了"。这里用两帧标记
// 把"正在读哪个文件、读到哪个字节"报回面板，摘要行本身仍是工具的原生输出 ——
// **不去碰读取与计算本身**：分块拼摘要之类改法一旦读错一个字节，报出去的就是错摘要。
//
// 进度靠观察摘要进程自己的文件偏移（/proc/<pid>/fdinfo 里的 pos）得到：不额外读一遍
// 数据、也不影响它的输出。拿不到 /proc 时这一行就整行省掉，界面退回"只有已用时长"。
//
// 小文件（不超过 hashProgressMinBytes）走的就是原来那条路径，不多起任何进程 ——
// 一个几万个小文件的目录不该为进度多付几千次 fork。
func hashPerFileScript(command string) string {
	cmd := shellQuotePath(command)
	return "hash_one() {\n" +
		"  f=$1\n" +
		"  size=$(wc -c <\"$f\" 2>/dev/null) || size=0\n" +
		"  if [ \"$size\" -le " + strconv.Itoa(hashProgressMinBytes) + " ]; then\n" +
		"    " + cmd + " \"$f\"\n" +
		"    return $?\n" +
		"  fi\n" +
		"  printf '" + hashItemMarker + " %s %s\\n' \"$size\" \"$f\" >&2\n" +
		"  " + cmd + " \"$f\" &\n" +
		"  digest_pid=$!\n" +
		"  (\n" +
		"    while :; do\n" +
		// 取所有 fd 里最大的 pos：摘要工具持有的是文件，其余几个（管道、终端）的偏移量
		// 要么是 0，要么远小于它。
		"      pos=$(grep -h '^pos:' /proc/\"$digest_pid\"/fdinfo/* 2>/dev/null | awk '{ if ($2 + 0 > m) m = $2 + 0 } END { print m + 0 }')\n" +
		"      [ -n \"$pos\" ] && printf '" + hashBytesMarker + " %s %s\\n' \"$pos\" \"$size\" >&2\n" +
		"      sleep 1\n" +
		"    done\n" +
		"  ) &\n" +
		"  watcher_pid=$!\n" +
		"  wait \"$digest_pid\"\n" +
		"  status=$?\n" +
		"  kill \"$watcher_pid\" 2>/dev/null\n" +
		"  wait \"$watcher_pid\" 2>/dev/null\n" +
		"  return $status\n" +
		"}\n" +
		"for f do hash_one \"$f\"; done"
}

// hashProgress 把执行过程中的输出折算成结果帧与进度帧。
type hashProgress struct {
	// mu 同时保护计数与响应写入：回调来自两条拷贝协程（stdout / stderr），
	// 并发写同一份响应是错的。
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
	last    time.Time
	algo    hashAlgo
	roots   []hashRoot
	total   int64
	done    int64
	// task 是这次操作的任务记录：每一条结果同时是明细表里的一行。
	task *fileTask
	// itemRel 是最近一次宣告"开始处理"的那个文件（相对路径）。进度帧里只有字节数，
	// 要对上界面里的哪一行得靠它 —— 而且它能顺手把"计算中"的标记打在正确的行上。
	itemRel string
	// errMsg 是整批失败的原因（fail 时记下）。收尾时据此落库 —— 界面上的
	// 错误帧与历史记录里的结论必须是同一个说法。
	errMsg string
}

// line 是 execOnHostStream 的输出回调，也用于重放提权那次执行的完整输出。
func (p *hashProgress) line(text string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if n, ok := parseArchiveTotal(text); ok {
		p.total = n
		p.send(hashEvent{Type: "progress", Total: n}, false)
		return
	}

	// 两个进度标记必须排在摘要解析之前：它们的正文里也可能出现十六进制串，
	// 让它们落到下面那条路上只会污染结果。
	if size, abs, ok := parseHashItem(text); ok {
		p.beginItem(abs, size)
		return
	}
	if done, total, ok := parseHashBytes(text); ok {
		p.bytesAt(done, total)
		return
	}

	digest, abs, ok := parseHashLine(text, p.algo.HexLen)
	if !ok {
		// 认不出的行（警告、读文件失败的报错）不计数：把它们算成"已完成一个"
		// 会让进度走到 100% 却少几条结果，比停在 99% 更让人困惑。
		// 少掉的那些由 done 帧的 total/done 差值如实告诉用户。
		return
	}
	rel, ok := relativeUnder(p.roots, abs)
	if !ok {
		rel = abs
	}
	p.done++
	// 走目标机命令这条路上 sha256sum 不输出大小，明细里记 -1（界面显示"—"）。
	p.task.addItem(rel, -1, digest, "")
	p.send(hashEvent{Type: "result", Path: rel, Hash: digest}, false)
	p.send(hashEvent{Type: "progress", Total: p.total, Done: p.done}, true)
}

// beginItem 记下"开始处理这个文件"并立即下发一帧；调用方必须已持锁。
//
// 路径要先换算成与结果帧同口径的相对路径：界面靠路径把进度贴到正确的行上，
// 两条路各报一种形式的话，贴上去的就是另一行。
func (p *hashProgress) beginItem(abs string, size int64) {
	rel, ok := relativeUnder(p.roots, abs)
	if !ok {
		rel = abs
	}
	p.itemRel = rel
	b := size
	p.send(hashEvent{Type: "item", Path: rel, Size: &b}, false)
}

// bytesAt 下发"这个文件读到哪儿了"；调用方必须已持锁。
//
// 节流与进度帧一致：目标机每秒报一次，面板不必再往浏览器推更多。
func (p *hashProgress) bytesAt(done, total int64) {
	p.send(hashEvent{Type: "bytes", Path: p.itemRel, Bytes: done, BytesTotal: total}, true)
}

// item 是面板侧那条路的"开始处理"通知（它拿到的已经是相对路径）。
func (p *hashProgress) item(rel string, size int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.itemRel = rel
	if size < 0 {
		p.send(hashEvent{Type: "item", Path: rel}, false)
		return
	}
	b := size
	p.send(hashEvent{Type: "item", Path: rel, Size: &b}, false)
}

// bytes 是面板侧那条路的字节进度。
func (p *hashProgress) bytes(done, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bytesAt(done, total)
}

// result 下发一条面板侧算出的结果；调用方必须已持锁或处于单协程阶段。
func (p *hashProgress) result(ev hashEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done++
	p.task.addItem(ev.Path, sizeOf(ev.Size), ev.Hash, ev.Error)
	p.send(ev, false)
	p.send(hashEvent{Type: "progress", Total: p.total, Done: p.done}, true)
}

// sizeOf 把可缺省的大小折算成明细里的取值：-1 表示这条路拿不到大小。
func sizeOf(size *int64) int64 {
	if size == nil {
		return -1
	}
	return *size
}

// setTotal 记下分母并立即下发一帧（不节流）：这是"还要等多久"的起点。
func (p *hashProgress) setTotal(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.total = n
	p.send(hashEvent{Type: "progress", Total: n, Done: p.done}, false)
}

// completed 返回已出结果的数量，调用方据此判断"这一趟有没有拿到东西"。
func (p *hashProgress) completed() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done
}

// notify 下发一帧不该被节流的帧（prepare / escalating / error）。
func (p *hashProgress) notify(ev hashEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.send(ev, false)
}

// fail 下发错误帧并结束这一趟。
func (p *hashProgress) fail(message string) {
	p.mu.Lock()
	p.errMsg = message
	p.mu.Unlock()
	p.notify(hashEvent{Type: "error", Message: message})
}

// failure 返回整批失败的原因；空字符串表示这趟跑完了。
func (p *hashProgress) failure() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.errMsg
}

// finish 下发 done 帧。
//
// 带上 total 与 done：两者不等时说明有文件没算出结果（读不了、已损坏），
// 界面据此给出提示 —— 只报"完成"会让用户以为清单是完整的。
func (p *hashProgress) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.send(hashEvent{Type: "done", Total: p.total, Done: p.done}, false)
}

// send 写一帧并立即冲刷；调用方必须已持锁。throttle 只对 progress 帧为真。
func (p *hashProgress) send(ev hashEvent, throttle bool) {
	if throttle && time.Since(p.last) < archiveProgressInterval {
		return
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	p.last = time.Now()
	_, _ = p.w.Write(append(data, '\n'))
	if p.flusher != nil {
		p.flusher.Flush()
	}
}

func (s *Server) handleHash(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req hashRequest
	if err := decodeJSONBody(w, r, &req, 1<<20); err != nil {
		return
	}
	if len(req.Paths) == 0 {
		writeErr(w, http.StatusBadRequest, "未选择要计算哈希的条目")
		return
	}
	algo, ok := findHashAlgo(req.Algo)
	if !ok {
		writeErr(w, http.StatusBadRequest, "不支持的哈希算法："+req.Algo)
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

	// 分类用 Stat（跟随链接）而不是 Lstat：与下面 find -H 的口径一致。
	// 这一步同时起到"路径必须存在"的校验作用 —— 提前报错好过让用户对着
	// 一条 find 的原始报错猜是哪里不对。
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

	// 任务在开始之前登记：一个几万文件的目录要算很久，用户中途关掉窗口时，
	// 已经算完的那部分清单必须留得下来（哈希的明细是逐条入库的）。
	task := s.beginFileTask(hostID, store.TaskKindHash,
		fmt.Sprintf("计算哈希（%s） %s", strings.ToUpper(algo.Key), summarizePaths(req.Paths)), algo.Key)

	progress := &hashProgress{w: w, flusher: flusher, algo: algo, roots: roots, task: task}
	// 收尾放在这里而不是散在各条返回路径上：整批失败的原因记在 progress 里，
	// 界面上的错误帧与历史记录里的结论因此不会各说各话。
	defer func() {
		switch {
		case r.Context().Err() != nil:
			// 客户端先走了：这次操作对用户已经没有意义，记成中断而不是成功。
			task.abort()
		case progress.failure() != "":
			task.finish(progress.failure())
		default:
			task.finish("")
		}
	}()

	progress.notify(hashEvent{Type: "prepare"})

	// 探测失败（连不上等）不能断言"目标机没装"，那种情况直接走面板侧 ——
	// 面板侧不依赖目标机装任何东西，是唯一一定有结果的一条路。
	missing, probeErr := s.missingCommand(r.Context(), hostID, algo.Command)
	if probeErr == nil && !missing {
		s.hashViaCommand(r.Context(), hostID, roots, algo, progress)
		return
	}
	s.hashViaPanel(r.Context(), client, roots, algo, progress)
}

// hashViaCommand 让目标机自己算哈希。
func (s *Server) hashViaCommand(ctx context.Context, hostID int64, roots []hashRoot, algo hashAlgo, p *hashProgress) {
	script := hashScript(roots, algo.Command)

	res, err := s.execOnHostStream(ctx, hostID, script, hashTimeout, p.line)
	if err != nil {
		p.fail("计算哈希失败：" + err.Error())
		return
	}
	// 拿到结果就按完成收场：个别文件读不了（权限、已损坏）不该让整批作废，
	// 少掉的那些由 done 帧的 total/done 差值如实告诉用户。
	if res.ExitCode == 0 || p.completed() > 0 {
		p.finish()
		return
	}

	// 一个结果都没有才值得判断是不是权限问题：find 读不了目录时会整体失败，
	// 这时改用 root 重做一次是唯一能拿到结果的途径。
	rawDetail := strings.TrimSpace(res.Stderr)
	if rawDetail == "" {
		rawDetail = strings.TrimSpace(res.Stdout)
	}
	if !isPermissionDeniedText(rawDetail) {
		detail := tailOutput(rawDetail)
		if detail == "" {
			detail = fmt.Sprintf("命令以退出码 %d 结束", res.ExitCode)
		}
		p.fail("计算哈希失败：" + detail)
		return
	}

	// 提权通道拿不到逐条输出，只能整段跑完再解析。结果一样是完整的，
	// 只是等待期间进度条退回不确定状态 —— 与压缩/解压的处理保持一致。
	p.notify(hashEvent{Type: "escalating"})
	out, err := s.runEscalated(ctx, hostID, script, hashTimeout, escalateRequired)
	if err != nil {
		p.fail("计算哈希失败：" + err.Error())
		return
	}
	// 重放的是完整输出，不会与前面的结果重复：能走到这里说明一条结果都没出过。
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		p.line(line)
	}
	p.finish()
}

// hashViaPanel 由面板读文件算哈希。
//
// 只在目标机没有对应命令时使用（BLAKE3 的常态）。代价是文件字节要过一遍面板，
// 换来的是"任何目标机、任何算法都有结果"，不必要求用户去装东西。
//
// 这条路走的是 SFTP，用的是登录用户的权限 —— 文件管理模块不支持提权（见
// openSFTP 的说明），因此读不了的文件只能如实标在那一行上，不会整批失败。
func (s *Server) hashViaPanel(ctx context.Context, client *sftpGuard, roots []hashRoot, algo hashAlgo, p *hashProgress) {
	targets, err := collectHashTargets(client, roots)
	if err != nil {
		p.fail("扫描目录失败：" + hashScanError(err))
		return
	}
	p.setTotal(int64(len(targets)))

	for _, item := range targets {
		if ctx.Err() != nil {
			// 客户端已经走了，继续读文件只是白白占着目标机的带宽。
			return
		}
		// 先宣告"开始处理这个文件"：这条路要自己把字节读过来，进度随手就能报，
		// 但界面得先知道"现在算的是哪一个"。
		p.item(item.rel, item.size)
		digest, err := hashOneFile(client, item, algo, func(done int64) {
			p.bytes(done, item.size)
		})
		if err != nil {
			size := item.size
			p.result(hashEvent{
				Type:  "result",
				Path:  item.rel,
				Size:  &size,
				Error: hashReadError(err),
			})
			continue
		}
		size := item.size
		p.result(hashEvent{Type: "result", Path: item.rel, Size: &size, Hash: digest})
	}
	p.finish()
}

// hashScanError 把遍历失败翻译成用户能懂的话。
func hashScanError(err error) string {
	if isPermissionDenied(err) {
		return "读取目录被拒绝（权限不足）"
	}
	return err.Error()
}

// hashReadError 把单个文件的读取失败翻译成用户能懂的话。
func hashReadError(err error) string {
	if isPermissionDenied(err) {
		return "读取被拒绝（权限不足）"
	}
	return err.Error()
}

// hashOneFile 流式读一个文件并算出摘要。
//
// 逐块读而不是 io.Copy：这条路要顺手上报"读了多少"。缓冲区是固定的 1 MiB，
// 所以几个 GB 的文件也只在面板里占这么多内存 —— 与 io.Copy 的用量同一量级。
//
// onProgress 可缺省；它会被高频调用（每 MiB 一次），节流由调用方负责。
func hashOneFile(client *sftpGuard, item hashTarget, algo hashAlgo, onProgress func(done int64)) (string, error) {
	file, err := client.Open(item.abs)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hasher := algo.New()
	buf := make([]byte, 1<<20)
	var done int64
	for {
		n, err := file.Read(buf)
		if n > 0 {
			if _, werr := hasher.Write(buf[:n]); werr != nil {
				return "", werr
			}
			done += int64(n)
			if onProgress != nil {
				onProgress(done)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// collectHashTargets 展开出所有要算的文件。
func collectHashTargets(client *sftpGuard, roots []hashRoot) ([]hashTarget, error) {
	var out []hashTarget
	for _, root := range roots {
		if !root.isDir {
			// 选中的是单个文件：直接算它自己，不必遍历。
			info, err := client.Stat(root.path)
			if err != nil {
				return nil, err
			}
			out = append(out, hashTarget{abs: root.path, rel: path.Base(root.path), size: info.Size()})
			continue
		}
		if err := walkHashTargets(client, root.path, "", &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// walkHashTargets 递归收集一个目录下的普通文件。
//
// 不跟随目录内的符号链接：与目标机那条 find（不带 -L）口径一致，
// 也避免自指的链接把遍历带进死循环。
//
// 子目录读不了就跳过，不让整批失败：目录里混着一个没有读权限的子目录是常态，
// 而"整个目录都算不了"和"少了几个文件"对用户是两件事。
func walkHashTargets(client *sftpGuard, dir, prefix string, out *[]hashTarget) error {
	items, err := client.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, item := range items {
		name := item.Name()
		full := path.Join(dir, name)
		rel := path.Join(prefix, name)

		if item.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if item.IsDir() {
			_ = walkHashTargets(client, full, rel, out)
			continue
		}
		// 只算普通文件：管道、设备、socket 读起来会阻塞或永远读不完。
		if !item.Mode().IsRegular() {
			continue
		}
		*out = append(*out, hashTarget{abs: full, rel: rel, size: item.Size()})
	}
	return nil
}
