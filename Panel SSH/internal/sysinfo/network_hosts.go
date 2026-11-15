package sysinfo

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
)

// /etc/hosts 的读取与改写。
//
// 这里刻意只做两件事：把原文解析成条目、在原文上改一行。**改动不写成目标机上的
// 一段脚本**，理由与 env 包处理 PATH 时一样：按正则删行会误伤用户手写的行
// （`~/.profile` 那次就是这样把登录环境搞坏的），而 hosts 比 profile 更没有
// 试错余地 —— 改错一条会让目标机在一段时间里解析不到自己的名字。
// 因此目标机只负责"把给定的内容写下去"，改哪一行、改成什么由面板算好。
//
// 改写是纯函数：内容从哪来（采集到的，还是写入前重读的）由调用方决定。

// HostsPath 是这条链路唯一会写的文件。
const HostsPath = "/etc/hosts"

// hostsLinePrefix 是采集时为每一行加的前缀，见 unescapeHosts。
const hostsLinePrefix = "|"

// 一次 hosts 变更的三种动作。
const (
	HostsAdd    = "add"
	HostsUpdate = "update"
	HostsRemove = "remove"
)

// HostEntry 是 /etc/hosts 里的一条记录，Comment 是行内 `#` 之后的内容。
type HostEntry struct {
	IP    string   `json:"ip"`
	Names []string `json:"names"`
	// Comment 不含 `#` 本身；空串表示这一行没有注释。
	Comment string `json:"comment"`
	// Line 是这条记录在原文里的行号（1-based，空行与注释行都计入）。
	// 改与删都靠它定位：同一个 IP 或同一个主机名在 hosts 里重复出现是常事，
	// 只按内容找找不准是哪一条。
	Line int `json:"line"`
}

// HostChangeRequest 是一次 /etc/hosts 变更。
type HostChangeRequest struct {
	Action string `json:"action"`
	// HostsMd5 是提交方读到的那份原文的摘要（见 HostsDigest）。
	//
	// 服务端在写入前会重读文件比对：不一致说明这中间有人改过，此时宁可拒绝，
	// 也不能把基于旧内容算出来的结果盖上去 —— 那会把别人的改动整段抹掉，
	// 而且是静默的。
	HostsMd5 string `json:"hostsMd5"`

	// Line 与 OldNames 是 update / remove 的定位锚点。
	//
	// 摘要已经能挡住"文件被改过"，锚点是第二道：用户看到的行号与主机名必须
	// 与文件里那两行仍然对得上。两道都要，是因为摘要会随每一次无关的改动
	// 一起变（别人加了一行，你的行号仍是准的），而单靠行号又会在别人
	// 插删行时指向错误的那一条。
	Line     int      `json:"line"`
	OldNames []string `json:"oldNames"`

	// IP / Names / Comment 是 add 与 update 要写入的新内容。
	IP      string   `json:"ip"`
	Names   []string `json:"names"`
	Comment string   `json:"comment"`
}

// parseHosts 解析采集到的 hosts 一节，返回条目与原文摘要。
func parseHosts(section string) ([]HostEntry, string) {
	content := unescapeHosts(section)
	entries := make([]HostEntry, 0, 16)
	for i, line := range hostLines(content) {
		entry := parseHostLine(line)
		if entry == nil {
			continue
		}
		entry.Line = i + 1
		entries = append(entries, *entry)
	}
	return entries, HostsDigest(content)
}

// ApplyHostChange 在给定的 hosts 内容上做一次变更，返回新内容。
//
// 调用方必须先自行确认这份内容就是目标机上当前的原文（摘要比对），
// 否则这里算出的行号可能指向别的记录。
func ApplyHostChange(content string, req HostChangeRequest) (string, error) {
	lines := hostLines(hostsContent(content))

	switch req.Action {
	case HostsAdd:
		return hostsAdd(lines, req)
	case HostsUpdate:
		return hostsRewrite(lines, req, true)
	case HostsRemove:
		return hostsRewrite(lines, req, false)
	default:
		return "", errors.New("未知的 Hosts 操作：" + req.Action)
	}
}

