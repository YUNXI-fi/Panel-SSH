package env

import (
	"context"
	"fmt"
	"strings"
)

// verifyItem 是复核项。
//
// Kind：
//   - presence：包应当已安装（Expect 非空时校验版本）；
//   - gone：包应当已被卸载；
//   - recipe：目录软件，用 Detect 命令取版本；
//   - recipe-removed：目录软件应当彻底不存在（tarball 安装的 Go）；
//   - check：目录软件自带复核脚本（Check），脚本输出 OK 表示通过，
//     其它内容原样作为问题描述 —— 数据库这类"装上了不等于能用"的条目靠它；
//   - upgrade-all：整机应已无待升级包。
type verifyItem struct {
	Name   string
	Expect string
	Kind   string
	Detect string
	// Check 是 Kind 为 check 时的复核脚本：输出 OK 表示通过，
	// 其它输出被当作失败原因带到界面上。
	Check string
}

// verifyScript 生成复核脚本，输出 name<TAB>expect<TAB>actual 行；
// actual 为 "-" 表示目标不存在。只读、不需要特权。
func verifyScript(p *Profile, items []verifyItem) string {
	var b strings.Builder
	b.WriteString("export LC_ALL=C\n")
	b.WriteString("emit() { printf '%s\\t%s\\t%s\\n' \"$1\" \"$2\" \"$3\"; }\n")

	// 只要有一个自带复核脚本的条目，就把"服务是否在跑"的判定放进脚本：
	// systemd 与 OpenRC 各有各的查询方式，包一层让上层脚本只管问 yes/no。
	// 走 service 而不是只认 systemctl：Alpine 上根本没有 systemctl。
	if hasCheckItem(items) {
		b.WriteString(`
svc_active() { systemctl is-active --quiet "$1" 2>/dev/null || service "$1" status >/dev/null 2>&1; }
`)
	}

	switch p.Family {
	case FamilyApt:
		b.WriteString(`
pkgver() {
  out=$(dpkg-query -W -f='${db:Status-Abbrev}\t${Version}' "$1" 2>/dev/null)
  case "$out" in
    ii*|hi*) echo "$out" | cut -f2 ;;
    *) echo "-" ;;
  esac
}
`)
	case FamilyDNF, FamilyYUM:
		b.WriteString(`
pkgver() {
  out=$(rpm -q --qf '%|EPOCH?{%{EPOCH}:}:{}|%{VERSION}-%{RELEASE}' -- "$1" 2>/dev/null)
  [ -z "$out" ] && echo "-" || echo "$out"
}
`)
	case FamilyAPK:
		b.WriteString(`
pkgver() {
  if apk info -e "$1" >/dev/null 2>&1; then
    apk list --installed 2>/dev/null | sed -n "s/^$1-\([^ ]*\) .*/\1/p" | head -1
  else
    echo "-"
  fi
}
`)
	}

	for _, it := range items {
		switch it.Kind {
		case "presence", "gone":
			fmt.Fprintf(&b, "emit %s %s \"$(pkgver %s)\"\n",
				shellQuote(it.Name), shellQuote(it.Expect), shellQuote(it.Name))
		case "recipe":
			// Detect 是目录项给出的版本输出命令，包一层取值即可。
			fmt.Fprintf(&b, "v=$(%s 2>/dev/null | head -1); [ -z \"$v\" ] && v='-'; emit %s %s \"$v\"\n",
				it.Detect, shellQuote(it.Name), shellQuote(it.Expect))
		case "recipe-removed":
			fmt.Fprintf(&b, "if [ -e /usr/local/go ]; then emit %s %s present; else emit %s %s -; fi\n",
				shellQuote(it.Name), shellQuote(it.Expect), shellQuote(it.Name), shellQuote(it.Expect))
		case "check":
			// 自带复核脚本：它自己决定"什么叫做成了"，这里只负责把结论带回去。
			// 取最后 3 行并把换行/制表符压成空格：解析侧按"一行三列"读结果，
			// 多行或多列的原因会静默丢掉后半截。
			fmt.Fprintf(&b, "out=$(%s 2>&1 | tail -3 | tr '\\n\\t' '  ')\n", it.Check)
			b.WriteString("[ -z \"$out\" ] && out='复核脚本没有返回结果'\n")
			fmt.Fprintf(&b, "emit %s '' \"$out\"\n", shellQuote(it.Name))
		case "upgrade-all":
			b.WriteString(upgradeAllCheck(p.Family))
		}
	}
	return b.String()
}

// hasCheckItem 判断复核项里有没有"自带脚本"的条目。
func hasCheckItem(items []verifyItem) bool {
	for _, it := range items {
		if it.Kind == "check" && it.Check != "" {
			return true
		}
	}
	return false
}

