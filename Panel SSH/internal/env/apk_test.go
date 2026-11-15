package env

import (
	"context"
	"testing"
)

func TestApkParsePackages(t *testing.T) {
	raw := `alpine-baselayout-3.4.7-r2 x86_64 {alpine-baselayout} (installed) [system]
musl-1.2.5-r0 x86_64 {musl} (installed) [system]
py3-appdirs-1.4.4-r5 noarch {py3-appdirs} (installed) [world]
nginx-1.26.3-r0 aarch64 {nginx} (installed) [world]
`
	m := &apkManager{}
	pkgs := m.ParsePackages(raw)
	if len(pkgs) != 4 {
		t.Fatalf("包数量 = %d, want 4: %+v", len(pkgs), pkgs)
	}
	if pkgs[0].Name != "alpine-baselayout" || pkgs[0].Version != "3.4.7-r2" || pkgs[0].Arch != "x86_64" {
		t.Fatalf("首包解析错误: %+v", pkgs[0])
	}
	// 包名自身含连字符时不能被版本切分吞掉。
	if pkgs[2].Name != "py3-appdirs" || pkgs[2].Version != "1.4.4-r5" {
		t.Fatalf("连字符包名解析错误: %+v", pkgs[2])
	}
}

func TestApkUpgrades(t *testing.T) {
	raw := `<musl current: 1.2.5-r0 vs 1.2.5-r1>
<openssl current: 3.3.2-r1 vs 3.3.2-r2>
`
	m := &apkManager{}
	ups, err := m.Upgrades(context.Background(), &stubExecutor{stdout: raw})
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 2 {
		t.Fatalf("升级项数 = %d, want 2", len(ups))
	}
	if ups[0].Name != "musl" || ups[0].Current != "1.2.5-r0" || ups[0].Candidate != "1.2.5-r1" {
		t.Fatalf("升级项解析错误: %+v", ups[0])
	}
}

func TestApkSimOutput(t *testing.T) {
	// 模拟 apk awk 段产出的 TSV。
	output := `
fetch https://mirrors.tuna.tsinghua.edu.cn/alpine/v3.20/main/x86_64/APKINDEX.tar.gz
===CHANGES===
install	ca-certificates		20240226-r0	
install	nginx		1.26.3-r0	
upgrade	musl	1.2.5-r0	1.2.5-r1	
===CONFLICTS===

===RAW===
OK: 72 MiB in 71 packages
===RC===
0
`
	rpt := parseSimTSV(output)
	if !rpt.OK || len(rpt.Install) != 2 || len(rpt.Upgrade) != 1 {
		t.Fatalf("apk 模拟结果解析错误: %+v", rpt)
	}
}

func TestApkSimConflict(t *testing.T) {
	output := `
===CHANGES===

===CONFLICTS===
ERROR: unable to select packages:
  ngix (no such package):
===RAW===
===RC===
1
`
	rpt := parseSimTSV(output)
	if rpt.OK {
		t.Fatal("apk 报缺包时不应为 OK")
	}
}
