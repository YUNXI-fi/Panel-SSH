package api

import (
	"encoding/base64"
	"strings"
	"testing"
)

// 安全应用的操作目录是前端唯一的执行入口：任何一条没有 build 的动作、
// 或者默认动作指向不存在的 id，都会让用户在界面上点到一个死按钮。
func TestSecurityAppsAreComplete(t *testing.T) {
	for slug, app := range securityApps {
		if len(app.Actions) == 0 {
			t.Errorf("%s 没有声明任何操作", slug)
			continue
		}
		ids := map[string]bool{}
		for _, action := range app.Actions {
			if action.ID == "" || action.Label == "" {
				t.Errorf("%s 存在缺少 id 或名称的操作", slug)
			}
			if action.build == nil {
				t.Errorf("%s/%s 缺少命令构造器", slug, action.ID)
			}
			ids[action.ID] = true
		}
		if app.DefaultAction != "" && !ids[app.DefaultAction] {
			t.Errorf("%s 的默认动作 %s 不存在", slug, app.DefaultAction)
		}
	}
}

func TestSecurityParamValidation(t *testing.T) {
	app := securityApps["nmap"]
	var scan *securityAction
	for _, action := range app.Actions {
		if action.ID == "scan" {
			scan = action
		}
	}
	if scan == nil {
		t.Fatal("nmap 缺少端口扫描动作")
	}

	ok, err := normalizeSecurityParams(scan, map[string]string{
		"target": "192.168.1.0/24", "ports": "1-1000", "mode": "-sT",
	})
	if err != nil {
		t.Fatalf("合法参数被拒绝：%v", err)
	}
	if ok["target"] != "192.168.1.0/24" {
		t.Fatalf("参数被意外改写：%+v", ok)
	}

	if _, err := normalizeSecurityParams(scan, map[string]string{
		"target": "127.0.0.1; rm -rf /", "ports": "22", "mode": "-sT",
	}); err == nil {
		t.Fatal("带 shell 元字符的目标应被拒绝")
	}
	if _, err := normalizeSecurityParams(scan, map[string]string{
		"target": "127.0.0.1", "ports": "22", "mode": "-sS",
	}); err == nil {
		t.Fatal("不在选项里的扫描方式应被拒绝")
	}
	if _, err := normalizeSecurityParams(scan, map[string]string{
		"target": "127.0.0.1", "ports": "abc", "mode": "-sT",
	}); err == nil {
		t.Fatal("非法端口范围应被拒绝")
	}
}

func TestSecurityShellPath(t *testing.T) {
	if got := shellPath("~/.gnupg/gpg.conf"); got != `"$HOME/.gnupg/gpg.conf"` {
		t.Fatalf("家目录路径没有改用 $HOME：%s", got)
	}
	if got := shellPath("/etc/fail2ban/jail.local"); !strings.HasPrefix(got, "'/etc/") {
		t.Fatalf("绝对路径应按单引号处理：%s", got)
	}
}

func findSecurityAction(t *testing.T, slug, id string) *securityAction {
	t.Helper()
	app := securityApps[slug]
	if app == nil {
		t.Fatalf("缺少应用 %s", slug)
	}
	for _, action := range app.Actions {
		if action.ID == id {
			return action
		}
	}
	t.Fatalf("%s 缺少动作 %s", slug, id)
	return nil
}

func TestParseFail2BanJailList(t *testing.T) {
	out := "Status\n|- Number of jail:\t2\n`- Jail list:\tsshd, nginx-http-auth\n"
	got := parseFail2BanJailList(out)
	if len(got) != 2 || got[0] != "sshd" || got[1] != "nginx-http-auth" {
		t.Fatalf("Jail 列表解析错误：%#v", got)
	}
	if names := parseFail2BanJailList("Status\n|- Number of jail:\t0\n`- Jail list:\t\n"); len(names) != 0 {
		t.Fatalf("空列表应返回空结果：%#v", names)
	}
}

func TestParseFail2BanJailStatus(t *testing.T) {
	out := "Status for the jail: sshd\n" +
		"|- Filter\n" +
		"|  |- Currently failed:\t1\n" +
		"|  |- Total failed:\t8\n" +
		"|  `- File list:\t/var/log/auth.log\n" +
		"`- Actions\n" +
		"   |- Currently banned:\t2\n" +
		"   |- Total banned:\t5\n" +
		"   `- Banned IP list:\t192.0.2.10 2001:db8::1\n"

	jail := fail2banJail{Name: "sshd"}
	parseFail2BanJailStatus(out, &jail)

	if jail.CurrentlyFailed != 1 || jail.TotalFailed != 8 {
		t.Fatalf("失败数解析错误：%+v", jail)
	}
	if jail.CurrentlyBanned != 2 || jail.TotalBanned != 5 {
		t.Fatalf("封禁数解析错误：%+v", jail)
	}
	if len(jail.BannedIPs) != 2 || jail.BannedIPs[1] != "2001:db8::1" {
		t.Fatalf("封禁 IP 解析错误：%#v", jail.BannedIPs)
	}
}

