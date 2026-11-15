package env

// 目录里"装上了不等于能用"的条目，其安装后复核脚本放在这里。
//
// 复核脚本的约定：输出 OK 表示通过，其它内容原样作为失败原因回给用户
// （由 verify.go 的 check 类型消费）。服务类条目（数据库、Web 服务器）
// 的复核由 catalog_system.go 里的 svcCheck 生成，这里只放非服务类的。

// checkBun 复核 bun 能不能真的跑起来。
//
// 不能只查文件在不在：bun 是预编译二进制，glibc 过旧时"装得上、一执行就报错"，
// 只做存在性检查会给出一个假结论。
func checkBun(*Profile) string {
	return `v=$(/usr/local/bin/bun --version 2>&1 | head -1)
case "$v" in
  [0-9]*) echo OK ;;
  *) echo "bun 安装后无法运行（常见原因是 glibc 过旧）：$v" ;;
esac`
}

// checkDsh 复核 dsh 能真的跑起来。
//
// 比照 checkBun：装上了不等于能用。dsh 背后是一整棵 npm 依赖树，
// 装了一半（网络中断、npm 缓存损坏）时命令文件在、执行却失败，
// 而"点开工作台能不能用"恰恰只有真跑一遍才回答得了。
//
// 先自己找可执行文件而不直接用 `dsh`：面板跑命令是非登录 shell，
// npm 的全局 bin 目录未必在 PATH 上（与 detectDsh 同一理由）。
// 判定上给退出码留了余地 —— 上游没有承诺 --version 的输出格式，
// 只要它成功地跑完（退出码 0）就说明这棵树是好的。
func checkDsh(*Profile) string {
	return `p=$(command -v dsh 2>/dev/null)
if [ -z "$p" ]; then
  for d in /usr/local/bin /usr/bin; do [ -x "$d/dsh" ] && p="$d/dsh" && break; done
fi
if [ -z "$p" ]; then
  echo "npm 报告已安装，但 PATH 与常见目录里都找不到 dsh 可执行文件"
  exit 0
fi
out=$("$p" --version 2>&1)
rc=$?
case "$out" in
  *[0-9].[0-9]*) echo OK ;;
  *) [ "$rc" = 0 ] && echo OK || echo "dsh 可执行文件存在但无法运行：$(echo "$out" | head -1)" ;;
esac`
}

// checkCUDA 复核 nvcc 能否执行并报出版本。
//
// 同理：CUDA 是几个 GB 的包，装了一半或依赖不匹配时二进制在、执行失败，
// 复核必须真的把它跑一遍才算数。
func checkCUDA(*Profile) string {
	return `v=$(/usr/local/cuda/bin/nvcc --version 2>&1 | tail -1 | sed -E 's/.*release ([0-9.]+).*/\1/')
case "$v" in
  [0-9]*) echo OK ;;
  *) echo "nvcc 无法执行或版本无法识别：$v" ;;
esac`
}

// checkDocker 复核 Docker 守护进程真的能用。
//
// 只验 `docker --version` 是不够的：Docker 装上了但 daemon 起不来（cgroups、
// iptables、存储驱动不匹配）是最经典的失败态，那时版本命令照常打出版本，
// 而用户一条 `docker run` 都跑不了。
//
// 与普通服务不同，这里要把"探活被权限拒绝"和"守护进程真的坏了"分开：
// 安装不会把登录用户加进 docker 组（那等于给 root 权限），因此绝大多数机器上
// `docker info` 以普通用户执行都会是 permission denied。服务在跑就说明装好了，
// 面板后续管理 Docker 时会自动提权。
func checkDocker(p *Profile) string {
	unit := map[Family]string{
		FamilyApt: "docker", FamilyDNF: "docker", FamilyYUM: "docker", FamilyAPK: "docker",
	}[p.Family]
	return `v=$(docker --version 2>/dev/null | sed -E 's/Docker version ([^ ,]+).*/\1/')
[ -z "$v" ] && { echo '未安装：docker 命令不可用'; exit 0; }
if ! svc_active ` + unit + `; then echo "$v：服务未运行（` + unit + ` 未激活）"; exit 0; fi
if docker info >/dev/null 2>&1; then echo OK; exit 0; fi
out=$(docker info 2>&1)
case "$out" in
  *[Pp]ermission\ denied*) echo OK ;;
  *) echo "$v：docker info 失败：$(printf '%s' "$out" | head -n 1)" ;;
esac`
}

// checkMongo 复核 mongod 可用且服务在跑。
//
// 不做探活：mongosh 属于独立的 shell 包，没装它不代表服务有问题。
func checkMongo(p *Profile) string {
	return svcCheck(svcCheckSpec{
		version:     "mongod --version 2>/dev/null | head -1 | sed -E 's/.*v([0-9.]+).*/\\1/'",
		units:       map[Family]string{FamilyApt: "mongod", FamilyDNF: "mongod", FamilyYUM: "mongod"},
		missingHint: "mongod 命令不可用",
	})(p)
}