// HostsDigest 计算 hosts 原文的摘要，用作改动的乐观锁。
//
// 摘要算在"归一之后"的原文上（见 hostsContent），这样采集路径（分节输出，
// 每行都会带换行）与实际文件内容（cat 的输出）算出来的是同一个值。
func HostsDigest(raw string) string {
	sum := md5.Sum([]byte(hostsContent(raw)))
	return hex.EncodeToString(sum[:])
}

// hostsContent 把一份 hosts 原文归一到"不含尾随换行、行尾统一为 \n"的形式。
func hostsContent(raw string) string {
	normalized := strings.ReplaceAll(raw, "\r\n", "\n")
	return strings.TrimRight(normalized, "\n")
}

// unescapeHosts 还原采集时加的行首前缀。
//
// 为什么要加前缀：采集输出是按行分节的（见 splitSections），而行首是 "###"
// 的行会被当成下一个分节的开始。hosts 里写一行 `##########` 当分隔注释完全
// 合法，一旦命中，这一节的内容就从那里被截断 —— 界面上少几条记录还只是小事，
// 摘要与真实文件不再一致会让保存永远失败（每次都提示"文件已被他人修改"）。
// 前缀取 "|"：它不会出现在 hosts 行首（那里是 IP 或 `#`），还原时逐行剥掉即可。
func unescapeHosts(section string) string {
	lines := strings.Split(section, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimPrefix(line, hostsLinePrefix)
	}
	// splitSections 会给每一行（含最后一行）都补上换行，去掉尾部那一行空串。
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// parseHostLine 解析一行，返回 nil 表示这一行没有主机记录（空行、整行注释、
// 或只有地址没有名字）。
func parseHostLine(line string) *HostEntry {
	body, comment := splitHostComment(line)
	fields := strings.Fields(body)
	if len(fields) < 2 {
		return nil
	}
	return &HostEntry{IP: fields[0], Names: fields[1:], Comment: comment}
}

// hostsAdd 追加一条记录。
func hostsAdd(lines []string, req HostChangeRequest) (string, error) {
	line, err := hostLine(req)
	if err != nil {
		return "", err
	}

	// 主机名已存在时不追加，直接指出它在第几行。
	//
	// hosts 的匹配是"第一条命中生效"，再追加一条同名记录只会让用户以为自己
	// 改的那条起作用了，而实际生效的仍是前面那条。也不做隐式替换：面板并不知道
	// 用户想保留哪一条的其它别名，替他决定比让他自己看一眼更容易出错。
	existing := indexHostNames(lines)
	for _, name := range splitNames(req.Names) {
		if at, ok := existing[strings.ToLower(name)]; ok {
			return "", fmt.Errorf("主机名 %s 已存在（第 %d 行），请直接编辑那一条", name, at)
		}
	}

	return joinHostLines(append(lines, line)), nil
}

// hostsRewrite 改写（keep 为真）或删除（keep 为假）锚点指向的那一行。
func hostsRewrite(lines []string, req HostChangeRequest, keep bool) (string, error) {
	idx := req.Line - 1
	if req.Line < 1 || idx >= len(lines) {
		return "", fmt.Errorf("第 %d 行已不存在，文件可能已被修改，请刷新后重试", req.Line)
	}

	current := parseHostLine(lines[idx])
	if current == nil {
		return "", fmt.Errorf("第 %d 行已不是一条主机记录，文件可能已被修改，请刷新后重试", req.Line)
	}
	if !sameNames(current.Names, req.OldNames) {
		return "", fmt.Errorf("第 %d 行的主机名已是 %s，文件可能已被修改，请刷新后重试",
			req.Line, strings.Join(current.Names, " "))
	}

	if !keep {
		remaining := make([]string, 0, len(lines)-1)
		remaining = append(remaining, lines[:idx]...)
		remaining = append(remaining, lines[idx+1:]...)
		return joinHostLines(remaining), nil
	}

	line, err := hostLine(req)
	if err != nil {
		return "", err
	}
	replaced := make([]string, 0, len(lines))
	replaced = append(replaced, lines[:idx]...)
	replaced = append(replaced, line)
	replaced = append(replaced, lines[idx+1:]...)
	return joinHostLines(replaced), nil
}

// hostLine 把请求里的新内容拼成一条 hosts 记录。
func hostLine(req HostChangeRequest) (string, error) {
	ip := strings.TrimSpace(req.IP)
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("IP 地址不合法：%s", req.IP)
	}

	names := splitNames(req.Names)
	if len(names) == 0 {
		return "", errors.New("请至少填写一个主机名")
	}
	for _, name := range names {
		if err := validateHostName(name); err != nil {
			return "", err
		}
	}

	line := ip + " " + strings.Join(names, " ")
	if comment := cleanComment(req.Comment); comment != "" {
		line += " # " + comment
	}
	return line, nil
}

