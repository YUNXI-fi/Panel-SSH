package env

import (
	"errors"
	"fmt"
	"strings"
)

// 本文件处理 PATH 这类"累加语义"的变量。
//
// 为什么不能拿 VarSetScript 去写 PATH：那个写入是**字面量**的 —— 值里写 $PATH
// 也会被当成 $ PATH 这几个字符原样落进文件。于是 export PATH='/a:/b:/usr/bin:/bin'
// 会把当时的 PATH 整串**冻结**在文件里：用户以后用包管理器装的任何工具都不会再
// 自动进 PATH，而这类事故的现象是"昨天还能敲的命令今天找不到了"，极难归因。
//
// 所以 PATH 走这条单独的路：
//
//	# panelssh-path: /var/apps/x/target/bin /home/u/.local/bin
//	export PATH="/var/apps/x/target/bin:/home/u/.local/bin:$PATH"
//
// 两行都由面板整组重写 —— 每次都先删掉旧的一组再写新的一组，因此：
//   - 重复点"加入 PATH"不会像参考脚本那样把同一个目录越堆越多；
//   - 双引号是**故意的**，这里必须让目标机自己展开 $PATH（这是全项目唯一的例外，
//     代价是目录路径里不能出现 $、反引号、双引号、反斜杠 —— 由 ValidateVarPathDir 挡住）；
//   - 标记行让人一眼看出这几条是面板在管，也让"面板管了哪些目录"有唯一答案
//     （解析它，而不是去猜用户手写了什么）。
//
// 重写只删面板自己写下的那两行（标记行 + 紧随其后的赋值行），用户手写的任何行
// 都原样保留。这样也够用：面板那行追加在文件末尾，又用双引号让目标机自己展开
// $PATH，它天然排在用户任何一行之后，"最后一条生效"依然成立。
//
// 为什么必须只删自己那两行 —— 这是踩过的坑：早期判据按 "PATH=" 前缀删行，本意
// 是顺手清掉用户写死的面值行，结果把 ~/.profile 里这种**累加行**也删了：
//
//	if [ -d "$HOME/.local/bin" ] ; then
//	    PATH="$HOME/.local/bin:$PATH"
//	fi
//
// 删掉中间一行就留下一个空的 if 块，bash 报"未预期的记号 fi"，整个文件从此不再
// 执行：用户的环境被改坏，而面板那行排在语法错误之后也永远跑不到，复核只能如实
// 报"没生效"。多留用户一行 PATH= 的代价，远小于删错一行的代价。

// ActionEnvPathAdd 是把一个目录加入 PATH 的动作。
//
// 不叫 env-set 加特例：语义完全不同（一个写入面值，一个往集合里加一项），
// 混在一起会让"用户填的值里出现 $PATH 怎么办"变成一个隐式分叉。
const ActionEnvPathAdd = "env-path-add"

// varPathMarker 是面板管理的那组 PATH 行的标记前缀。
const varPathMarker = "# panelssh-path:"

// VarPathName 是 PATH 的名字。
//
// 它不是"又一个变量名"：写它必须整组重写、并且让目标机自己展开 $PATH（见文件头），
// 而字面量写入会把它冻结、按 "PATH=" 前缀删行会破坏用户的启动文件（见
// ValidateVarWritable）。凡是需要认出它的地方都用这个常量，让这些约定只有一个来源。
const VarPathName = "PATH"

// ValidateVarPathDir 校验一个要写进 PATH 的目录。
//
// 这份校验不是形式主义：目录会被拼进双引号包围的赋值行，$、反引号、双引号、
// 反斜杠在里面都会被 shell 解释。这些目录来自扫描结果或前端选择，都不是可信输入。
func ValidateVarPathDir(dir string) error {
	if dir == "" {
		return errors.New("目录不能为空")
	}
	if !strings.HasPrefix(dir, "/") {
		return fmt.Errorf("必须是绝对路径：%s", dir)
	}
	if strings.ContainsAny(dir, "$\"`\\") || strings.ContainsAny(dir, " \t\n\r") {
		return fmt.Errorf("路径里不能出现空格、引号、$ 或反斜杠：%s", dir)
	}
	if strings.Contains(dir, ":") {
		return fmt.Errorf("路径里不能出现冒号（PATH 用冒号分隔）：%s", dir)
	}
	return nil
}

