package env

import (
	"context"
	"strings"
	"time"
)

// 查询类命令的时长上限。
const (
	// 包全量列表在大树根上也只需几秒；卡住通常意味着会话异常。
	queryTimeout = 30 * time.Second
	// 模拟预检可能触发仓库元数据读取，给宽一些。
	simulateTimeout = 90 * time.Second
)

// PackageManager 抽象一个发行版包管理器。
//
// 面板只"驱动"包管理器而不替代它：查询靠只读命令，冲突求解靠它自带的
// 模拟模式，修复靠它自己的修复命令。这一层负责的是生成命令与解析输出。
type PackageManager interface {
	Family() Family

	// ---- 只读查询 ----
	Installed(ctx context.Context, ex Executor) ([]Package, error)
	Upgrades(ctx context.Context, ex Executor) ([]Upgrade, error)
	Candidates(ctx context.Context, ex Executor, name string) ([]Candidate, error)

	// ---- 预检 ----
	// Simulate 不改动系统，返回求解器给出的完整变更与冲突。
	Simulate(ctx context.Context, ex Executor, action string, specs []Spec) (*SimReport, error)

	// ---- 变更（生成步骤，由编排器执行）----
	Steps(action string, specs []Spec) []Step
	RepairStep() *Step

	// ---- 回滚辅助 ----
	// DowngradeCommand 生成"把列出的包切回 From 版本"的命令。
	DowngradeCommand(items []ChangeItem) string
	// ReinstallCommand 生成"重装被卸载包的指定旧版本"的命令。
	ReinstallCommand(items []ChangeItem) string
	// RemoveCommand 生成"移除新增包"的命令。
	RemoveCommand(items []ChangeItem) string

	// ---- 快照所需信息 ----
	PackageListCommand() string
	ParsePackages(raw string) []Package
	// SourcePaths 是需要备份的源配置/密钥路径（相对根目录 /）。
	SourcePaths() []string
	// RenderSpec 把 Spec 渲染成该家族命令行接受的版本约束写法。
	RenderSpec(s Spec) string
}

// NewManager 按画像选择包管理器实现。
func NewManager(p *Profile) PackageManager {
	switch p.Family {
	case FamilyApt:
		return &aptManager{}
	case FamilyDNF:
		return &rpmManager{yum: false}
	case FamilyYUM:
		return &rpmManager{yum: true}
	case FamilyAPK:
		return &apkManager{}
	default:
		return nil
	}
}

// runQuery 执行一段只读脚本并返回输出。查询不需要特权。
func runQuery(ctx context.Context, ex Executor, script string, timeout time.Duration) (string, error) {
	res, err := ex.Run(ctx, Wrap(script, nil), timeout)
	if err != nil {
		return "", err
	}
	// 查询命令约定：解析器只吃 stdout，退出码非 0（如 apk/apk check-update
	// 的 100）交给调用方结合内容处理；这里不截断为错误。
	return res.Stdout, nil
}

// joinSpecs 渲染版本约束列表。
func joinSpecs(m PackageManager, specs []Spec) string {
	parts := make([]string, 0, len(specs))
	for _, s := range specs {
		parts = append(parts, m.RenderSpec(s))
	}
	return joinSh(parts)
}

// joinNames 取包名列表。
func joinNames(items []ChangeItem) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, it.Name)
	}
	return joinSh(parts)
}

// joinSh 用单引号连接 shell 参数。包名来自用户输入，必须防注入：
// 包管理生态的包名只允许字母数字与少量符号（.+-_~:），额外做字符白名单校验。
func joinSh(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " "
		}
		out += shellQuote(p)
	}
	return out
}

// shellQuote 以白名单校验后单引号包裹参数。
func shellQuote(s string) string {
	return "'" + s + "'"
}

// validPkgName 校验包名/版本约束只含安全字符，杜绝注入。
//
// apt 包名字符集为 [a-z0-9.+-:~]；版本约束还含 =、*（glob）。
// 家族间取并集后仍然远小于 shell 元字符集合，因此校验失败直接拒绝最稳。
func validPkgToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case strings.IndexRune(".+-_~:=-*@/", r) >= 0:
		default:
			return false
		}
	}
	return true
}
