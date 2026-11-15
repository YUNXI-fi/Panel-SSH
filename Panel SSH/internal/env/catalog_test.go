package env

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// 目录条目最终会落到真实机器上：包名写错、漏掉某个家族的包、或者计划里出现
// 一条空命令，用户都要等到预检甚至安装时才会撞上。这里把每条目录项的基本契约
// 钉一遍 —— 新增条目时不必再靠人眼通读。
func TestCatalogRecipesAreComplete(t *testing.T) {
	profiles := map[Family]*Profile{
		FamilyApt: {Family: FamilyApt, OS: "ubuntu", Version: "24.04", Codename: "noble", Arch: "amd64", PkgArch: "x86_64"},
		FamilyDNF: {Family: FamilyDNF, OS: "rocky", Version: "9.4", Arch: "amd64", PkgArch: "x86_64"},
		FamilyYUM: {Family: FamilyYUM, OS: "centos", Version: "7.9", Arch: "amd64", PkgArch: "x86_64"},
		FamilyAPK: {Family: FamilyAPK, OS: "alpine", Version: "3.20.3", Codename: "v3.20", Arch: "arm64", PkgArch: "aarch64"},
	}
	// 这几个条目的 Plan 要求显式版本号（空串会按设计报错）。
	versions := map[string]string{"golang": "1.24.0", "nodejs": "22", "openjdk": "21"}
	validCategories := map[string]bool{
		"runtime": true, "toolchain": true, "database": true, "service": true,
		"monitor": true, "utility": true, "network": true, "security": true, "backup": true,
	}

	seen := map[string]bool{}
	for _, r := range Catalog() {
		t.Run(r.Slug, func(t *testing.T) {
			if r.Slug == "" || r.Name == "" || r.Description == "" {
				t.Fatalf("slug / 名称 / 描述不能为空：%+v", r)
			}
			if seen[r.Slug] {
				t.Fatalf("slug 重复：%s", r.Slug)
			}
			seen[r.Slug] = true

			if !validCategories[r.Category] {
				t.Fatalf("分类 %q 不在已知集合里：界面按分类分组，写错就会多出一个陌生的分组", r.Category)
			}
			if r.DetectCommand == nil {
				t.Fatal("缺少版本探测命令")
			}

			supported := 0
			for family, p := range profiles {
				if !r.Supports(family) {
					continue
				}
				supported++

				plan, err := r.Plan(context.Background(), p, versions[r.Slug])
				if err != nil {
					t.Fatalf("%s 家族上生成计划失败：%v", family, err)
				}
				if len(plan.Steps) == 0 {
					t.Fatalf("%s 家族上的计划没有任何步骤", family)
				}
				for _, step := range plan.Steps {
					if strings.TrimSpace(step.Command) == "" {
						t.Fatalf("%s 家族上存在空命令的步骤：%s", family, step.Name)
					}
				}
				// 预检要能回答"这一装会动哪些包"：要么给了包，要么给了自定义模拟脚本。
				if len(plan.PkgSpecs) == 0 && plan.SimScript == "" {
					t.Fatalf("%s 家族上的计划既没有包也没有模拟脚本", family)
				}
				// 会顺手启动服务的条目必须有安装后复核脚本：否则"服务没起来"
				// 这种半成品状态会被判成安装成功 —— 而它恰恰是用户最想知道的结论。
				if r.CheckCommand == nil {
					for _, step := range plan.Steps {
						if strings.Contains(step.Name, "开机自启") {
							t.Fatalf("%s 在 %s 上会启动服务，却没有声明安装后复核脚本", r.Slug, family)
						}
					}
				}
			}
			if supported == 0 {
				t.Fatalf("%s 不支持任何家族：这样的条目在界面上永远是灰的", r.Slug)
			}
		})
	}
}

