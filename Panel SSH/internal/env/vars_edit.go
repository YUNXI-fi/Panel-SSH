package env

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 本文件是环境变量的写入侧：决定"写到哪个文件"、生成改写脚本、生成备份与还原脚本。
//
// 两条硬规矩：
//
//  1. **只写面板自己的文件**。系统级固定写 /etc/profile.d/panel-env.sh，绝不去改
//     /etc/environment、/etc/profile 或别人 profile.d 里的文件 —— 那些文件里
//     可能混着发行版与管理员自己的逻辑，按行删改是在替别人做决定。
//     用户级只允许写家目录下那几个 shell 启动文件。
//     因此"修改一个已存在于别处的变量"在语义上是**覆盖**（在更晚被读取的文件里
//     写一个定义），界面上也是这么说的。
//
//  2. **备份先于改写**。改写脚本在 awk 写出临时文件之前不动原文件，先用 cat 覆盖
//     而不是 mv —— 见 varSetScript 的注释。

// 环境变量的写操作动作名。与包管理的动作同处 env_ops，界面按动作名给中文标签。
const (
	ActionEnvSet   = "env-set"
	ActionEnvUnset = "env-unset"
	// ActionEnvRestore 是"把某个变量文件还原成某次操作前的样子"，
	// 也就是环境变量变更的回滚动作。它同样登记成一条操作记录（origin_op_id
	// 指向被还原的那次），因此回滚本身也可追溯、可再回滚。
	ActionEnvRestore = "env-restore"
)

// VarPlan 是一次环境变量写入的计划存档（存入 env_ops.plan_json）。
//
// 它同时承担两件事：给界面展示"这次改动写在哪个文件"，以及让事后回滚知道
// 该把哪个文件还原成什么样。
type VarPlan struct {
	Action string   `json:"action"`
	Kind   string   `json:"kind"`
	Target string   `json:"target"`
	Steps  []string `json:"steps"`
	Name   string `json:"name"`
	// Value 是写入的值（删除操作为空）。
	Value string `json:"value,omitempty"`
	Scope string `json:"scope"`
	File  string `json:"file"`
	// 这里刻意不记"改写前文件是否存在"：那个事实必须由备份目录里的标记文件回答
	// （见 WriteVarBackup）。计划存档是面板侧的记录，与目标机的实际状态没有绑定
	// 关系 —— 拿它当依据去决定"该删还是该写回"，一旦两者不一致就会删错文件。
	Note string `json:"note,omitempty"`
}

// SystemVarFile 是系统级变量的落地文件。
//
// 为什么不写 /etc/environment：那个文件不是 shell 脚本，由 PAM 直接按 key=value
// 解析，不支持引号与变量展开；而 /etc/profile.d 是各发行版公认的"放系统级环境
// 变量"的位置，且在 /etc/profile 之后被读取 —— 在这里写一个更晚的定义，能稳定
// 覆盖掉 /etc/environment 里已有的同名变量。
const SystemVarFile = "/etc/profile.d/panel-env.sh"

// userVarFiles 是允许写入的用户级文件。顺序即界面上的推荐顺序。
var userVarFiles = []string{".profile", ".bashrc", ".bash_profile", ".bash_login"}

// UserVarFiles 返回用户级可选文件（家目录相对名），供界面展示候选。
func UserVarFiles() []string { return append([]string(nil), userVarFiles...) }

// DefaultUserVarFile 是用户级变量的默认落地文件。
//
// 选 ~/.profile 而不是 ~/.bashrc：登录 shell 读的是它，而且 Debian/Ubuntu 的
// ~/.profile 会再去 source ~/.bashrc —— 写这里对"登录后有、非交互脚本里也有"，
// 写进 ~/.bashrc 则会被它开头那段"非交互直接 return"挡在门外。
const DefaultUserVarFile = ".profile"