// recipeVerifyItem 决定一次目录软件安装之后怎么复核。
//
// 优先级：目录项自带的复核脚本（CheckCommand）> 计划里声明的版本命令 > 默认版本探测。
//
// 只比版本号是不够的："装上了"不等于"能用"。数据库、Web 服务器这类服务没起来时，
// 版本命令照样打出版本，而这恰恰是用户最需要知道的结论。
func recipeVerifyItem(r *Recipe, rp *RecipePlan, p *Profile, version string) verifyItem {
	// 复核项的名字会直接出现在界面的结论里，因此用人话名称（node/postgres）
	// 而不是内部 slug。
	label := rp.VerifyLabel
	if label == "" {
		label = r.Name
	}

	if r.CheckCommand != nil {
		return verifyItem{Name: label, Kind: "check", Check: r.CheckCommand(p)}
	}

	detect := rp.Verify
	if detect == "" {
		detect = r.DetectCommand(p)
	}
	return verifyItem{Name: label, Expect: version, Kind: "recipe", Detect: detect}
}

// upgradeAllCheck 输出整机残留升级数（apt/apk）或求解器退出码（rpm 系）。
func upgradeAllCheck(f Family) string {
	switch f {
	case FamilyApt:
		return `n=$(apt list --upgradable 2>/dev/null | grep -c '/'); emit "__all__" "" "$n"
`
	case FamilyAPK:
		return `n=$(apk version -l '<' 2>/dev/null | grep -c '<'); emit "__all__" "" "$n"
`
	default:
		// dnf/yum check-update：100=有更新，0=无更新。
		bin := "dnf"
		if f == FamilyYUM {
			bin = "yum"
		}
		return `set +e
` + bin + ` check-update -q >/dev/null 2>&1
emit "__all__" "" "$?"
`
	}
}

// verify 执行复核并汇总结论。verified 为假时 note 列出不通过项。
func (r *Runner) verify(ctx context.Context, ex Executor, p *Profile, mgr PackageManager, items []verifyItem) (bool, string) {
	if len(items) == 0 {
		return true, ""
	}
	res, err := ex.Run(ctx, Wrap(verifyScript(p, items), nil), queryTimeout)
	if err != nil {
		return false, "复核命令执行失败：" + err.Error()
	}

	// 以请求项为主键对齐输出行，缺行直接判失败（脚本被裁剪/命令缺失）。
	actuals := map[string]string{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) == 3 {
			actuals[fields[0]] = fields[2]
		}
	}

	problems := make([]string, 0)
	for _, it := range items {
		actual, ok := actuals[it.Name]
		if !ok {
			problems = append(problems, it.Name+"：未取得复核结果")
			continue
		}
		switch it.Kind {
		case "presence":
			if actual == "-" {
				problems = append(problems, it.Name+"：安装后仍查询不到")
			} else if it.Expect != "" && !versionOK(p.Family, it.Expect, actual) {
				problems = append(problems, fmt.Sprintf("%s：版本不符（期望 %s，实际 %s）", it.Name, it.Expect, actual))
			}
		case "gone":
			if actual != "-" {
				problems = append(problems, fmt.Sprintf("%s：仍已安装（%s）", it.Name, actual))
			}
		case "recipe":
			if actual == "-" {
				problems = append(problems, it.Name+"：安装后未检测到")
			} else if it.Expect != "" && !recipeVersionOK(it.Expect, actual) {
				problems = append(problems, fmt.Sprintf("%s：版本不符（期望 %s 系列，实际 %s）", it.Name, it.Expect, actual))
			}
		case "recipe-removed":
			if actual != "-" {
				problems = append(problems, "/usr/local/go 仍然存在，旧版本本应未安装 Go")
			}
		case "check":
			// 脚本自己给的结论：OK 是通过，其它内容就是失败原因。
			// 比较前必须去掉首尾空白：脚本把换行压成了空格，OK 后面会剩一个尾空格 ——
			// 不处理的话每一次复核都会误报未通过。
			reason := strings.TrimSpace(actual)
			if reason != "OK" {
				problems = append(problems, it.Name+"："+reason)
			}
		case "upgrade-all":
			pending := false
			if p.Family == FamilyDNF || p.Family == FamilyYUM {
				pending = actual == "100"
			} else {
				pending = actual != "0"
			}
			if pending {
				problems = append(problems, "仍有软件包可以升级")
			}
		}
	}
	return len(problems) == 0, strings.Join(problems, "；")
}

// versionOK 判断已装版本是否满足包级版本期望。
//
// apt 的 pkg=ver 是精确匹配；dnf/yum 的 pkg-ver 接受前缀（NEVR 模糊）；
// 以 * 结尾的期望按通配处理（apt glob）。
func versionOK(family Family, expect, actual string) bool {
	if expect == "" || expect == "*" || actual == expect {
		return true
	}
	if strings.HasSuffix(expect, "*") {
		return strings.HasPrefix(actual, strings.TrimSuffix(expect, "*"))
	}
	if family == FamilyDNF || family == FamilyYUM {
		return strings.HasPrefix(actual, expect)
	}
	return false
}

// recipeVersionOK 处理"大版本系列"期望（Node 22 → 22.x；Java 21 → 21.x），
// 精确版本（Go 1.26.1）走相等分支。
func recipeVersionOK(expect, actual string) bool {
	return actual == expect || strings.HasPrefix(actual, expect+".")
}
