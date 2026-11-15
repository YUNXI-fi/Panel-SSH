package sysinfo

import (
	"strings"
	"testing"
)

// 采集路径（sed 给每行加前缀、行尾带换行）与写入前重读（cat 原样输出）必须给出
// 同一个摘要。不一致的后果是保存永远失败：每次都提示"文件已被他人修改"，
// 而用户对着一个根本没变过的文件，怎么刷新都一样。
func TestHostsDigestSameForCollectAndCat(t *testing.T) {
	file := "127.0.0.1 localhost\n#### 分隔注释\n::1 ip6-localhost ip6-loopback\n"
	collected := "|127.0.0.1 localhost\n|#### 分隔注释\n|::1 ip6-localhost ip6-loopback\n"
	if got, want := HostsDigest(unescapeHosts(collected)), HostsDigest(file); got != want {
		t.Fatalf("采集与直读的摘要应一致：采集 %s，直读 %s", got, want)
	}
}

// 行首是 "####" 的分隔注释必须原样保留。采集输出是按行分节的，
// 没有那层前缀的话这一行会被当成下一个分节的开始，后面的内容整段丢失 ——
// 界面上少几条记录只是小事，摘要与真实文件对不上才是真的麻烦。
func TestParseHostsKeepsHashSeparator(t *testing.T) {
	raw := "###TOOLS\nip\n###HOSTS\n" +
		"|127.0.0.1 localhost\n" +
		"|\n" +
		"|#### 分隔注释\n" +
		"|192.168.1.10 nas.lan nas # 内网存储\n" +
		"###RESOLV\nnameserver 1.1.1.1\n"

	entries, digest := parseHosts(splitSections(raw)[secHosts])
	if len(entries) != 2 {
		t.Fatalf("应解析出 2 条记录，实际 %d 条：%+v", len(entries), entries)
	}
	if entries[0].IP != "127.0.0.1" || entries[0].Line != 1 || len(entries[0].Names) != 1 {
		t.Fatalf("第一条记录解析有误：%+v", entries[0])
	}
	// 行号是原文里的行号，空行与注释行都计入 —— 改与删都靠它定位。
	if entries[1].Line != 4 {
		t.Fatalf("第二条应落在第 4 行，实际 %d", entries[1].Line)
	}
	if strings.Join(entries[1].Names, " ") != "nas.lan nas" || entries[1].Comment != "内网存储" {
		t.Fatalf("别名与行内注释解析有误：%+v", entries[1])
	}

	want := HostsDigest("127.0.0.1 localhost\n\n#### 分隔注释\n192.168.1.10 nas.lan nas # 内网存储\n")
	if digest != want {
		t.Fatalf("摘要应算在还原后的原文上：得到 %s，期望 %s", digest, want)
	}
}

const hostsSample = "127.0.0.1 localhost\n\n# 内网\n192.168.1.10 nas.lan nas\n203.0.113.7 web web.lan # 测试机\n"

func TestApplyHostChangeAdd(t *testing.T) {
	got, err := ApplyHostChange(hostsSample, HostChangeRequest{
		Action: HostsAdd,
		IP:     "10.0.0.5",
		Names:  []string{"db db.lan"},
	})
	if err != nil {
		t.Fatalf("追加失败：%v", err)
	}
	if want := hostsSample + "10.0.0.5 db db.lan\n"; got != want {
		t.Fatalf("追加结果有误：\n%q\n期望：\n%q", got, want)
	}

	// 末尾没有换行的文件：必须先补齐，否则新记录会与最后一条拼成同一行，
	// 解析出来变成"IP 后面跟着四个主机名"的一条怪记录。
	got, err = ApplyHostChange("127.0.0.1 localhost", HostChangeRequest{
		Action: HostsAdd, IP: "10.0.0.5", Names: []string{"db"},
	})
	if err != nil {
		t.Fatalf("追加失败：%v", err)
	}
	if want := "127.0.0.1 localhost\n10.0.0.5 db\n"; got != want {
		t.Fatalf("末尾无换行时应先补换行：%q", got)
	}

	// 同名主机名已存在时直接拒绝，并指出它在第几行：追加一条同名记录不会生效
	// （hosts 取第一条命中），用户会以为改生效了。
	_, err = ApplyHostChange(hostsSample, HostChangeRequest{
		Action: HostsAdd, IP: "10.0.0.6", Names: []string{"nas"},
	})
	if err == nil || !strings.Contains(err.Error(), "第 4 行") {
		t.Fatalf("同名主机名应被拒绝并指出行号，实际：%v", err)
	}

	_, err = ApplyHostChange(hostsSample, HostChangeRequest{
		Action: HostsAdd, IP: "10.0.0.999", Names: []string{"x"},
	})
	if err == nil || !strings.Contains(err.Error(), "IP 地址不合法") {
		t.Fatalf("非法地址应被拒绝，实际：%v", err)
	}

	_, err = ApplyHostChange(hostsSample, HostChangeRequest{
		Action: HostsAdd, IP: "10.0.0.7", Names: []string{"a b"},
	})
	if err != nil {
		t.Fatalf("多个别名应被拆开写入：%v", err)
	}

	if _, err := ApplyHostChange(hostsSample, HostChangeRequest{
		Action: HostsAdd, IP: "10.0.0.8", Names: []string{"中文名"},
	}); err == nil || !strings.Contains(err.Error(), "主机名只能包含") {
		t.Fatalf("非 ASCII 主机名应被拒绝，实际：%v", err)
	}
}