// VarPathDirs 从文件内容里解析出面板已经管了哪些 PATH 目录。
//
// 只认标记行：不去猜用户手写的 export PATH= 里有什么 —— 那一行可能是
// PATH="$PATH:$HOME/bin" 这种需要求值才说得清的形式，猜错就会把目录弄丢。
func VarPathDirs(content []byte) []string {
	var dirs []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(content), "\n") {
		text := strings.TrimSpace(line)
		if !strings.HasPrefix(text, varPathMarker) {
			continue
		}
		for _, dir := range strings.Fields(strings.TrimPrefix(text, varPathMarker)) {
			if dir == "" || seen[dir] {
				continue
			}
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// varPathDropAwk 只删面板自己写下的两行：标记行，以及紧跟它的那一行 PATH 赋值。
//
// 用"上一行是不是标记行"的状态位（marker）做判据，而不是按 "PATH=" 前缀做全局
// 匹配 —— 后者的代价见文件头：用户的累加行同样以 PATH= 开头，删掉会让 if 块变空、
// 整个文件语法错误。这里宁可漏删用户的行（无害，面板那行在后面照样生效），
// 也不能多删一行。
//
// 另外它丢掉文件末尾的空白行：面板写的那个块前面有一个空行（用来与原有内容隔开），
// 不一起丢掉的话每写一次就多一个空行。中间的空行原样保留 —— 那是用户文件自己的排版，
// 面板没有理由去压。
const varPathDropAwk = `{
  line = $0
  key = line
  sub(/^[ \t]+/, "", key)
  if (marker) {
    marker = 0
    if (key ~ /^(export[ \t]+)?PATH[ \t]*=/) next
  }
  if (key ~ /^# panelssh-path:/) { marker = 1; next }
  if (key == "") { blanks++; next }
  while (blanks > 0) { print ""; blanks-- }
  print line
}`

// VarPathSetScript 生成"把这一组目录写进 PATH"的脚本。
//
// dirs 是**完整的目标集合**（不是增量）：调用方已经从备份内容里读出面板原先管着
// 哪些目录，加上新目录后传进来。脚本只负责"清掉旧的一组、写下新的一组"，
// 不做集合运算 —— 那部分逻辑留在面板侧，才能被单元测试直接覆盖。
func VarPathSetScript(path string, dirs []string) (string, error) {
	if len(dirs) == 0 {
		return "", errors.New("PATH 目录列表为空")
	}
	unique := make([]string, 0, len(dirs))
	seen := map[string]bool{}
	for _, dir := range dirs {
		if err := ValidateVarPathDir(dir); err != nil {
			return "", err
		}
		if seen[dir] {
			continue
		}
		seen[dir] = true
		unique = append(unique, dir)
	}

	marker := varPathMarker + " " + strings.Join(unique, " ")
	// 双引号是故意的（见文件头）：这里必须让目标机展开 $PATH。
	assign := `export PATH="` + strings.Join(unique, ":") + `:$PATH"`

	return `
set -e
F=` + shellSingleQuote(path) + `
TMP="$F.panelssh.$$.tmp"
if [ -f "$F" ]; then
  awk '` + varPathDropAwk + `' "$F" > "$TMP"
else
  : > "$TMP"
fi
printf '\n%s\n%s\n' ` + shellSingleQuote(marker) + ` ` + shellSingleQuote(assign) + ` >> "$TMP"
cat "$TMP" > "$F"
rm -f "$TMP"
echo '===END==='
true`, nil
}