// 网络安全分类是应用中心的一个独立页签：条目数量、顺序与适用场景都要稳定。
// 顺序即界面顺序（按使用频率排列），这里把它钉住，避免后续插入新条目时
// 悄悄打乱用户已经熟悉的排列。
func TestSecurityCatalogOrderAndCopy(t *testing.T) {
	apt := &Profile{Family: FamilyApt, OS: "ubuntu", Version: "24.04", Arch: "amd64"}
	want := []string{
		"nmap", "tcpdump", "ufw", "fail2ban", "clamav",
		"wireguard-tools", "openvpn", "nethogs", "tor", "gnupg",
		"pass", "lynis", "rkhunter", "chkrootkit", "firewalld",
	}

	var got []string
	for _, r := range Catalog() {
		if r.Category != "security" {
			// 系统环境软件只安装不打开：不应带启动元数据，界面因此不会
			// 给它们生成桌面图标或「打开」入口。
			if r.Launch != nil {
				t.Errorf("%s 属于 %s，不应声明打开方式", r.Slug, r.Category)
			}
			continue
		}
		got = append(got, r.Slug)
		if strings.TrimSpace(r.UseCase) == "" {
			t.Errorf("%s 缺少适用场景说明", r.Slug)
		}
		if r.Launch == nil || strings.TrimSpace(r.Launch.Command) == "" {
			t.Errorf("%s 缺少打开命令", r.Slug)
		}
		// 探测命令不能把 stderr 并进 stdout：未安装时 shell 的
		// "command not found" 会被 awk 当成版本号，导致卡片误报已安装。
		if detect := r.DetectCommand(apt); strings.Contains(detect, "2>&1") {
			t.Errorf("%s 的版本探测合并了 stderr，请改用 2>/dev/null 或 command -v 守卫", r.Slug)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("网络安全分类条目或顺序不符合预期：\n got: %v\nwant: %v", got, want)
	}
}

// dsh 的安装计划必须先准备 Node.js，最后才装 dsh 本体。
//
// 这条断言把"Node 一起装"这个决策钉死：dsh 靠 npm 才装得上，而 npm 来自
// Node.js。谁哪天图省事把 Node 那几步摘掉，用户点一次安装只会拿到一行
// "npm: command not found" —— 而那行字出现在几百行安装日志的末尾。
func TestPlanDshIncludesNodeFirst(t *testing.T) {
	recipe := GetRecipe("dsh")
	if recipe == nil {
		t.Fatal("软件目录里应当有 dsh 这一项")
	}

	apt := &Profile{
		Family: FamilyApt, OS: "ubuntu", Version: "24.04", Codename: "noble",
		Arch: "amd64", PkgArch: "x86_64",
	}
	plan, err := recipe.Plan(context.Background(), apt, "")
	if err != nil {
		t.Fatalf("生成计划失败：%v", err)
	}

	install := -1
	for i, step := range plan.Steps {
		if strings.Contains(step.Command, "npm install -g @deepseek-ai/dsh") {
			install = i
		}
	}
	if install < 0 {
		t.Fatal("计划里没有 npm 安装 dsh 的步骤")
	}
	if install != len(plan.Steps)-1 {
		t.Fatalf("npm 安装应当排在最后（前面几步是准备环境），实际在第 %d/%d 步",
			install+1, len(plan.Steps))
	}
	if !plan.Steps[install].Critical {
		t.Fatal("npm 安装步骤应当标为关键：它失败就没有任何可交付的结果")
	}

	// Node.js 的准备步骤必须排在它前面。
	nodePrep := false
	for _, step := range plan.Steps[:install] {
		if strings.Contains(strings.ToLower(step.Command), "node") {
			nodePrep = true
		}
	}
	if !nodePrep {
		t.Fatal("npm 安装之前应当先有准备 Node.js 的步骤")
	}

	// 探测走 npm 自己的全局包列表，而不是 PATH 里的 dsh：面板跑命令是非登录
	// shell，npm 的全局 bin 目录未必在 PATH 上，那样会把"装了"报成没装。
	if !strings.Contains(recipe.DetectCommand(apt), "npm ls -g") {
		t.Fatal("版本探测应当走 npm ls -g")
	}

	// 复核必须回答"能不能跑"，而不只是打个版本号：dsh 背后是一整棵依赖树，
	// 装了一半时命令文件在、执行却失败。
	if recipe.CheckCommand == nil {
		t.Fatal("dsh 应当声明安装后复核脚本")
	}
	if !strings.Contains(recipe.CheckCommand(apt), "command -v dsh") {
		t.Fatal("复核脚本应当先自己定位 dsh，不能直接依赖 PATH")
	}
}
