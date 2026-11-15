package env

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// 本文件是"直接由发行版仓库提供"的目录条目。
//
// 目录里绝大多数条目都是这一类：包名按家族不同、安装就是一次包管理器操作、
// 回滚就是卸载同名包、复核就是再跑一次版本探测。给它们各写一个 planXxx
// 只会把同一段代码复制十几遍，因此统一由下面的数据表生成 —— 新增一项就是
// 加一段数据，而不是加一个函数。
//
// 真正特殊的（需要第三方源或官方 tarball 的 Node.js / Go / Docker）仍在
// catalog.go 里单独实现。

// systemRecipeSpec 描述一个由系统仓库提供的目录条目。
type systemRecipeSpec struct {
	slug        string
	name        string
	category    string
	description string
	// detect 输出的第一行即版本号；无输出视为未安装。
	detect func(*Profile) string
	// packages 是各家族的包名。没有列出某个家族，就意味着该项不支持它 ——
	// 界面上会直接标成"不支持"，比装到一半报"找不到包"干净。
	packages map[Family][]string
	// rollback 是回滚时要卸载的包；留空表示与 packages 相同。
	rollback []string
	// prepare 是装完包之后、启动服务之前要额外跑的步骤（按家族给）。
	// 例如 PostgreSQL 在 RHEL 系上必须先生成数据目录，否则服务起不来。
	prepare map[Family][]Step
	// service 是要"开机自启并立即启动"的服务名（按家族给，因为单元名不一致：
	// MySQL 在 Debian 是 mysql、在 RHEL 是 mysqld）。留空表示装完就结束。
	service map[Family]string
	// check 是安装后的复核脚本：输出 OK 表示通过，其它内容作为失败原因。
	// 服务类（数据库、Web 服务器）必须给 —— 只看版本号会漏掉"装好了但没起来"。
	check func(*Profile) string
	// detectLabel 是复核结果里的人话名称；留空用 detect 命令本身。
	detectLabel string
	note        string
	// useCase 是适用场景说明，与 description（它是什么）互补。
	useCase string
	// launch 非空表示安装后可在面板窗口里打开；为空表示只安装不打开。
	launch *LaunchSpec
	// configPaths 是主要配置文件的绝对路径；应用包与详情页都从这里取。
	configPaths []string
}

// svcCheckSpec 描述一个服务类条目的复核：版本 + 服务在跑 +（可选）一次真实探活。
type svcCheckSpec struct {
	// version 是取版本号的命令（不含 $( )，由 svcCheck 包起来）。
	version string
	// units 是各家族的单元名；某个家族没给就跳过服务状态检查。
	units map[Family]string
	// probe 是额外的探活命令（命令行首必须是真实存在的可执行文件，
	// 缺失时会跳过而不是误报失败）。
	probe string
	// probeLabel 是探活失败时写进原因的名字。
	probeLabel string
	// missingHint 是版本命令无输出时的补充说明。
	missingHint string
}

// svcCheck 生成服务类条目的复核脚本。
//
// 三层递进，任何一层不过就把原因说明白：
//  1. 版本命令有没有输出 —— 没有就是没装上；
//  2. 服务在不在跑 —— 这是"装上了但用不了"最常见的样子；
//  3. 探活（连接/配置检查）—— 服务进程在，但实际接不接受请求是另一回事。
func svcCheck(s svcCheckSpec) func(*Profile) string {
	return func(p *Profile) string {
		var b strings.Builder

		b.WriteString("v=$(" + s.version + ")\n")
		hint := s.missingHint
		if hint == "" {
			hint = "命令不可用"
		}
		b.WriteString("[ -z \"$v\" ] && { echo '未安装：" + hint + "'; exit 0; }\n")

		if unit := s.units[p.Family]; unit != "" {
			b.WriteString("if ! svc_active " + unit + "; then echo \"$v：服务未运行（" + unit + " 未激活）\"; exit 0; fi\n")
		}

		if s.probe != "" {
			label := s.probeLabel
			if label == "" {
				label = s.probe
			}
			// 探活工具本身可能没装（例如只装了服务端没装客户端）：
			// 那种情况不该报成"服务有问题"，直接跳过这一层。
			bin := strings.Fields(s.probe)[0]
			b.WriteString("if command -v " + bin + " >/dev/null 2>&1 && ! (" + s.probe + ") >/dev/null 2>&1; then\n" +
				"  echo \"$v：服务在运行但探活失败（" + label + "）\"; exit 0\nfi\n")
		}

		b.WriteString("echo OK\n")
		return b.String()
	}
}