// VarTargetFile 校验并归一化目标文件。
//
// requested 来自前端（用户在下拉里选的文件），因此必须按白名单校验：
// 这是唯一一处"用户输入直接决定在目标机上写哪个路径"的地方。
func VarTargetFile(scope, home, requested string) (string, error) {
	if scope == ScopeSystem {
		if requested == "" {
			return SystemVarFile, nil
		}
		// 系统级只认这一个文件：允许前端指定其它 /etc 路径等于把"删改别人的
		// 配置文件"变成一次点击。
		if requested != SystemVarFile {
			return "", fmt.Errorf("系统级变量只能写入 %s", SystemVarFile)
		}
		return SystemVarFile, nil
	}
	if scope != ScopeUser {
		return "", fmt.Errorf("未知作用域：%s", scope)
	}
	base := strings.TrimSuffix(home, "/")
	if base == "" {
		return "", errors.New("读不到目标机的家目录，无法写入用户级变量")
	}
	if requested == "" {
		requested = base + "/" + DefaultUserVarFile
	}
	for _, name := range userVarFiles {
		if requested == base+"/"+name {
			return requested, nil
		}
	}
	return "", fmt.Errorf("用户级变量只能写入 %s 下的 %s", base, strings.Join(userVarFiles, "、"))
}

// VarBackupScript 读取目标文件内容（base64）并报告它是否存在。
//
// 与包管理快照同一个约定：内容回到面板侧落盘，回滚时再送回去 ——
// 备份放在目标机上会污染被管理的机器，也不受面板的保留策略约束。
func VarBackupScript(path string) string {
	q := shellSingleQuote(path)
	return `
export LC_ALL=C
echo '===EXISTS==='
if [ -e ` + q + ` ]; then echo yes; else echo no; fi
echo '===CONTENT_B64==='
if [ -e ` + q + ` ]; then base64 < ` + q + ` | tr -d '\n'; fi
echo
echo '===END==='
true`
}

// ParseVarBackup 解析备份脚本的输出。
func ParseVarBackup(out string) (content []byte, existed bool, err error) {
	sec := splitSections(out)
	existed = strings.TrimSpace(sec["EXISTS"]) == "yes"
	b64 := strings.TrimSpace(sec["CONTENT_B64"])
	if b64 == "" {
		return nil, existed, nil
	}
	decoded, derr := base64.StdEncoding.DecodeString(b64)
	if derr != nil {
		return nil, existed, fmt.Errorf("备份内容解码失败：%w", derr)
	}
	return decoded, existed, nil
}

// varMatchAwk 是"找出文件里所有对该变量的赋值行"的 awk 程序，写入与删除共用。
//
// 它会跳过前导空白与 export 前缀 —— 用户手写的文件里 `export FOO=1`、
// `  FOO=1`、`export FOO = 1`（这种不合法，不管）都按同一条处理，
// 否则改写会留下一行没删掉的旧定义，新值随文件读取顺序被打回原形。
//
// 只打印行内容不打印行号：这里的匹配结果要直接与"面板期望写入的那一行"做
// 字符串比对（见 VarSetExpectation），多一个行号前缀就永远比不中了。
const varMatchAwk = `{
  line = $0
  sub(/^[ \t]+/, "", line)
  if (line ~ /^export[ \t]+/) sub(/^export[ \t]+/, "", line)
  if (line ~ "^" n "[ \t]*=") print $0
}`

// varDropAwk 与 varMatchAwk 判据一致，但做的是"删掉这些行、其余原样保留"。
const varDropAwk = `{
  line = $0
  sub(/^[ \t]+/, "", line)
  if (line ~ /^export[ \t]+/) sub(/^export[ \t]+/, "", line)
  if (line ~ "^" n "[ \t]*=") next
  print
}`

// ValidateVarWritable 判定一个变量能不能走"设置变量 / 删除变量"这条路。
//
// 现在只挡 PATH，两条理由各自都足以让它不能这样改：
//
//  1. 这里的写入是**字面量**的：用户填什么就写什么。对 PATH 而言，这等于把当前的
//     PATH 抄进文件存死 —— 以后用包管理器装的工具不会再自动进 PATH（见 vars_path.go
//     的文件头）。系统里唯一合法的 PATH 写入形态是"整组重写 + 双引号展开 $PATH"。
//  2. 删除侧的判据按 "PATH=" 前缀删掉文件里所有 PATH 定义，其中包含用户写在
//     if [ -d "$HOME/.local/bin" ] ; then PATH="$HOME/.local/bin:$PATH" ; fi
//     里的**累加行** —— 删掉就留下一个空的 if 块，bash 报语法错误，整个启动文件
//     从此不再执行。那条判据本身是对的（同一个变量留两处定义会互相打架），
//     错的是把 PATH 交给它处理。
//
// 于是 PATH 只走 vars_path.go 那条路：带标记行、整组重写，且只动面板自己写的行。
func ValidateVarWritable(name string) error {
	if name == VarPathName {
		return errors.New("PATH 不能用「设置变量 / 删除变量」改：字面量写入会把当前的 PATH 冻结进文件，" +
			"而删除会连用户写在 if 判断里的累加行一起删掉，让整个启动文件语法错误。" +
			"要往 PATH 里加目录，请用环境扫描里的「加入 PATH」")
	}
	return nil
}

