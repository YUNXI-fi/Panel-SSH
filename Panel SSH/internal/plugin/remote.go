package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
)

// 插件在目标机上的落位根目录，取用户态路径。
//
// 之所以不需要 root：面板登录的账号要能在目标机上跑插件的命令，插件就必须是
// 这个账号能读到的东西 —— 放进 root 才可写的位置，等于把"装插件"变成一次提权
// 操作，而这套体系里没有任何理由走那条路。
const RemoteRoot = ".panelssh/plugins"

// remotePushTimeout 是远端落位整条链路的上限。
//
// 解压本身很快，耗时几乎全在把包传过去；2 分钟足够一台正常网络的目标机收下一个
// 32 MiB 的包。超过这个时间基本可以确定链路有问题，早失败比挂在那里等更好。
const remotePushTimeout = 2 * time.Minute

// maxRemoteOutput 限制落位脚本回传的输出长度。
const maxRemoteOutput = 64 << 10

// RemoteEnv 是推送到目标主机所需的全部外部能力。
//
// 由调用方（api 包）实现，因为"怎么连上一台主机"是 api 层的事，plugin 包
// 不该反过来依赖 sshclient / sftp。这里只声明它需要什么。
type RemoteEnv interface {
	// HomeDir 返回目标机上登录账号的绝对家目录。
	HomeDir(ctx context.Context) (string, error)
	// Upload 把 src 的内容写到远端路径 dst，父目录不存在时自行创建。
	Upload(ctx context.Context, dst string, src io.Reader, size int64) error
	// Exec 执行一段 shell 脚本，返回 stdout、stderr 与退出码。
	// 脚本非零退出时返回的 err 为 nil —— 退出码是业务结果，不是通信故障，
	// 交给调用方判断才能给出准确的话术。
	Exec(ctx context.Context, script string) (stdout, stderr string, exitCode int, err error)
}

// PushToRemote 把安装包推送到目标主机并落位。
//
// 传的是安装包原文而不是本地解压后的目录：传输量小得多，也要紧的是目标机上
// 落的这份必须与本地镜像来自同一个字节流 —— 两边各自解压一次，校验与落位逻辑
// 才只有一份（本地的 Stage 与远端的 unzip 都在解同一个包）。
//
// 落位不完整时整体回滚，任何失败只留下一句错误，不留半个目录。
func PushToRemote(ctx context.Context, env RemoteEnv, archive []byte, manifest *Manifest) error {
	if env == nil {
		return errors.New("目标主机执行环境不可用")
	}
	if manifest == nil {
		return errors.New("插件清单缺失")
	}
	// 这两个标识会参与远端路径拼接，必须先确认它们不含路径分隔符。
	// 清单在校验阶段已经卡过一遍，这里是"拼路径之前再确认一次"的兜底。
	if !identifierPattern.MatchString(manifest.Org) || !identifierPattern.MatchString(manifest.Name) ||
		!versionPattern.MatchString(manifest.Version) {
		return errors.New("插件标识非法，拒绝写入目标主机")
	}

	ctx, cancel := context.WithTimeout(ctx, remotePushTimeout)
	defer cancel()

	home, err := env.HomeDir(ctx)
	if err != nil {
		return fmt.Errorf("读取目标主机的家目录失败：%w", err)
	}
	root := path.Join(home, RemoteRoot)
	remoteArchive := path.Join(root, stagingDir, manifest.Org, manifest.Name, "package.zip")

	if err := env.Upload(ctx, remoteArchive, strings.NewReader(string(archive)), int64(len(archive))); err != nil {
		return fmt.Errorf("上传安装包失败：%w", err)
	}

	stdout, stderr, code, err := env.Exec(ctx, remoteInstallScript(root, remoteArchive, manifest))
	if err != nil {
		return fmt.Errorf("在目标主机上落位失败：%w", err)
	}
	// 远端脚本用了 set -e，只要中间任何一步失败就会带着非零码退出，
	// 因此这里只需要把它的输出当作错误详情转出去。
	if code != 0 {
		return fmt.Errorf("在目标主机上落位失败：%s", remoteFailureDetail(stdout, stderr, code))
	}
	return nil
}

// UninstallRemote 删除插件在目标机上的全部版本目录。
//
// 只删这一份插件自己的目录，不碰远端插件根目录本身：根目录里可能还有别的
// 插件，而"删掉别人"不在这次卸载的语义里。
func UninstallRemote(ctx context.Context, env RemoteEnv, org, name string) error {
	if env == nil {
		return errors.New("目标主机执行环境不可用")
	}
	if !identifierPattern.MatchString(org) || !identifierPattern.MatchString(name) {
		return errors.New("插件标识非法")
	}

	ctx, cancel := context.WithTimeout(ctx, remotePushTimeout)
	defer cancel()

	home, err := env.HomeDir(ctx)
	if err != nil {
		return fmt.Errorf("读取目标主机的家目录失败：%w", err)
	}
	target := path.Join(home, RemoteRoot, org, name)

	// 目录本来就不存在时直接成功：卸载的目标是"它在目标机上了无痕迹"，
	// 而不是"必须执行过一次删除"。
	stdout, stderr, code, err := env.Exec(ctx, remoteUninstallScript(target))
	if err != nil {
		return fmt.Errorf("删除目标主机上的插件目录失败：%w", err)
	}
	if code != 0 {
		return fmt.Errorf("删除目标主机上的插件目录失败：%s", remoteFailureDetail(stdout, stderr, code))
	}
	return nil
}