// systemRecipe 把一份数据变成 Recipe。
func systemRecipe(spec systemRecipeSpec) *Recipe {
	families := make([]Family, 0, len(spec.packages))
	for f := range spec.packages {
		families = append(families, f)
	}
	// 家族顺序固定下来：它只影响覆盖判断，但顺序不稳会让测试与日志难以对账。
	sort.Slice(families, func(i, j int) bool { return families[i] < families[j] })

	detectLabel := spec.detectLabel
	if detectLabel == "" {
		detectLabel = spec.name
	}

	return &Recipe{
		Slug:          spec.slug,
		Name:          spec.name,
		Category:      spec.category,
		Description:   spec.description,
		UseCase:       spec.useCase,
		Note:          spec.note,
		Launch:        spec.launch,
		Families:      families,
		DetectCommand: spec.detect,
		DetectLabel:   detectLabel,
		ServiceUnits:  spec.service,
		ConfigPaths:   spec.configPaths,
		CheckCommand:  spec.check,
		Plan: func(_ context.Context, p *Profile, _ string) (*RecipePlan, error) {
			names := spec.packages[p.Family]
			if len(names) == 0 {
				return nil, fmt.Errorf("%s 不支持该系统家族（%s）", spec.name, p.Family)
			}
			specs := make([]Spec, 0, len(names))
			for _, n := range names {
				specs = append(specs, Spec{Name: n})
			}
			rollback := spec.rollback
			if len(rollback) == 0 {
				rollback = names
			}

			label := spec.detectLabel
			if label == "" {
				label = spec.name
			}

			steps := []Step{{
				Name:     "安装 " + spec.name,
				Command:  managerSteps(p, ActionInstall, specs),
				Critical: true,
			}}
			steps = append(steps, spec.prepare[p.Family]...)
			if unit := spec.service[p.Family]; unit != "" {
				steps = append(steps, serviceStep(p.Family, unit))
			}

			return &RecipePlan{
				PkgSpecs:       specs,
				Steps:          steps,
				Verify:         spec.detect(p),
				VerifyLabel:    label,
				RollbackRemove: rollback,
				Note:           spec.note,
			}, nil
		},
	}
}

// serviceStep 生成"开机自启并立即启动"这一步。
//
// 与 Docker 的取舍一致：容器里没有 systemd / OpenRC，enable 失败不该把一次
// 已经装好的软件判成安装失败 —— 二进制与数据目录都已经就位了。
func serviceStep(f Family, unit string) Step {
	if f == FamilyAPK {
		return Step{
			Name: "设置 " + unit + " 开机自启并启动（OpenRC）",
			Command: "rc-update add " + unit + " default 2>/dev/null || true\n" +
				"service " + unit + " start 2>/dev/null || true",
			Critical: false,
		}
	}
	return Step{
		Name:     "设置 " + unit + " 开机自启并启动",
		Command:  "systemctl enable --now " + unit + " 2>/dev/null || true",
		Critical: false,
	}
}

// allFamilies 是常见组合：四种家族都用同一个包名。
func allFamilies(name string) map[Family][]string {
	return map[Family][]string{
		FamilyApt: {name},
		FamilyDNF: {name},
		FamilyYUM: {name},
		FamilyAPK: {name},
	}
}

// allFamilyService 是常见组合：四种家族的单元名一致。
func allFamilyService(name string) map[Family]string {
	return map[Family]string{
		FamilyApt: name,
		FamilyDNF: name,
		FamilyYUM: name,
		FamilyAPK: name,
	}
}

// launchOf 生成安全类条目的启动元数据：打开时执行一条状态命令，并列出配置文件。
func launchOf(command string, configPaths ...string) *LaunchSpec {
	return &LaunchSpec{Command: command, ConfigPaths: configPaths}
}

// ---- 版本探测 ----

// systemVersionCommand 生成一个版本探测函数：命令输出的第一行即版本号，
// 没有任何输出就意味着目标机上没装它。
func systemVersionCommand(cmd string) func(*Profile) string {
	return func(*Profile) string { return firstLineVersion(cmd) }
}

// ---- 目录条目 ----