// VarSetScript 生成"把变量写进文件"的脚本：先删掉该变量在这个文件里的旧定义，
// 再在末尾追加一行。
//
// 为什么是"删旧 + 追加"而不是原地替换：同一个变量在这个文件里可能出现多次
// （历史上追加过几回），只替换第一处会留下一行更晚生效的旧值 —— 用户看到的是
// "我改了，但值没变"。清干净再追加，"最后一条定义"就一定是面板写的那条。
//
// 最后用 cat 覆盖回原文件而不是 mv：mv 会把文件属主与权限换成执行者。
// 系统级由 root 执行，mv 会把 /etc/profile.d 下的文件属主改成 root（本来就该是
// root，看不出问题），但用户级一旦走了提权路径就会把用户的家目录文件改成 root 所有，
// 那台机器上该用户下次登录可能就读不到了。cat 只改内容，inode、属主、权限都不动。
//
// 变量名在写入行里**不加引号**（export FOO='bar'）：写成 export 'FOO'='bar'
// 语法上也能跑，但 varDropAwk 的判据是"去掉 export 前缀后以 FOO= 开头"，带引号的
// 名字匹配不上 —— 后果是下次再改同一个变量时删不掉这一行，旧值越堆越多。
// 名字已由 ValidVarName 校验为裸标识符，不带引号是安全的。
func VarSetScript(path, name, value string) (string, error) {
	if !ValidVarName(name) {
		return "", fmt.Errorf("变量名不合法：%s", name)
	}
	if err := ValidateVarValue(value); err != nil {
		return "", err
	}
	q, qn := shellSingleQuote(path), shellSingleQuote(name)
	// 写入行在面板侧就拼成最终形态，再整体当成一个单引号字面量交给 printf。
	//
	// 不能把 shellSingleQuote(value) 直接喂给 %s：那对引号是给 shell 看的语法，
	// shell 解析参数时就会把它剥掉，文件里落成裸值 —— 值里的 $、空格、分号从此
	// 有了别的含义（PATH=$PATH:/opt 会被再次展开，这与"用户填的是字面值"相悖）。
	// 写成字面量参数，printf 才会把它一个字节不改地打出来。
	line := shellSingleQuote("export " + name + "=" + shellSingleQuote(value))
	return `
set -e
F=` + q + `
TMP="$F.panelssh.$$.tmp"
if [ -f "$F" ]; then
  awk -v n=` + qn + ` '` + varDropAwk + `' "$F" > "$TMP"
else
  : > "$TMP"
fi
printf '\n%s\n' ` + line + ` >> "$TMP"
cat "$TMP" > "$F"
rm -f "$TMP"
echo '===MATCHES==='
awk -v n=` + qn + ` '` + varMatchAwk + `' "$F"
echo '===END==='
true`, nil
}

// VarUnsetScript 生成"从文件里删掉变量"的脚本。
//
// 只删面板可写文件里的定义。变量若同时定义在 /etc/environment 或别人的
// profile.d 里，删完它仍然生效 —— 这一点由调用方在结果里如实告知，
// 不能在界面上假装删干净了。
func VarUnsetScript(path, name string) (string, error) {
	if !ValidVarName(name) {
		return "", fmt.Errorf("变量名不合法：%s", name)
	}
	q, qn := shellSingleQuote(path), shellSingleQuote(name)
	return `
set -e
F=` + q + `
if [ ! -f "$F" ]; then
  echo '===MATCHES==='
  echo '===END==='
  exit 0
fi
TMP="$F.panelssh.$$.tmp"
awk -v n=` + qn + ` '` + varDropAwk + `' "$F" > "$TMP"
cat "$TMP" > "$F"
rm -f "$TMP"
echo '===MATCHES==='
awk -v n=` + qn + ` '` + varMatchAwk + `' "$F"
echo '===END==='
true`, nil
}