func TestApplyHostChangeAnchors(t *testing.T) {
	// 行号与主机名都对得上：改写第 5 行，其余行原样保留。
	got, err := ApplyHostChange(hostsSample, HostChangeRequest{
		Action:   HostsUpdate,
		Line:     5,
		OldNames: []string{"web", "web.lan"},
		IP:       "203.0.113.8",
		Names:    []string{"web", "web.lan"},
		Comment:  "换过地址",
	})
	if err != nil {
		t.Fatalf("改写失败：%v", err)
	}
	want := "127.0.0.1 localhost\n\n# 内网\n192.168.1.10 nas.lan nas\n203.0.113.8 web web.lan # 换过地址\n"
	if got != want {
		t.Fatalf("改写结果有误：\n%q\n期望：\n%q", got, want)
	}

	// 锚点对不上（文件在别处被改过）：必须拒绝，而不是照着行号改一条别的记录。
	_, err = ApplyHostChange(hostsSample, HostChangeRequest{
		Action: HostsUpdate, Line: 3, OldNames: []string{"web"},
		IP: "203.0.113.9", Names: []string{"web"},
	})
	if err == nil || !strings.Contains(err.Error(), "请刷新") {
		t.Fatalf("锚点对不上时应拒绝并提示刷新，实际：%v", err)
	}

	_, err = ApplyHostChange(hostsSample, HostChangeRequest{
		Action: HostsUpdate, Line: 9, OldNames: []string{"web"},
		IP: "203.0.113.9", Names: []string{"web"},
	})
	if err == nil || !strings.Contains(err.Error(), "已不存在") {
		t.Fatalf("行号越界时应拒绝，实际：%v", err)
	}
}

func TestApplyHostChangeRemove(t *testing.T) {
	got, err := ApplyHostChange(hostsSample, HostChangeRequest{
		Action: HostsRemove, Line: 5, OldNames: []string{"web", "web.lan"},
	})
	if err != nil {
		t.Fatalf("删除失败：%v", err)
	}
	want := "127.0.0.1 localhost\n\n# 内网\n192.168.1.10 nas.lan nas\n"
	if got != want {
		t.Fatalf("删除结果有误：\n%q\n期望：\n%q", got, want)
	}

	// 删掉最后一条记录后不该留一个空行：空文件就是空文件。
	got, err = ApplyHostChange("127.0.0.1 localhost\n", HostChangeRequest{
		Action: HostsRemove, Line: 1, OldNames: []string{"localhost"},
	})
	if err != nil {
		t.Fatalf("删除失败：%v", err)
	}
	if got != "" {
		t.Fatalf("删空后应是空文件，实际 %q", got)
	}
}

// 注释里的换行会凭空多出一条记录，必须在写入前压平。
func TestApplyHostChangeCommentStaysSingleLine(t *testing.T) {
	got, err := ApplyHostChange("", HostChangeRequest{
		Action: HostsAdd, IP: "10.0.0.5", Names: []string{"db"}, Comment: "第一行\n第二行",
	})
	if err != nil {
		t.Fatalf("追加失败：%v", err)
	}
	if want := "10.0.0.5 db # 第一行 第二行\n"; got != want {
		t.Fatalf("注释应压成单行：%q", got)
	}
}