// remoteInstallScript 生成目标机上的落位脚本。
//
// 脚本里所有变量都加了引号：目标机上的家目录未必是简单的 /home/xxx，
// 带空格或特殊字符时未加引号的展开会把路径拆成两段，删除类命令在那种情况下
// 会删到别的地方去。
//
// 顺序刻意是"先在暂存目录解压 → 校验入口 → 删旧版本 → 落位"：
// 解压失败时旧版本原样不动，目标机上那台机器上正在被插件读写的东西不会丢。
func remoteInstallScript(root, remoteArchive string, manifest *Manifest) string {
	org := manifest.Org
	name := manifest.Name
	version := manifest.Version
	entry := manifest.Entry

	pluginDir := path.Join(root, org, name)
	staged := path.Join(root, stagingDir, org, name)
	target := path.Join(pluginDir, version)

	// 逐行说明（顺序即语义）：
	//  1. 清空暂存目录后解压。旧版本此时一动不动 —— 解压失败时目标机上
	//     正在用的那份还是完整的。
	//  2. 剥掉多余的顶层目录，规则与面板本地的 detectTopLevelPrefix 一致：
	//     暂存目录下只有唯一一个条目且它是目录时，才认为包是多套了一层。
	//     两边规则一致，才不会出现"面板能装、目标机装上却打不开"。
	//  3. 入口文件必须存在，否则落位了也是个打不开的空壳。
	//  4. 同版本重装先把旧目录挪到 .previous，落位成功再删。中途失败能挪回来，
	//     不会留下"目录名还在、内容已经空了"的插件。
	//  5. 清掉其它版本目录：它们不被任何记录引用，留着只占磁盘。
	return `set -e

plugin_dir=` + shellQuote(pluginDir) + `
staged=` + shellQuote(staged) + `
target=` + shellQuote(target) + `
archive=` + shellQuote(remoteArchive) + `
entry=` + shellQuote(entry) + `

rm -rf "$staged"
mkdir -p "$staged"
unzip -q -o "$archive" -d "$staged"

top=
count=0
for item in "$staged"/*; do
    [ -e "$item" ] || continue
    count=$((count + 1))
    top=$item
done
if [ "$count" -eq 1 ] && [ -d "$top" ]; then
    mv "$top" "$staged.contents"
    rm -rf "$staged"
    mv "$staged.contents" "$staged"
fi

if [ ! -f "$staged/plugin.json" ]; then
    echo "安装包根目录缺少 plugin.json" >&2
    exit 3
fi
if [ ! -f "$staged/$entry" ]; then
    echo "安装包内缺少入口文件：$entry" >&2
    exit 4
fi

mkdir -p "$plugin_dir"
if [ -e "$target" ]; then
    rm -rf "$target.previous"
    mv "$target" "$target.previous"
fi
if ! mv "$staged" "$target"; then
    [ -e "$target.previous" ] && mv "$target.previous" "$target"
    exit 5
fi
rm -rf "$target.previous"

for sibling in "$plugin_dir"/*; do
    [ -e "$sibling" ] || continue
    if [ "$sibling" != "$target" ]; then
        rm -rf "$sibling"
    fi
done

rm -rf ` + shellQuote(path.Join(root, stagingDir, org)) + `
echo ok
`
}

// remoteUninstallScript 生成目标机上的删除脚本。
func remoteUninstallScript(target string) string {
	return `set -e
target=` + shellQuote(target) + `
rm -rf "$target"
echo ok
`
}

// shellQuote 把路径安全地拼进 shell 脚本。
//
// 与 api 包里的同名函数是同一份约定（那里是私有函数，这里不能直接引用）：
// 路径由面板自己算出，但仍然可能包含目标机家目录里的任意字符 —— 单引号
// 是必须处理的，否则一个 `a'b` 就能截断参数并在目标机上执行额外命令。
func shellQuote(p string) string {
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

// remoteFailureDetail 把远端脚本的输出整理成一句错误详情。
//
// 优先 stderr：脚本里所有主动报错都写在 stderr 上，而 stdout 可能混着
// unzip 的进度。两边都空时才退回退出码 —— 那种情况下用户至少要知道
// 它是怎么结束的。
func remoteFailureDetail(stdout, stderr string, code int) string {
	for _, text := range []string{stderr, stdout} {
		if detail := strings.TrimSpace(clipRemoteOutput(text)); detail != "" {
			return detail
		}
	}
	return fmt.Sprintf("目标机上的命令以退出码 %d 结束", code)
}

// clipRemoteOutput 限制回传输出的长度，并保留最后几行。
//
// 留尾部而不是头部：脚本的报错总是在最后一行，而前面可能是几百行
// unzip 的进度。
func clipRemoteOutput(text string) string {
	if len(text) <= maxRemoteOutput {
		return text
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	return "(输出过长，仅保留末尾)\n" + strings.Join(lines, "\n")
}