// VarMatches 从写入/删除脚本的输出里取出仍匹配该变量的行（已去掉首尾空白）。
func VarMatches(out string) []string {
	raw := splitSections(out)["MATCHES"]
	lines := make([]string, 0, 2)
	for _, line := range strings.Split(raw, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
}

// VarSetExpectation 返回写入后应当出现的那一行原文，供复核比对。
func VarSetExpectation(name, value string) string {
	return "export " + name + "=" + shellSingleQuote(value)
}

// VarRestoreScript 生成回滚脚本：把文件还原成备份内容。
//
// existed 为假表示改写前这个文件不存在 —— 那它多半是面板为了写变量而建的
// （/etc/profile.d/panel-env.sh 就是典型），回滚时应当删掉它，
// 而不是写回一份空文件：空文件会继续参与 shell 启动，虽然内容为空，
// 但"面板留下的痕迹"没有被清掉。
func VarRestoreScript(path string, content []byte, existed bool) string {
	q := shellSingleQuote(path)
	if !existed {
		return `
set -e
rm -f ` + q + `
true`
	}
	b64 := base64.StdEncoding.EncodeToString(content)
	return `
set -e
F=` + q + `
TMP=$(mktemp)
cat > "$TMP" <<'PANELSSH_ENV_B64'
` + b64 + `
PANELSSH_ENV_B64
base64 -d "$TMP" > "$TMP.raw"
cat "$TMP.raw" > "$F"
rm -f "$TMP" "$TMP.raw"
true`
}

// ValidateVarValue 校验待写入的值。
//
// 拒绝换行：值会以单引号字面量写进 shell 文件，含换行虽然语法上可行，
// 但会让文件变得难以人工核对，而环境变量里出现换行的场景几乎都是误粘贴。
// 上限 4KB：shell 启动文件会被每个登录会话读一遍，塞进超大字符串是在给
// 目标机的每次登录加负担。
func ValidateVarValue(value string) error {
	if strings.ContainsAny(value, "\n\r") {
		return errors.New("值里不能有换行")
	}
	if len(value) > 4096 {
		return errors.New("值过长（上限 4096 字节）")
	}
	return nil
}

// VarBackupFileName 与 VarAbsentFileName 是操作快照目录里的备份文件。
//
// 与包管理快照同一个约定：备份落在面板侧目录（<DataDir>/env/host-N/snapshots/op-N），
// 不落在目标机上 —— 备份是最该与被管理的机器隔开的东西。
//
// 用两个文件名而不是"内容为空即不存在"来区分情形：文件原本不存在与原本是个空文件
// 是两件不同的事，还原时的动作也不同（删掉 vs 写回空内容）。空文件虽然内容为空，
// 却会继续参与每个登录会话的启动 —— "面板留下的痕迹"并没有被清掉。
const (
	VarBackupFileName = "var-backup.b64"
	VarAbsentFileName = "var-absent"
)

// WriteVarBackup 把变更前的文件内容写进快照目录。
//
// 存 base64 文本而不是原始字节：原文件可能没有结尾换行、也可能含非文本内容，
// base64 能把字节精确保留下来（还原脚本按字节写回），也便于人工打开核对。
func WriteVarBackup(dir string, content []byte, existed bool) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if !existed {
		return os.WriteFile(filepath.Join(dir, VarAbsentFileName), nil, 0o600)
	}
	encoded := base64.StdEncoding.EncodeToString(content)
	return os.WriteFile(filepath.Join(dir, VarBackupFileName), []byte(encoded), 0o600)
}

// ReadVarBackup 读回某个操作留下的文件备份，同时回答"它当时是否存在"。
func ReadVarBackup(dir string) ([]byte, bool, error) {
	if _, err := os.Stat(filepath.Join(dir, VarAbsentFileName)); err == nil {
		return nil, false, nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, VarBackupFileName))
	if err != nil {
		return nil, false, fmt.Errorf("读取变量文件备份失败：%w", err)
	}
	content, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, false, fmt.Errorf("变量文件备份解码失败：%w", err)
	}
	return content, true, nil
}