func TestParseGpgColonKeys(t *testing.T) {
	out := "tru::1:1794752785:1826288783:3:1:5\n" +
		"pub:u:2048:1:1ED066272DF02328:1794752816:1857824816::u:::scESC::::::23::0:\n" +
		"fpr:::::::::D000C84F349269AEE50168111ED066272DF02328:\n" +
		"uid:u::::1794752816::C234CC3ED55F6FAF92BAF573796E42AF1DA5E9D8::张三 <zhang@example.com>::::::::::0:\n" +
		"sub:u:2048:1:671E92086A2B70B6:1794752816:1857824816:::::e::::::23:\n" +
		"fpr:::::::::9E9C9D13F78F801E2E7E17ED671E92086A2B70B6:\n" +
		"pub:u:255:22:47B7A789BEABE14E:1794752820:::u:::scESC:::::ed25519:::0:\n" +
		"fpr:::::::::A643D75167F5126283672F8147B7A789BEABE14E:\n" +
		"uid:u::::1794752820::5BB810CE29DFCD66620A18ED3198FBA4B5D068F4::李四 <li@example.com>::::::::::0:\n"

	keys := parseGpgColonKeys(out)
	if len(keys) != 2 {
		t.Fatalf("应解析出两把主钥：%d", len(keys))
	}

	rsa := keys[0]
	if rsa.Fingerprint != "D000C84F349269AEE50168111ED066272DF02328" {
		t.Fatalf("主钥指纹解析错误：%s", rsa.Fingerprint)
	}
	if rsa.Name != "张三" || rsa.Email != "zhang@example.com" {
		t.Fatalf("UID 拆分错误：%q / %q", rsa.Name, rsa.Email)
	}
	if rsa.Algorithm != "RSA" || rsa.Bits != 2048 {
		t.Fatalf("算法解析错误：%s %d", rsa.Algorithm, rsa.Bits)
	}
	if !containsAll(rsa.Capabilities, "sign", "certify", "encrypt") {
		t.Fatalf("能力解析错误：%#v", rsa.Capabilities)
	}
	if len(rsa.Subkeys) != 1 || rsa.Subkeys[0].Algorithm != "RSA" {
		t.Fatalf("子钥解析错误：%#v", rsa.Subkeys)
	}
	if !containsAll(rsa.Subkeys[0].Capabilities, "encrypt") {
		t.Fatalf("子钥能力解析错误：%#v", rsa.Subkeys[0].Capabilities)
	}

	ed := keys[1]
	if ed.Algorithm != "Ed25519" || ed.Bits != 255 {
		t.Fatalf("曲线算法解析错误：%s %d", ed.Algorithm, ed.Bits)
	}
}

func TestGnuPGGenerateBuildsParameterFile(t *testing.T) {
	action := findSecurityAction(t, "gnupg", "generate")
	script, err := action.build(map[string]string{
		"name": "张三", "email": "zhang@example.com",
		"algo": "ed25519", "expire": "2y", "passphrase": "p@ss:word",
	})
	if err != nil {
		t.Fatalf("生成命令构造失败：%v", err)
	}
	if strings.Contains(script, "p@ss:word") {
		t.Fatal("口令不应以明文出现在投递的脚本里")
	}
	params := decodeSecurityScriptFile(t, script)
	for _, want := range []string{
		"Key-Type: EDDSA", "Key-Curve: Ed25519",
		"Subkey-Type: ECDH", "Subkey-Curve: Curve25519",
		"Name-Real: 张三", "Name-Email: zhang@example.com",
		"Expire-Date: 2y", "Passphrase: p@ss:word", "%commit",
	} {
		if !strings.Contains(params, want) {
			t.Fatalf("参数文件缺少 %q：\n%s", want, params)
		}
	}

	script, err = action.build(map[string]string{
		"name": "无口令", "algo": "rsa2048", "expire": "0", "passphrase": "",
	})
	if err != nil {
		t.Fatalf("空口令构造失败：%v", err)
	}
	if !strings.Contains(decodeSecurityScriptFile(t, script), "%no-protection") {
		t.Fatal("空口令应使用 %no-protection")
	}

	if _, err := action.build(map[string]string{
		"name": "bad\nKey-Length: 4096", "algo": "rsa2048", "expire": "1y",
	}); err == nil {
		t.Fatal("姓名里的换行应被拒绝")
	}
}

func TestGnuPGImportRejectsPrivateKey(t *testing.T) {
	action := findSecurityAction(t, "gnupg", "import")
	if _, err := action.build(map[string]string{
		"armored": "-----BEGIN PGP PRIVATE KEY BLOCK-----\nabc\n-----END PGP PRIVATE KEY BLOCK-----",
	}); err == nil {
		t.Fatal("私钥块应被拒绝")
	}
	script, err := action.build(map[string]string{
		"armored": "-----BEGIN PGP PUBLIC KEY BLOCK-----\nabc\n-----END PGP PUBLIC KEY BLOCK-----",
	})
	if err != nil {
		t.Fatalf("公钥块被拒绝：%v", err)
	}
	if !strings.Contains(script, `gpg --batch --yes --import "$KEYFILE"`) {
		t.Fatalf("导入命令不符合预期：%s", script)
	}
}

func containsAll(values []string, want ...string) bool {
	set := map[string]bool{}
	for _, value := range values {
		set[value] = true
	}
	for _, value := range want {
		if !set[value] {
			return false
		}
	}
	return true
}

// decodeSecurityScriptFile 取出脚本里 base64 -d 写临时文件的那一段，
// 用于断言投递到目标机的真实内容。
func decodeSecurityScriptFile(t *testing.T, script string) string {
	t.Helper()
	const prefix = "printf '%s' '"
	start := strings.Index(script, prefix)
	if start < 0 {
		t.Fatalf("脚本里没有 base64 写入：%s", script)
	}
	rest := script[start+len(prefix):]
	end := strings.Index(rest, "' | base64 -d")
	if end < 0 {
		t.Fatalf("脚本缺少 base64 解码段：%s", script)
	}
	data, err := base64.StdEncoding.DecodeString(rest[:end])
	if err != nil {
		t.Fatalf("base64 解码失败：%v", err)
	}
	return string(data)
}