// maxHostNameLen 是完整域名的长度上限（RFC 1035）。
const maxHostNameLen = 253

// validateHostName 校验一个主机名或别名。
//
// 只放行字母、数字、点与连字符：hosts 里一个多余的空格就会让后面的内容变成
// 另一条记录的主机名（写出来的行会被解析成完全不同的东西），而引号、`#`
// 这类字符会改变这一行的结构 —— 宁可在面板上明确拒绝，也不要写进系统文件后
// 让目标机解析出用户没打算写的东西。
func validateHostName(name string) error {
	if len(name) > maxHostNameLen {
		return fmt.Errorf("主机名过长（超过 %d 个字符）：%s", maxHostNameLen, name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
		default:
			return fmt.Errorf("主机名只能包含字母、数字、点与连字符：%s", name)
		}
	}
	return nil
}

// splitNames 把主机名收拾成一份列表：按空白拆开、去掉空项与重复项。
//
// 用户在表单里习惯一次填 "web db"（这正是 hosts 的写法），因此不能
// 把一个输入框的值当成一个名字。
func splitNames(raw []string) []string {
	names := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, item := range raw {
		for _, name := range strings.Fields(item) {
			key := strings.ToLower(name)
			if seen[key] {
				continue
			}
			seen[key] = true
			names = append(names, name)
		}
	}
	return names
}

// sameNames 比较两组主机名是否一致，忽略顺序与大小写。
//
// 锚点比对用集合而不是逐字比较：用户在表单里把别名换个顺序再保存，
// 不该被当成"文件已被他人修改"。
func sameNames(a, b []string) bool {
	left, right := splitNames(a), splitNames(b)
	if len(left) != len(right) {
		return false
	}
	index := make(map[string]bool, len(left))
	for _, name := range left {
		index[strings.ToLower(name)] = true
	}
	for _, name := range right {
		if !index[strings.ToLower(name)] {
			return false
		}
	}
	return true
}

// indexHostNames 给出主机名到行号（1-based）的索引；同名时保留最先出现的那一行，
// 因为生效的正是第一条。
func indexHostNames(lines []string) map[string]int {
	index := make(map[string]int, len(lines))
	for i, line := range lines {
		entry := parseHostLine(line)
		if entry == nil {
			continue
		}
		for _, name := range entry.Names {
			key := strings.ToLower(name)
			if _, ok := index[key]; !ok {
				index[key] = i + 1
			}
		}
	}
	return index
}

// splitHostComment 把一行拆成"记录"与"行内注释"两段。
func splitHostComment(line string) (body, comment string) {
	if idx := strings.IndexByte(line, '#'); idx >= 0 {
		return line[:idx], strings.TrimSpace(line[idx+1:])
	}
	return line, ""
}

// cleanComment 把注释压成单行。
//
// 注释里带换行会让写入的那一行裂成两行，而 hosts 的每一行都是一条独立记录 ——
// 那等于凭空多出一条谁也没写过的记录。
func cleanComment(raw string) string {
	replaced := strings.NewReplacer("\r", " ", "\n", " ").Replace(raw)
	return strings.TrimSpace(replaced)
}

// hostLines 把原文切成行；空内容给空切片而不是 [""]。
func hostLines(content string) []string {
	if content == "" {
		return nil
	}
	return strings.Split(content, "\n")
}

// joinHostLines 把行拼回文本，末尾补一个换行。
//
// 补换行不是格式偏好：文本文件最后一行没有换行时，任何"追加一行"的工具都会
// 把它与下一条拼成同一行，而 hosts 的一行就是一条记录 —— 拼起来之后，
// 解析层看到的会是"IP 后面跟着四个主机名"，用户看到一条莫名其妙的记录。
func joinHostLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}