// systemRecipes 返回全部由数据表生成的目录条目。
func systemRecipes() []*Recipe {
	specs := []systemRecipeSpec{
		// ---------- 运行环境 ----------
		{
			slug: "php", name: "PHP", category: "runtime",
			description: "PHP CLI 与 php-fpm 进程管理器（Alpine 的包名带版本号，如 php83，故不在此支持）",
			detect:      systemVersionCommand("php -v | head -1 | awk '{print $2}'"),
			detectLabel: "php",
			packages: map[Family][]string{
				FamilyApt: {"php-cli", "php-fpm"},
				FamilyDNF: {"php-cli", "php-fpm"},
				FamilyYUM: {"php-cli", "php-fpm"},
			},
			note: "php-fpm 不会被自动设为开机自启：是否需要常驻取决于你打算怎么跑它",
		},
		{
			slug: "ruby", name: "Ruby", category: "runtime",
			description: "Ruby 解释器与原生扩展编译所需的开发头文件",
			detect:      systemVersionCommand("ruby -v | awk '{print $2}'"),
			detectLabel: "ruby",
			packages: map[Family][]string{
				FamilyApt: {"ruby", "ruby-dev"},
				FamilyDNF: {"ruby", "ruby-devel"},
				FamilyYUM: {"ruby", "ruby-devel"},
				FamilyAPK: {"ruby", "ruby-dev"},
			},
		},

		// ---------- 开发工具链 ----------
		{
			slug: "cmake", name: "CMake", category: "toolchain",
			description: "跨平台构建系统生成器",
			detect:      systemVersionCommand("cmake --version | head -1 | awk '{print $3}'"),
			detectLabel: "cmake",
			packages:    allFamilies("cmake"),
		},
		{
			slug: "ninja", name: "Ninja", category: "toolchain",
			description: "轻量高速构建后端，常与 CMake 搭配使用",
			detect:      systemVersionCommand("ninja --version"),
			detectLabel: "ninja",
			packages: map[Family][]string{
				FamilyApt: {"ninja-build"},
				FamilyDNF: {"ninja-build"},
				FamilyYUM: {"ninja-build"},
				FamilyAPK: {"ninja"},
			},
		},
		{
			slug: "git", name: "Git", category: "toolchain",
			description: "版本控制客户端，拉取与构建源码时几乎必备",
			detect:      systemVersionCommand("git --version | awk '{print $3}'"),
			detectLabel: "git",
			packages:    allFamilies("git"),
		},
		{
			slug: "autotools", name: "Autotools", category: "toolchain",
			description: "autoconf / automake / libtool，构建带 configure 脚本的老牌项目需要",
			detect:      systemVersionCommand("autoconf --version | head -1 | awk '{print $NF}'"),
			detectLabel: "autoconf",
			packages: map[Family][]string{
				FamilyApt: {"autoconf", "automake", "libtool"},
				FamilyDNF: {"autoconf", "automake", "libtool"},
				FamilyYUM: {"autoconf", "automake", "libtool"},
				FamilyAPK: {"autoconf", "automake", "libtool"},
			},
		},
		{
			slug: "clang", name: "Clang / LLVM", category: "toolchain",
			description: "LLVM 编译器前端，可替代 gcc 参与构建",
			detect:      systemVersionCommand("clang --version | head -1 | sed -E 's/.*version ([0-9][0-9.]*).*/\\1/'"),
			detectLabel: "clang",
			packages:    allFamilies("clang"),
		},
		{
			slug: "rust", name: "Rust", category: "toolchain",
			description: "rustc 编译器与 cargo 包管理器（来自发行版仓库，非 rustup）",
			detect:      systemVersionCommand("rustc --version | awk '{print $2}'"),
			detectLabel: "rustc",
			packages: map[Family][]string{
				FamilyApt: {"rustc", "cargo"},
				FamilyDNF: {"rust", "cargo"},
				FamilyYUM: {"rust", "cargo"},
				FamilyAPK: {"rust", "cargo"},
			},
			note: "仓库版本的 Rust 通常落后于官方 stable，需要最新版请自行用 rustup 安装",
		},
		{
			slug: "maven", name: "Maven", category: "toolchain",
			description: "Java 项目的依赖管理与构建工具",
			detect:      systemVersionCommand("mvn -v | head -1 | awk '{print $3}'"),
			detectLabel: "maven",
			packages:    allFamilies("maven"),
		},
		{
			slug: "gradle", name: "Gradle", category: "toolchain",
			description: "Java / Kotlin 项目的构建工具（Alpine 仓库不提供，故不在此支持）",
			detect:      systemVersionCommand("gradle -v | grep -m1 '^Gradle' | awk '{print $2}'"),
			detectLabel: "gradle",
			packages: map[Family][]string{
				FamilyApt: {"gradle"},
				FamilyDNF: {"gradle"},
				FamilyYUM: {"gradle"},
			},
		},
		{
			slug: "pkgconf", name: "pkg-config", category: "toolchain",
			description: "编译时查询库的编译与链接参数，很多源码构建流程依赖它",
			detect:      systemVersionCommand("pkg-config --version"),
			detectLabel: "pkg-config",
			packages: map[Family][]string{
				FamilyApt: {"pkg-config"},
				FamilyDNF: {"pkgconf-pkg-config"},
				FamilyYUM: {"pkgconf-pkg-config"},
				FamilyAPK: {"pkgconf"},
			},
		},
		{
			slug: "gdb", name: "GDB", category: "toolchain",
			description: "GNU 调试器，排查崩溃与核心转储时使用",
			detect:      systemVersionCommand("gdb --version | head -1 | awk '{print $NF}'"),
			detectLabel: "gdb",
			packages:    allFamilies("gdb"),
		},
		{
			slug: "ffmpeg", name: "FFmpeg", category: "toolchain",
			description: "音视频转码与处理工具（ffmpeg / ffprobe）",
			detect:      systemVersionCommand("ffmpeg -version | head -1 | awk '{print $3}' | cut -d- -f1"),
			detectLabel: "ffmpeg",
			packages:    allFamilies("ffmpeg"),
			note:        "RHEL 系（Rocky / Alma / RHEL）的官方仓库不含 ffmpeg，需要先启用 EPEL 或 RPM Fusion，否则预检会提示找不到包",
		},

		// ---------- 服务与中间件 ----------
		{
			slug: "nginx", name: "Nginx", category: "service",
			description: "Web 服务器与反向代理，装完即设为开机自启并启动",
			detect:      systemVersionCommand("nginx -v 2>&1 | sed -E 's#.*nginx/([^ ]+).*#\\1#'"),
			detectLabel: "nginx",
			packages:    allFamilies("nginx"),
			service: map[Family]string{
				FamilyApt: "nginx", FamilyDNF: "nginx", FamilyYUM: "nginx", FamilyAPK: "nginx",
			},
			check: svcCheck(svcCheckSpec{
				version: "nginx -v 2>&1 | sed -E 's#.*nginx/([^ ]+).*#\\1#'",
				units: map[Family]string{
					FamilyApt: "nginx", FamilyDNF: "nginx", FamilyYUM: "nginx", FamilyAPK: "nginx",
				},
				probe:       "nginx -t",
				probeLabel:  "nginx -t 配置检查",
				missingHint: "nginx 命令不可用",
			}),
		},

		// ---------- 数据库与缓存 ----------
		{
			slug: "postgresql", name: "PostgreSQL", category: "database",
			description: "PostgreSQL 服务端与客户端，装完即设为开机自启并启动",
			detect:      systemVersionCommand("postgres --version | awk '{print $NF}'"),
			detectLabel: "postgres",
			packages: map[Family][]string{
				FamilyApt: {"postgresql"},
				FamilyDNF: {"postgresql-server"},
				FamilyYUM: {"postgresql-server"},
				FamilyAPK: {"postgresql", "postgresql-client"},
			},
			// RHEL 系的包只装二进制，数据目录要显式初始化，否则服务起不来。
			prepare: map[Family][]Step{
				FamilyDNF: {{Name: "初始化 PostgreSQL 数据目录", Command: "postgresql-setup --initdb 2>&1 || true", Critical: false}},
				FamilyYUM: {{Name: "初始化 PostgreSQL 数据目录", Command: "postgresql-setup --initdb 2>&1 || true", Critical: false}},
			},
			service: map[Family]string{
				FamilyApt: "postgresql", FamilyDNF: "postgresql", FamilyYUM: "postgresql", FamilyAPK: "postgresql",
			},
			check: svcCheck(svcCheckSpec{
				version: "postgres --version 2>/dev/null | awk '{print $NF}'",
				units: map[Family]string{
					FamilyApt: "postgresql", FamilyDNF: "postgresql", FamilyYUM: "postgresql", FamilyAPK: "postgresql",
				},
				probe:       "pg_isready -q",
				probeLabel:  "pg_isready 连接检查",
				missingHint: "postgres 命令不可用",
			}),
			note: "Debian/Ubuntu 的包会自动建好默认实例；RHEL 系由本步骤补一次 initdb。数据库默认只监听本机，对外开放需自行改配置",
		},
		{
			slug: "mariadb", name: "MariaDB", category: "database",
			description: "MySQL 兼容的关系数据库，装完即设为开机自启并启动",
			detect:      systemVersionCommand("mariadb --version 2>/dev/null | awk '{print $5}' | cut -d- -f1 || mysql --version | awk '{print $5}' | cut -d- -f1"),
			detectLabel: "mariadb",
			packages: map[Family][]string{
				FamilyApt: {"mariadb-server"},
				FamilyDNF: {"mariadb-server"},
				FamilyYUM: {"mariadb-server"},
				FamilyAPK: {"mariadb", "mariadb-client"},
			},
			service: map[Family]string{
				FamilyApt: "mariadb", FamilyDNF: "mariadb", FamilyYUM: "mariadb", FamilyAPK: "mariadb",
			},
			check: svcCheck(svcCheckSpec{
				// 客户端可执行文件名随版本变（10.5+ 是 mariadb，更早只有 mysql），
				// 因此两个都试一遍。
				version: "{ command -v mariadb >/dev/null 2>&1 && mariadb --version || mysql --version; } 2>/dev/null | awk '{print $5}' | cut -d- -f1",
				units: map[Family]string{
					FamilyApt: "mariadb", FamilyDNF: "mariadb", FamilyYUM: "mariadb", FamilyAPK: "mariadb",
				},
				probe:       "mysqladmin ping --silent",
				probeLabel:  "mysqladmin ping",
				missingHint: "mariadb / mysql 客户端命令不可用",
			}),
			note: "root 账号默认走 unix_socket 认证：本机 root 用户可直接 mysql 登录，远程连接需另建账号",
		},
		{
			slug: "mysql", name: "MySQL", category: "database",
			description: "Oracle MySQL 官方服务端（Alpine 仓库不提供，故不在此支持）",
			detect:      systemVersionCommand("mysqld --version 2>/dev/null | awk '{print $3}' | cut -d- -f1"),
			detectLabel: "mysqld",
			packages: map[Family][]string{
				FamilyApt: {"mysql-server"},
				FamilyDNF: {"mysql-server"},
			},
			// 单元名两个家族不一致：Debian 叫 mysql，RHEL 叫 mysqld。
			service: map[Family]string{FamilyApt: "mysql", FamilyDNF: "mysqld"},
			check: svcCheck(svcCheckSpec{
				version:     "mysqld --version 2>/dev/null | awk '{print $3}' | cut -d- -f1",
				units:       map[Family]string{FamilyApt: "mysql", FamilyDNF: "mysqld"},
				probe:       "mysqladmin ping --silent",
				probeLabel:  "mysqladmin ping",
				missingHint: "mysqld 命令不可用",
			}),
			note: "与 MariaDB 二选一：同一台机器上同时装两套 MySQL 系服务端会互相抢 3306 端口与 /var/lib/mysql",
		},
		{
			slug: "redis", name: "Redis", category: "database",
			description: "内存键值数据库，常用作缓存、会话与消息队列",
			detect:      systemVersionCommand("redis-server --version | awk '{print $3}' | cut -d= -f2"),
			detectLabel: "redis-server",
			packages: map[Family][]string{
				FamilyApt: {"redis-server"},
				FamilyDNF: {"redis"},
				FamilyYUM: {"redis"},
				FamilyAPK: {"redis"},
			},
			service: map[Family]string{
				FamilyApt: "redis-server", FamilyDNF: "redis", FamilyYUM: "redis", FamilyAPK: "redis",
			},
			check: svcCheck(svcCheckSpec{
				version: "redis-server --version 2>/dev/null | awk '{print $3}' | cut -d= -f2",
				units: map[Family]string{
					FamilyApt: "redis-server", FamilyDNF: "redis", FamilyYUM: "redis", FamilyAPK: "redis",
				},
				probe:       "redis-cli ping | grep -q PONG",
				probeLabel:  "redis-cli ping",
				missingHint: "redis-server 命令不可用",
			}),
			note: "默认只监听 127.0.0.1 且无密码。要对内网开放，务必同时设置 requirepass 与防火墙规则",
		},

		// ---------- 监控与磁盘 ----------
		{
			slug: "btop", name: "btop", category: "monitor",
			description: "终端资源监视器：CPU、内存、磁盘、网络与进程一屏看完",
			detect:      systemVersionCommand("btop --version 2>/dev/null | head -1 | grep -oE '[0-9]+\\.[0-9.]+'"),
			detectLabel: "btop",
			packages: map[Family][]string{
				FamilyApt: {"btop"},
				FamilyDNF: {"btop"},
				FamilyAPK: {"btop"},
			},
			note: "RHEL 系需要先启用 EPEL 仓库",
		},
		{
			slug: "htop", name: "htop", category: "monitor",
			description: "交互式进程查看器，比 top 更易读",
			detect:      systemVersionCommand("htop --version | head -1 | awk '{print $2}'"),
			detectLabel: "htop",
			packages:    allFamilies("htop"),
		},
		{
			slug: "ncdu", name: "ncdu", category: "monitor",
			description: "按目录逐层查看磁盘占用，找大文件最快的手段之一",
			detect:      systemVersionCommand("ncdu -v 2>&1 | head -1 | awk '{print $2}'"),
			detectLabel: "ncdu",
			packages:    allFamilies("ncdu"),
		},
		{
			slug: "duf", name: "duf", category: "monitor",
			description: "彩色磁盘用量表格，比 df 直观",
			detect:      systemVersionCommand("duf --version 2>/dev/null | head -1 | awk '{print $2}'"),
			detectLabel: "duf",
			packages: map[Family][]string{
				FamilyApt: {"duf"},
				FamilyDNF: {"duf"},
				FamilyAPK: {"duf"},
			},
			note: "RHEL 系需要先启用 EPEL 仓库",
		},
		{
			slug: "glances", name: "Glances", category: "monitor",
			description: "跨平台系统监控，支持 Web 模式与导出指标",
			detect:      systemVersionCommand("glances --version 2>/dev/null | head -1 | awk '{print $2}'"),
			detectLabel: "glances",
			packages: map[Family][]string{
				FamilyApt: {"glances"},
				FamilyDNF: {"glances"},
				FamilyAPK: {"glances"},
			},
			note: "RHEL 系需要先启用 EPEL 仓库",
		},

		// ---------- 终端效率 ----------
		{
			slug: "fzf", name: "fzf", category: "utility",
			description: "模糊查找器，可用于历史命令、文件与任何列表的筛选",
			detect:      systemVersionCommand("fzf --version | awk '{print $1}'"),
			detectLabel: "fzf",
			packages:    allFamilies("fzf"),
		},
		{
			slug: "zoxide", name: "zoxide", category: "utility",
			description: "智能目录跳转（z 命令），按访问频率记忆路径",
			detect:      systemVersionCommand("zoxide --version | awk '{print $2}'"),
			detectLabel: "zoxide",
			packages:    allFamilies("zoxide"),
			note:        "装完还需在 shell 配置里加 eval \"$(zoxide init bash)\"（zsh 同理）才会生效",
		},
		{
			slug: "bat", name: "bat", category: "utility",
			description: "带语法高亮与行号的 cat 替代品",
			detect: systemVersionCommand(
				`{ command -v bat >/dev/null 2>&1 && bat --version || batcat --version; } 2>/dev/null | head -1 | awk '{print $2}'`),
			detectLabel: "bat",
			packages: map[Family][]string{
				FamilyApt: {"bat"},
				FamilyDNF: {"bat"},
				FamilyAPK: {"bat"},
			},
			note: "Debian/Ubuntu 上可执行文件名是 batcat（与 bacula 的 bat 冲突）",
		},
		{
			slug: "fd", name: "fd", category: "utility",
			description: "比 find 更快、更易懂的文件查找工具",
			detect: systemVersionCommand(
				`{ command -v fd >/dev/null 2>&1 && fd --version || fdfind --version; } 2>/dev/null | head -1 | awk '{print $2}'`),
			detectLabel: "fd",
			packages: map[Family][]string{
				FamilyApt: {"fd-find"},
				FamilyDNF: {"fd-find"},
				FamilyAPK: {"fd"},
			},
			note: "Debian/Ubuntu 的包名为 fd-find，可执行文件名是 fdfind",
		},
		{
			slug: "ripgrep", name: "ripgrep", category: "utility",
			description: "高速递归文本搜索（rg），尊重 .gitignore",
			detect:      systemVersionCommand("rg --version | head -1 | awk '{print $2}'"),
			detectLabel: "rg",
			packages:    allFamilies("ripgrep"),
		},
		{
			slug: "tmux", name: "tmux", category: "utility",
			description: "终端复用器：断线后会话仍在，长任务不必挂着 SSH",
			detect:      systemVersionCommand("tmux -V | awk '{print $2}'"),
			detectLabel: "tmux",
			packages:    allFamilies("tmux"),
		},
		{
			slug: "tree", name: "tree", category: "utility",
			description: "以树状结构列出目录内容",
			detect:      systemVersionCommand("tree --version | awk '{print $2}' | sed 's/^v//'"),
			detectLabel: "tree",
			packages:    allFamilies("tree"),
		},

		// ---------- 网络诊断 ----------
		{
			slug: "mtr", name: "mtr", category: "network",
			description: "把 traceroute 与 ping 合成一屏，持续性观察每一跳的丢包与延迟",
			detect:      systemVersionCommand("mtr --version 2>&1 | head -1 | awk '{print $2}'"),
			detectLabel: "mtr",
			packages:    allFamilies("mtr"),
		},
		{
			slug: "nmap", name: "nmap", category: "security",
			description: "端口与服务扫描，识别主机暴露面",
			useCase:     "上线前自查、内网资产盘点、排查异常开放端口",
			launch:      launchOf("nmap --version"),
			detect:      systemVersionCommand("nmap --version | head -1 | awk '{print $3}'"),
			detectLabel: "nmap",
			packages:    allFamilies("nmap"),
			note:        "扫描前请确认已获得目标网络的授权",
		},
		{
			slug: "iperf3", name: "iperf3", category: "network",
			description: "带宽测速服务端/客户端，定位链路瓶颈",
			detect:      systemVersionCommand("iperf3 --version 2>/dev/null | head -1 | awk '{print $2}'"),
			detectLabel: "iperf3",
			packages: map[Family][]string{
				FamilyApt: {"iperf3"},
				FamilyDNF: {"iperf3"},
				FamilyAPK: {"iperf3"},
			},
		},
		{
			slug: "socat", name: "socat", category: "network",
			description: "多功能中继工具：端口转发、Unix socket、TLS 隧道",
			detect:      systemVersionCommand("socat -V 2>&1 | head -1 | awk '{print $3}'"),
			detectLabel: "socat",
			packages:    allFamilies("socat"),
		},
		{
			slug: "tcpdump", name: "tcpdump", category: "security",
			description: "命令行抓包，解析协议与握手过程",
			useCase:     "定位连接超时、丢包、TLS 握手失败等网络疑难问题",
			launch:      launchOf("tcpdump --version"),
			detect:      systemVersionCommand("tcpdump --version | head -1 | awk '{print $3}'"),
			detectLabel: "tcpdump",
			packages:    allFamilies("tcpdump"),
			note:        "抓包需要 root 权限（或 CAP_NET_RAW）",
		},

		// ---------- 备份与同步 ----------
		{
			slug: "restic", name: "restic", category: "backup",
			description: "加密去重的增量备份，支持本地目录与 S3 等后端",
			detect:      systemVersionCommand("restic version | head -1 | awk '{print $2}'"),
			detectLabel: "restic",
			packages:    allFamilies("restic"),
		},
		{
			slug: "rclone", name: "rclone", category: "backup",
			description: "云存储同步与挂载，支持 S3、WebDAV、OneDrive 等",
			detect:      systemVersionCommand("rclone version | head -1 | awk '{print $2}' | sed 's/^v//'"),
			detectLabel: "rclone",
			packages:    allFamilies("rclone"),
		},
		{
			slug: "borgbackup", name: "BorgBackup", category: "backup",
			description: "去重压缩备份（borg），适合定期快照到远端仓库",
			detect:      systemVersionCommand("borg --version 2>/dev/null | awk '{print $2}'"),
			detectLabel: "borg",
			packages:    allFamilies("borgbackup"),
		},

		// ---------- 网络安全 ----------
		// 顺序即界面顺序：按日常使用频率与实用程度排列，先做暴露面排查与基础防护，
		// 再是 VPN、隐私与审计工具。
		{
			slug: "ufw", name: "UFW", category: "security",
			description: "Debian/Ubuntu 上的简易防火墙前端",
			useCase:     "快速放行或封禁端口，给新机器做最小暴露面",
			launch: launchOf("sudo ufw status verbose",
				"/etc/ufw/ufw.conf", "/etc/ufw/user.rules", "/etc/default/ufw"),
			detect:      systemVersionCommand("ufw --version 2>/dev/null | head -1 | awk '{print $2}'"),
			detectLabel: "ufw",
			packages: map[Family][]string{
				FamilyApt: {"ufw"},
				FamilyAPK: {"ufw"},
			},
			note: "安装后需手动启用（ufw enable），并确认已放行 SSH 端口",
		},
		{
			slug: "fail2ban", name: "Fail2Ban", category: "security",
			description: "按日志自动封禁暴力破解来源 IP",
			useCase:     "公网 SSH、Web 登录入口的入侵防护",
			launch: launchOf("sudo fail2ban-client status",
				"/etc/fail2ban/jail.local", "/etc/fail2ban/jail.d"),
			detect:      systemVersionCommand("fail2ban-client --version 2>/dev/null | head -1 | awk '{print $2}' | sed 's/^v//'"),
			detectLabel: "fail2ban-client",
			packages:    allFamilies("fail2ban"),
			service:     allFamilyService("fail2ban"),
			check: svcCheck(svcCheckSpec{
				version:     "fail2ban-client --version 2>/dev/null | head -1 | awk '{print $2}' | sed 's/^v//'",
				units:       allFamilyService("fail2ban"),
				missingHint: "fail2ban-client 不可用",
			}),
			note: "默认规则读取系统认证日志，启用前请确认日志路径",
		},
		{
			slug: "clamav", name: "ClamAV", category: "security",
			description: "开源病毒与恶意软件扫描引擎",
			useCase:     "扫描上传目录、邮件附件与可疑文件",
			launch: launchOf("clamscan --version",
				"/etc/clamav/clamd.conf", "/etc/clamav/freshclam.conf"),
			detect:      systemVersionCommand("clamscan --version 2>/dev/null | head -1 | awk '{print $2}'"),
			detectLabel: "clamscan",
			packages:    allFamilies("clamav"),
			note:        "需运行 freshclam 更新病毒库；首次全盘扫描耗时较长",
		},
		{
			slug: "wireguard-tools", name: "WireGuard", category: "security",
			description: "轻量 VPN 的配置与隧道管理工具",
			useCase:     "组建点对点加密网络、远程安全访问内网",
			launch:      launchOf("sudo wg show", "/etc/wireguard"),
			detect:      systemVersionCommand("wg --version 2>/dev/null | head -1 | awk '{print $2}' | sed 's/^v//'"),
			detectLabel: "wg",
			packages:    allFamilies("wireguard-tools"),
			note:        "仅安装配置工具，隧道与密钥需另行创建",
		},
		{
			slug: "openvpn", name: "OpenVPN", category: "security",
			description: "成熟的 SSL VPN 服务端与客户端",
			useCase:     "为远程办公、跨机房互联提供加密通道",
			launch: launchOf("openvpn --version",
				"/etc/openvpn/server", "/etc/openvpn/client"),
			detect:      systemVersionCommand("openvpn --version | head -1 | awk '{print $2}'"),
			detectLabel: "openvpn",
			packages:    allFamilies("openvpn"),
			note:        "仅安装服务端与客户端，证书与配置需另行生成",
		},
		{
			slug: "nethogs", name: "NetHogs", category: "security",
			description: "按进程查看实时网络带宽占用",
			useCase:     "定位异常外联、挖矿或数据外传的进程",
			launch:      launchOf("sudo nethogs"),
			detect:      systemVersionCommand("nethogs -V | head -1 | awk '{print $NF}'"),
			detectLabel: "nethogs",
			packages:    allFamilies("nethogs"),
			note:        "需要 root 权限才能看到其他进程的流量",
		},
		{
			slug: "tor", name: "Tor", category: "security",
			description: "匿名洋葱路由客户端",
			useCase:     "需要隐藏来源地址的合规匿名访问与研究",
			launch:      launchOf("sudo systemctl status tor --no-pager", "/etc/tor/torrc"),
			detect:      systemVersionCommand("tor --version 2>/dev/null | head -1 | awk '{print $3}'"),
			detectLabel: "tor",
			packages:    allFamilies("tor"),
			note:        "默认仅提供 SOCKS 代理，不会自动接管系统流量",
		},
		{
			slug: "gnupg", name: "GnuPG", category: "security",
			description: "OpenPGP 加密、签名与密钥管理",
			useCase:     "文件加密、发布包签名校验、密钥对管理",
			launch:      launchOf("gpg --list-keys", "~/.gnupg/gpg.conf", "~/.gnupg/gpg-agent.conf"),
			detect:      systemVersionCommand("gpg --version 2>/dev/null | head -1 | awk '{print $3}'"),
			detectLabel: "gpg",
			packages: map[Family][]string{
				FamilyApt: {"gnupg"},
				FamilyDNF: {"gnupg2"},
				FamilyYUM: {"gnupg2"},
				FamilyAPK: {"gnupg"},
			},
			note: "生成密钥时请设置口令，并妥善备份私钥",
		},
		{
			slug: "pass", name: "pass", category: "security",
			description: "基于 GPG 的命令行密码管理器",
			useCase:     "在服务器上安全保存部署口令与 API 密钥",
			launch:      launchOf("pass ls", "~/.password-store"),
			detect:      systemVersionCommand("pass --version 2>/dev/null | grep -oE '[0-9]+[.][0-9]+[.][0-9]+'"),
			detectLabel: "pass",
			packages:    allFamilies("pass"),
			note:        "使用前需先初始化 GPG 密钥（pass init）",
		},
		{
			slug: "lynis", name: "Lynis", category: "security",
			description: "系统安全基线审计与加固建议",
			useCase:     "新系统上线前体检、定期合规自查",
			launch:      launchOf("sudo lynis --version", "/etc/lynis/default.prf", "/etc/lynis/custom.prf"),
			detect:      systemVersionCommand("lynis --version 2>/dev/null | head -1"),
			detectLabel: "lynis",
			packages:    allFamilies("lynis"),
			note:        "审计本身只读，不会修改系统配置",
		},
		{
			slug: "rkhunter", name: "Rootkit Hunter", category: "security",
			description: "Rootkit 与可疑文件检查工具",
			useCase:     "怀疑主机被入侵时做完整性与后门排查",
			launch:      launchOf("sudo rkhunter --version", "/etc/rkhunter.conf", "/etc/rkhunter.conf.local"),
			detect:      systemVersionCommand("rkhunter --version 2>/dev/null | head -1 | awk '{print $3}'"),
			detectLabel: "rkhunter",
			packages:    allFamilies("rkhunter"),
			note:        "首次运行需执行 rkhunter --propupd 建立基线",
		},
		{
			slug: "chkrootkit", name: "chkrootkit", category: "security",
			description: "本地 Rootkit 检测工具",
			useCase:     "与 Rootkit Hunter 交叉验证，排查后门与篡改",
			launch:      launchOf("chkrootkit -V", "/etc/chkrootkit.conf"),
			detect:      systemVersionCommand("chkrootkit -V 2>/dev/null | head -1 | awk '{print $3}'"),
			detectLabel: "chkrootkit",
			packages:    allFamilies("chkrootkit"),
			note:        "扫描结果存在误报，需结合人工判断",
		},
		{
			slug: "firewalld", name: "firewalld", category: "security",
			description: "RHEL 系动态防火墙管理服务",
			useCase:     "CentOS/RHEL 上按区域管理端口与转发规则",
			launch: launchOf("sudo firewall-cmd --list-all",
				"/etc/firewalld/firewalld.conf", "/etc/firewalld/zones"),
			detect:      systemVersionCommand("firewall-cmd --version 2>/dev/null | head -1"),
			detectLabel: "firewall-cmd",
			packages:    allFamilies("firewalld"),
			note:        "默认不自动启用；启用前确认已放行 SSH，避免断连",
		},
	}

	recipes := make([]*Recipe, 0, len(specs))
	for _, spec := range specs {
		recipes = append(recipes, systemRecipe(spec))
	}
	return recipes
}
