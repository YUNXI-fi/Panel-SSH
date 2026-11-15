package env

import (
	"context"
	"testing"
)

func TestAptParsePackages(t *testing.T) {
	// Status-Abbrev：ii 正常安装、hi hold 状态仍算已装、rc 只剩配置必须排除。
	raw := "ii \tadduser\t3.118ubuntu5\tall\n" +
		"ii \tapt\t2.4.14\tamd64\n" +
		"rc \toldconf\t1.0\tall\n" +
		"hi \tvim\t2:8.2-3\tamd64\n"
	m := &aptManager{}
	pkgs := m.ParsePackages(raw)
	got := map[string]string{}
	for _, p := range pkgs {
		got[p.Name] = p.Version
	}
	if len(pkgs) != 3 {
		t.Fatalf("解析出 %d 个包，应排除 rc 残留得到 3 个: %+v", len(pkgs), pkgs)
	}
	if got["oldconf"] != "" {
		t.Fatal("rc 状态的残留配置包不应算作已安装")
	}
	if got["vim"] != "2:8.2-3" {
		t.Fatalf("带 epoch 的版本号解析错误: %q", got["vim"])
	}
}

func TestAptUpgrades(t *testing.T) {
	raw := `
Listing... Done
libc6/focal-security 2.31-0ubuntu9.16 amd64 [upgradable from: 2.31-0ubuntu9.15]
nginx/jammy-updates 1.18.0-0ubuntu1.3 amd64 [upgradable from: 1.18.0-0ubuntu1.2]
WARNING: apt does not have a stable CLI interface. Use with caution in scripts.
`
	m := &aptManager{}
	ups, err := m.Upgrades(context.Background(), &stubExecutor{stdout: raw})
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 2 {
		t.Fatalf("可升级包数量 = %d, want 2: %+v", len(ups), ups)
	}
	if ups[0].Name != "libc6" || ups[0].Current != "2.31-0ubuntu9.15" || ups[0].Candidate != "2.31-0ubuntu9.16" {
		t.Fatalf("首条升级项解析错误: %+v", ups[0])
	}
}

func TestAptCandidates(t *testing.T) {
	raw := `nginx:
  Installed: 1.18.0-0ubuntu1.2
  Candidate: 1.18.0-0ubuntu1.3
  Version table:
     1.18.0-0ubuntu1.3 500
        500 http://archive.ubuntu.com/ubuntu jammy-updates/main amd64 Packages
     1.18.0-0ubuntu1 500
        500 http://archive.ubuntu.com/ubuntu jammy/main amd64 Packages
`
	m := &aptManager{}
	cands, err := m.Candidates(context.Background(), &stubExecutor{stdout: raw}, "nginx")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 3 {
		t.Fatalf("候选版本数 = %d, want 3（含已装置顶）: %+v", len(cands), cands)
	}
	if cands[0].Version != "1.18.0-0ubuntu1.2" || cands[0].Repo != "(已安装)" {
		t.Fatalf("已装版本应置顶: %+v", cands[0])
	}
	if cands[1].Version != "1.18.0-0ubuntu1.3" {
		t.Fatalf("候选版本顺序错误: %+v", cands)
	}
}

func TestParseSimTSVApt(t *testing.T) {
	// 模拟 apt awk 段产出的 TSV：RC=1 在有变更项时是 dnf 系的正常情况，
	// apt 正常 RC=0；这里同时验证"有变更即不报冲突"的判定。
	output := `
===CHANGES===
install	nginx		1.18.0-0ubuntu1.3	amd64
upgrade	libc6	2.31-0ubuntu9.15	2.31-0ubuntu9.16	amd64
downgrade	custom-pkg	2.0	1.0	amd64
remove	oldnginx	1.18.0-0ubuntu1		amd64
===CONFLICTS===

===SIZE===
27.9 MB
===RAW===
===RC===
0
`
	rpt := parseSimTSV(output)
	if !rpt.OK {
		t.Fatal("无冲突行且 RC=0 应当 OK")
	}
	if len(rpt.Install) != 1 || rpt.Install[0].Name != "nginx" {
		t.Fatalf("新装项解析错误: %+v", rpt.Install)
	}
	if len(rpt.Upgrade) != 1 || rpt.Upgrade[0].From != "2.31-0ubuntu9.15" {
		t.Fatalf("升级项解析错误: %+v", rpt.Upgrade)
	}
	if len(rpt.Downgrade) != 1 || len(rpt.Remove) != 1 {
		t.Fatalf("降级/卸载项解析错误: %+v %+v", rpt.Downgrade, rpt.Remove)
	}
	// 27.9 MB 按 1000 进制换算为 KiB 口径的 27900 KB。
	if rpt.DownloadKB != 27_900 {
		t.Fatalf("下载量换算 = %d KB, want 27900", rpt.DownloadKB)
	}
}

func TestParseSimTSVConflict(t *testing.T) {
	output := `
===CHANGES===

===CONFLICTS===
E: Unable to locate package ngix
===SIZE===

===RAW===
E: Unable to locate package ngix
===RC===
100
`
	rpt := parseSimTSV(output)
	if rpt.OK {
		t.Fatal("求解器报错时不应为 OK")
	}
	if len(rpt.Conflicts) != 1 || rpt.Conflicts[0] != "E: Unable to locate package ngix" {
		t.Fatalf("冲突信息解析错误: %+v", rpt.Conflicts)
	}
}

func TestParseSimTSVEmptyFailure(t *testing.T) {
	// RC 非 0、无变更也无显式冲突行：必须从 RAW 补阻断信息，不能误判成功。
	output := `
===CHANGES===

===CONFLICTS===

===SIZE===

===RAW===
Exiting on user command "No".
===RC===
1
`
	rpt := parseSimTSV(output)
	if rpt.OK || len(rpt.Conflicts) == 0 {
		t.Fatalf("求解失败但无变更时应回退到 RAW 信息: %+v", rpt)
	}
}

func TestValidPkgToken(t *testing.T) {
	good := []string{"nginx", "libc6", "openjdk-17-jdk", "gcc=12.2.0-1", "g++-12", "python3.11", "kernel-headers*"}
	for _, s := range good {
		if !validPkgToken(s) {
			t.Errorf("合法 token 被拒绝: %q", s)
		}
	}
	bad := []string{"nginx;reboot", "$(id)", "nginx|whoami", "pkg`id`", "a b"}
	for _, s := range bad {
		if validPkgToken(s) {
			t.Errorf("危险 token 未被拦截: %q", s)
		}
	}
}
