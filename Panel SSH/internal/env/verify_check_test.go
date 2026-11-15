package env

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// "装上了不等于能用"这类条目靠自带脚本复核，而脚本的结论会直接决定界面上那句
// "版本复核未通过"。这里用真的 sh 跑一遍复核脚本、再把输出交给 verify()，
// 把"通过 / 失败原因怎么流转"钉住 —— 只断言字符串拼接是测不出这个链路的。
func TestVerifyCheckKind(t *testing.T) {
	items := []verifyItem{
		{Name: "nginx", Kind: "check", Check: "echo OK"},
		{Name: "postgres", Kind: "check", Check: "echo '服务未运行（postgresql 未激活）'"},
	}

	script := verifyScript(&Profile{Family: FamilyApt}, items)
	out, err := exec.Command("sh", "-c", script).Output()
	if err != nil {
		t.Fatalf("复核脚本执行失败：%v", err)
	}

	// verify() 只依赖 Executor，因此把真实 shell 的输出喂给桩即可完整走一遍。
	verified, note := (&Runner{}).verify(
		context.Background(), &stubExecutor{stdout: string(out)}, &Profile{Family: FamilyApt}, nil, items)

	if verified {
		t.Fatalf("有失败项时不该判定为通过：%s", note)
	}
	if !strings.Contains(note, "postgres") || !strings.Contains(note, "服务未运行") {
		t.Fatalf("失败原因没有带上：%q", note)
	}
	if strings.Contains(note, "nginx") {
		t.Fatalf("通过的条目不该出现在问题清单里：%q", note)
	}
}

// 复核脚本是字符串拼出来的，写错一个引号只会在真机上、用户点完安装之后才暴露。
// 这里对全部目录项 × 受支持家族生成的脚本做一次语法检查（不执行，只验能否解析）。
func TestGeneratedCheckScriptsParse(t *testing.T) {
	profiles := map[Family]*Profile{
		FamilyApt: {Family: FamilyApt, OS: "ubuntu", Version: "24.04", Codename: "noble", Arch: "amd64", PkgArch: "x86_64"},
		FamilyDNF: {Family: FamilyDNF, OS: "rocky", Version: "9.4", Arch: "amd64", PkgArch: "x86_64"},
		FamilyYUM: {Family: FamilyYUM, OS: "centos", Version: "7.9", Arch: "amd64", PkgArch: "x86_64"},
		FamilyAPK: {Family: FamilyAPK, OS: "alpine", Version: "3.20.3", Codename: "v3.20", Arch: "arm64", PkgArch: "aarch64"},
	}

	checked := 0
	for _, r := range Catalog() {
		if r.CheckCommand == nil {
			continue
		}
		for family, p := range profiles {
			if !r.Supports(family) {
				continue
			}
			items := []verifyItem{{Name: r.Name, Kind: "check", Check: r.CheckCommand(p)}}
			script := verifyScript(p, items)
			if err := exec.Command("sh", "-n", "-c", script).Run(); err != nil {
				t.Fatalf("%s 在 %s 上的复核脚本无法解析：%v\n%s", r.Slug, family, err, script)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("没有任何条目声明复核脚本：目录项都退回弱复核了，这条用例也就失去意义")
	}
}

// 复核脚本没有输出时必须判为失败并给出可读原因，不能静默通过 ——
// 那等于把"没跑起来"当成"检查通过"。
func TestVerifyCheckEmptyOutputIsFailure(t *testing.T) {
	items := []verifyItem{{Name: "redis", Kind: "check", Check: "true"}}

	script := verifyScript(&Profile{Family: FamilyApt}, items)
	out, err := exec.Command("sh", "-c", script).Output()
	if err != nil {
		t.Fatalf("复核脚本执行失败：%v", err)
	}

	verified, note := (&Runner{}).verify(
		context.Background(), &stubExecutor{stdout: string(out)}, &Profile{Family: FamilyApt}, nil, items)

	if verified {
		t.Fatal("复核脚本没有输出时应判为未通过")
	}
	if !strings.Contains(note, "没有返回结果") {
		t.Fatalf("应说明脚本没有返回结果，得到 %q", note)
	}
}
