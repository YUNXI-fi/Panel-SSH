// Package store 封装 SQLite 持久化。
// 时间字段统一以 Unix 秒（INTEGER）落库，避免不同驱动对 DATETIME 的解析差异。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 实现，无需 cgo，便于产出静态单二进制
)

// ErrNotFound 表示目标记录不存在。
var ErrNotFound = errors.New("记录不存在")

// Store 是数据库句柄的封装。
type Store struct {
	db *sql.DB
}

// Open 打开（必要时创建）数据库并执行迁移。
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// WAL 已开启，且设置了 5 秒 busy_timeout，因此读写可以并发：
	// 只读查询不必再排在写操作后面。旧实现把连接数压到 1，所有数据库访问
	// 都被串行化 —— 批量执行要为主机逐条查库，监控每 3 秒也要查两次，
	// 这些请求会互相排队。
	// 上限取 4：SQLite 仍是单写者模型，开太多连接只会增加锁竞争。
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	// 批量任务只活在发起它的那条 WebSocket 连接里，进程重启后不可能还有任务在跑。
	// 不在这里收尾的话，上次崩溃留在库里的 running 会永远停在"执行中"。
	if err := s.interruptRunningTasks(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.interruptRunningEnvOps(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT    NOT NULL UNIQUE,
    password_hash TEXT    NOT NULL,
    created_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS hosts (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL,
    addr       TEXT    NOT NULL,
    port       INTEGER NOT NULL DEFAULT 22,
    username   TEXT    NOT NULL,
    auth_type  TEXT    NOT NULL,
    secret_enc TEXT    NOT NULL DEFAULT '',
    -- escalate_enc 是提权口令（root 口令）的密文。
    -- 留空表示没单独配这一份，提权时退回登录凭据（见 api.hostEscalationSecret）。
    escalate_enc TEXT  NOT NULL DEFAULT '',
    group_name TEXT    NOT NULL DEFAULT '',
    remark     TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_hosts_group ON hosts(group_name);

-- local_privilege 是宿主机的提权账号，单行表（id 恒为 1）。
--
-- 宿主机不在 hosts 里：它没有 SSH 地址，也不该被当成可删除的远端记录；
-- 但提权口令需要持久化，因此单独一张表，避免为了存一份口令而在主机表里
-- 造一条地址为空的假记录，牵连列表、探测与删除逻辑。
CREATE TABLE IF NOT EXISTS local_privilege (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    account    TEXT    NOT NULL,
    secret_enc TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- plugins 表不在这里创建：它的唯一约束需要随"每台主机各装一份"的模型
-- 从 UNIQUE(org, name) 改成 UNIQUE(org, name, host_id)，而 SQLite 无法
-- ALTER 约束，只能重建表。因此它由版本号迁移统一负责，见 migratePluginsPerHost。
--
-- 也正因为如此，这里不能出现与它相关的 CREATE INDEX：迁移在下面才跑，
-- 建索引时列还不存在的话，一个正常的旧库会直接启动失败。

-- tasks / task_results 记录任务的元数据：批量执行（多主机跑同一条命令）与
-- 单台主机上的文件操作（哈希 / 压缩 / 解压 / 上传 / 下载，由 kind 区分）。
--
-- 刻意不存命令输出正文：单机输出上限 256 KiB 而单批上限 200 台，
-- 一次任务的理论输出可达 50 MB。这是个内嵌单二进制里的小库，
-- 存正文会让它迅速膨胀。历史记录真正需要的是
-- "哪台机器、成没成、退出码多少、花了多久"，输出属于当场看完就结束的东西。
CREATE TABLE IF NOT EXISTS tasks (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    command     TEXT    NOT NULL,
    timeout_sec INTEGER NOT NULL DEFAULT 0,
    -- running 之外有两种终态：done（正常跑完，含中途被客户端断开）与
    -- interrupted（面板进程重启时残留的 running，见 interruptRunningTasks）。
    status      TEXT    NOT NULL DEFAULT 'running',
    total       INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    -- 未结束时为 0，前端按 status 判断是否展示，不看这个字段。
    finished_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS task_results (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id     INTEGER NOT NULL,
    host_id     INTEGER NOT NULL,
    -- 主机名冗余存一份：主机随时可能被删除，而历史任务应当保留当时的名字，
    -- 否则一条昨天的记录会变成"主机 7"这种读不懂的东西。
    host_name   TEXT    NOT NULL DEFAULT '',
    -- success / failed / error，与前端 BatchWindow 的状态取值同名。
    status      TEXT    NOT NULL,
    exit_code   INTEGER NOT NULL DEFAULT -1,
    error       TEXT    NOT NULL DEFAULT '',
    note        TEXT    NOT NULL DEFAULT '',
    duration_ms INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_task_results_task ON task_results(task_id);

-- task_items 是文件类任务的逐条明细。
--
-- 批量执行不需要它："哪台机器、成没成"就是全部信息。而哈希算出来的清单、
-- 一个压缩包里有哪些条目，本身就是用户要回看的产物 —— 只记一句"N 个文件"
-- 等于没记，下次还得重算一遍。
--
-- 与 task_results 同样是"结果"而非"输出"：这里存的是被处理的路径与摘要，
-- 不含命令的原始输出，因此体量可控（每条几十到几百字节）。
CREATE TABLE IF NOT EXISTS task_items (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id INTEGER NOT NULL,
    path    TEXT    NOT NULL,
    -- 文件字节数；-1 表示这条路拿不到大小（走目标机命令算哈希时 sha256sum
    -- 不输出大小），界面据此显示"—"而不是一个假的 0。
    size    INTEGER NOT NULL DEFAULT -1,
    hash    TEXT    NOT NULL DEFAULT '',
    error   TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_task_items_task ON task_items(task_id);

-- notifications 是通知中心的消息。
--
-- category 区分来源（task / host / security），前端据此分类展示并决定点击行为；
-- level 是严重程度（info / success / warning / error），只用于配色。
-- 两者分开：同样是"主机"来源，"恢复上线"与"连接失败"的 category 相同而 level 相反。
--
-- ref 指向关联对象（目前只有任务通知用它存 tasks.id），前端据此把通知变成
-- 可点击的入口 —— 一条"任务 #12 有 3 台失败"的通知，点进去直接看详情才有用。
-- 没有关联对象时为 0。
CREATE TABLE IF NOT EXISTS notifications (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    category   TEXT    NOT NULL,
    level      TEXT    NOT NULL,
    title      TEXT    NOT NULL,
    body       TEXT    NOT NULL DEFAULT '',
    ref        INTEGER NOT NULL DEFAULT 0,
    read       INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_notifications_created ON notifications(created_at);

-- 未读计数是最高频的查询（铃铛角标要轮询），而它只关心极小一部分行。
-- 用部分索引只收 read = 0 的记录：表可以无限增长，这个索引始终只有未读那几条。
CREATE INDEX IF NOT EXISTS idx_notifications_unread ON notifications(id) WHERE read = 0;

-- desktop_layouts 保存每个用户自己的桌面布局（图标显隐与文件夹）。
--
-- 整份布局存一列 JSON，而不是拆成 items / folders 两张表：
-- 桌面始终是"整块一起改"的配置，拆表后每次拖一个图标都要跨表维护顺序与归属，
-- 换来的查询能力这里一条都用不上。
CREATE TABLE IF NOT EXISTS desktop_layouts (
    user_id    INTEGER PRIMARY KEY,
    layout     TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
);

-- env_ops / env_op_steps 记录环境管理操作（软件安装、升级、换源、回滚）。
--
-- 与 tasks 的关键差异：批量任务刻意不存命令输出，而环境操作的步骤输出是
-- 排障与审计的核心（一次操作通常只有几步到十几步），因此每步的 stdout/stderr
-- 都截断后落库；由写入方在应用层把每步输出限制在固定大小内。
CREATE TABLE IF NOT EXISTS env_ops (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    host_id       INTEGER NOT NULL,
    host_name     TEXT    NOT NULL DEFAULT '',
    -- install / upgrade / remove / mirror / rollback
    action        TEXT    NOT NULL,
    -- 人类可读的操作目标，如 "nodejs=22.*、nginx"，列表页直接展示。
    target        TEXT    NOT NULL DEFAULT '',
    -- 预检阶段得出的变更计划（JSON 原样留存），供事后复核"当时预告了什么"。
    plan_json     TEXT    NOT NULL DEFAULT '',
    -- running / success / failed / interrupted
    status        TEXT    NOT NULL DEFAULT 'running',
    compensated   INTEGER NOT NULL DEFAULT 0,
    -- verify 为假表示命令跑完了但版本复核不通过，不能当作干净的成功。
    verified      INTEGER NOT NULL DEFAULT 1,
    verify_note   TEXT    NOT NULL DEFAULT '',
    -- 快照在面板数据目录下的位置，不对外暴露。
    snapshot_dir  TEXT    NOT NULL DEFAULT '',
    -- 回滚操作指向它所撤销的那次操作。
    origin_op_id  INTEGER NOT NULL DEFAULT 0,
    summary       TEXT    NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    finished_at   INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_env_ops_host ON env_ops(host_id, id);

CREATE TABLE IF NOT EXISTS env_op_steps (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    op_id       INTEGER NOT NULL,
    seq         INTEGER NOT NULL,
    name        TEXT    NOT NULL,
    -- running / success / failed / skipped
    status      TEXT    NOT NULL,
    exit_code   INTEGER NOT NULL DEFAULT -1,
    stdout      TEXT    NOT NULL DEFAULT '',
    stderr      TEXT    NOT NULL DEFAULT '',
    duration_ms INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_env_op_steps_op ON env_op_steps(op_id, seq);

-- launch_apps 记录"安装后可以在面板里打开"的目录软件。
--
-- 与插件安装记录同理按主机切片：同一款工具装在两台机器上，桌面上就是两个入口。
-- host_id=0 表示宿主机，它不对应 hosts 表，因此这里不能加外键。
-- 记录由目录探测与安装成功两条路径维护，卸载/探测不到时删除。
CREATE TABLE IF NOT EXISTS launch_apps (
    host_id      INTEGER NOT NULL,
    slug         TEXT    NOT NULL,
    installed_at INTEGER NOT NULL,
    PRIMARY KEY (host_id, slug)
);

-- AI 助手的会话。一个会话就是一段彼此独立的对话。
--
-- title 在第一次落库时由服务端取用户第一句话生成，之后用于会话列表；
-- 用户清空某个会话时会被重置为空串，列表因此显示"新对话"而不是一个
-- 早已对不上内容的旧标题。
CREATE TABLE IF NOT EXISTS ai_sessions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    title      TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- AI 助手每个会话的对话存档，一个会话至多一行。
--
-- 存的是**模型看到的对话历史本身**，而不是界面上的消息流。窗口关掉再打开
-- 时，用户不只要看到之前聊过什么，更不希望模型把上下文忘掉 —— 这两件事
-- 都靠这一份数据，存两份反而会出现"界面显示的和模型记得的不一致"。
--
-- 历史经 trimHistory 裁剪后有明确上限（40 条 / 256 KiB），因此整份 JSON
-- 一次覆盖写就够，不值得为分片存储引入复杂度。
--
-- session_id 既是主键也是外键：会话被删除时这一行随之消失，不会留下
-- 一份没人认领的历史（否则新建会话若复用到同一个 id，就会读到别人的对话）。
--
-- tool_status_json 补的是模型历史里没有的信息：ai.Message 只有 role/content，
-- 而界面上"已执行的命令"那张卡片必须分清成功与失败，否则用户回看历史时
-- 会把一条失败的命令当成成功的。形如 {"call_abc":true,"call_def":false}。
CREATE TABLE IF NOT EXISTS ai_conversation (
    session_id       INTEGER PRIMARY KEY REFERENCES ai_sessions(id) ON DELETE CASCADE,
    history_json     TEXT    NOT NULL DEFAULT '',
    tool_status_json TEXT    NOT NULL DEFAULT '{}',
    updated_at       INTEGER NOT NULL
);
`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}

	// 结构性变更走版本号迁移。上面那批 CREATE TABLE IF NOT EXISTS 只负责
	// "新库从零建出当前形状"，无法表达"旧库要改形状"—— SQLite 改不了约束，
	// plugins 表必须整表重建，而重建只能做一次，不能每次启动都做。
	if err := s.migrateSchema(); err != nil {
		return err
	}

	// hosts.escalate_enc 是后加的列：早于它的库只有一份登录凭据。
	// 空默认值正好表达"没单独配提权口令"，提权时退回登录凭据。
	if err := s.ensureColumn("hosts", "escalate_enc",
		`ALTER TABLE hosts ADD COLUMN escalate_enc TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}

	// hosts.privilege 是提权机制短暂存在时加的列，那套机制已经移除。
	// 留着它不会出错，但会变成一列没人读也没人写的死字段 —— 后来的人
	// 会花时间去猜它是干什么的。清掉它，理由见 dropColumn。
	s.dropColumn("hosts", "privilege")

	// tasks 上的三列是"文件操作并入任务记录"时加的：早于它们的库只装批量执行。
	// kind 默认 'batch' 正好表达"这些历史记录都是批量执行"，不需要额外的数据订正。
	if err := s.ensureColumn("tasks", "kind",
		`ALTER TABLE tasks ADD COLUMN kind TEXT NOT NULL DEFAULT 'batch'`); err != nil {
		return err
	}
	// host_name 只在文件类任务上有值：列表页要显示"对哪台机器做的"，
	// 而列表不查结果表（那会变成 N+1），所以在这里冗余存一份。
	if err := s.ensureColumn("tasks", "host_name",
		`ALTER TABLE tasks ADD COLUMN host_name TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	// algo 是哈希任务的算法名，其余种类为空。
	if err := s.ensureColumn("tasks", "algo",
		`ALTER TABLE tasks ADD COLUMN algo TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	return nil
}

// ---------- 版本号迁移 ----------

// schemaVersionKey 是 meta 表里存 schema 版本号的键。
const schemaVersionKey = "schema.version"

// schemaMigrations 是全部结构性变更，下标即目标版本号。
//
// 为什么不继续用 CREATE TABLE IF NOT EXISTS + ensureColumn 打补丁：
// 那套只能表达"加一列"这种幂等操作。而 SQLite 不支持 ALTER 约束，
// 一旦要改唯一约束就只能整表重建 —— 重建不是幂等的，跑第二次会把
// 数据搬空。版本号让"这件事只做一次"变成显式事实，而不是靠运气：
// 每个迁移函数自己判断前置形态是否存在，执行完就把版本号推进一格。
//
// 新增迁移时只许在末尾追加，不许改动已有项 —— 已经跑过的库不会再回头。
var schemaMigrations = []func(*Store) error{
	migratePluginsPerHost,
	migrateDshCommandsPerHost,
	migrateAISessions,
}

// migrateSchema 把库推进到最新版本。
//
// 早于版本机制的库没有这个键，从 0 起跑 —— 这正是我们要的：
// 它们需要完整跑一遍历史迁移，把自己补成当前形状。
func (s *Store) migrateSchema() error {
	current := 0
	raw, err := s.GetMeta(schemaVersionKey)
	if err != nil {
		return err
	}
	if raw != "" {
		if _, err := fmt.Sscanf(raw, "%d", &current); err != nil {
			return fmt.Errorf("schema.version 无法解析（值=%q）：%w", raw, err)
		}
	}

	for i := current; i < len(schemaMigrations); i++ {
		if err := schemaMigrations[i](s); err != nil {
			return fmt.Errorf("执行 schema 迁移 %d 失败：%w", i+1, err)
		}
		if err := s.SetMeta(schemaVersionKey, fmt.Sprint(i+1)); err != nil {
			return err
		}
	}
	return nil
}

// migratePluginsPerHost（版本 1）把 plugins 从"全局一份"改成"每台主机各一份"。
//
// 原来的唯一约束是 UNIQUE(org, name)，现在必须是 UNIQUE(org, name, host_id)，
// 否则同一插件装到第二台主机时会撞唯一键。SQLite 的 ALTER 不支持改约束，
// 只能走"建新表 → 搬数据 → 删旧表 → 改名"。
//
// 存量记录一律落 host_id = 0，也就是宿主机（api.LocalHostID）：
// 旧模型下插件本来就装在面板所在的这台机器上，语义完全一致，
// 而且 0 恰好是 Go 的零值，不需要额外的数据订正。
func migratePluginsPerHost(s *Store) error {
	exists, err := s.tableExists("plugins")
	if err != nil {
		return err
	}
	if !exists {
		// 全新库：没有旧数据要搬，直接建成目标形状即可。
		// 基础 schema 里刻意没有 plugins 的建表语句，因此这里一定是从零建。
		if _, err := s.db.Exec(`CREATE TABLE plugins (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    org          TEXT    NOT NULL,
    name         TEXT    NOT NULL,
    display_name TEXT    NOT NULL DEFAULT '',
    description  TEXT    NOT NULL DEFAULT '',
    version      TEXT    NOT NULL DEFAULT '',
    entry        TEXT    NOT NULL DEFAULT '',
    icon         TEXT    NOT NULL DEFAULT '',
    source       TEXT    NOT NULL DEFAULT 'archive',
    capabilities TEXT    NOT NULL DEFAULT '',
    granted      TEXT    NOT NULL DEFAULT '',
    enabled      INTEGER NOT NULL DEFAULT 1,
    host_id      INTEGER NOT NULL DEFAULT 0,
    installed_at INTEGER NOT NULL,
    UNIQUE(org, name, host_id)
)`); err != nil {
			return err
		}
		if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_plugins_host ON plugins(host_id)`); err != nil {
			return err
		}
		return nil
	}

	// source 列是后来加的：更早的库没有它，搬运时要兜个默认值，
	// 否则 SELECT 会直接报"no such column"。
	hasSource, err := s.hasColumn("plugins", "source")
	if err != nil {
		return err
	}
	sourceExpr := `'archive'`
	if hasSource {
		sourceExpr = `source`
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := createPluginsTable(tx, "plugins_new"); err != nil {
		return err
	}

	// 搬运要显式对齐两边的列名。旧库未必有 source 列，用表达式补默认值。
	columns := `org, name, display_name, description, version, entry, icon, %s, capabilities, granted, enabled, 0, installed_at`
	target := `org, name, display_name, description, version, entry, icon, source, capabilities, granted, enabled, host_id, installed_at`
	// 刻意不搬 id：它只是内部主键，没有任何外部引用，重排一遍不影响语义，
	// 还能让新表与其它表一样保持连续自增。
	if _, err := tx.Exec(fmt.Sprintf(
		`INSERT INTO plugins_new (%s) SELECT `+columns+` FROM plugins`, target, sourceExpr)); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE plugins`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE plugins_new RENAME TO plugins`); err != nil {
		return err
	}
	// 索引放在搬完之后建。空老库下这一步是"建表时就建索引"，
	// 非空老库下则是一次普通建索引，两条路都是幂等的。
	if err := createPluginsIndexes(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// createPluginsTable 建出 plugins 表的目标形状。表名由本文件字面量给定，
// 不存在把外部输入拼进 SQL 的问题；之所以要参数化，是因为重建过程中
// 需要先建成 plugins_new 再改名。
//
// 必须接收 tx 而不是用 s.db：重建的三步（建新表、搬数据、改名）是一个整体，
// 中途失败要能整体回滚。走 s.db 的话建表会落在事务之外 —— 事务一失败，
// 库里就留下一个孤零零的 plugins_new，下次启动重建直接撞表名。
func createPluginsTable(tx *sql.Tx, table string) error {
	_, err := tx.Exec(fmt.Sprintf(`
CREATE TABLE %s (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    org          TEXT    NOT NULL,
    name         TEXT    NOT NULL,
    display_name TEXT    NOT NULL DEFAULT '',
    description  TEXT    NOT NULL DEFAULT '',
    version      TEXT    NOT NULL DEFAULT '',
    entry        TEXT    NOT NULL DEFAULT '',
    icon         TEXT    NOT NULL DEFAULT '',
    -- 安装来源：archive（用户上传的包）或 builtin（随面板内置）。
    source       TEXT    NOT NULL DEFAULT 'archive',
    capabilities TEXT    NOT NULL DEFAULT '',
    granted      TEXT    NOT NULL DEFAULT '',
    enabled      INTEGER NOT NULL DEFAULT 1,
    -- host_id 指明插件装在哪台主机上，0 是宿主机（面板自己所在的那台）。
    -- 唯一约束必须带上它，否则同一插件无法同时装到多台主机。
    host_id      INTEGER NOT NULL DEFAULT 0,
    installed_at INTEGER NOT NULL,
    UNIQUE(org, name, host_id)
)`, table))
	return err
}

// createPluginsIndexes 建出 plugins 上的索引。
//
// 每次打开插件窗口、桌面每次刷新图标都要按 host_id 过滤一次，
// 属于"高频且形状固定"的查询，给它一个索引是划算的。
func createPluginsIndexes(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_plugins_host ON plugins(host_id)`)
	return err
}

// legacyDshCommandKey 是 dsh 启动命令在旧模型里用的 meta 键。
//
// 旧实现只能让 dsh 跑在宿主机上，因此"一条全局命令"就够。现在每台主机各跑
// 一个实例，命令必须按主机分别保存。这个键只在下面那次迁移里被读一次，
// 之后不再有任何读写点。
const legacyDshCommandKey = "dsh.command"

// migrateDshCommandsPerHost（版本 2）把 dsh 的启动命令从全局 meta 键改成每台主机一份。
//
// 存量值的归属没有歧义：旧模型下 dsh 就装在宿主机（面板自己所在的那台）上，
// 因此无条件落到 host_id = 0，与插件迁移的取舍一致。
//
// 搬完即删掉旧键：留着会让"按主机读"与"读全局键"两份真值来源并存，
// 迟早出现界面显示 A、实际生效 B 这种没人能一眼看穿的错。
func migrateDshCommandsPerHost(s *Store) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// 一张主机一行：host_id 作主键，本身就是"一台主机只有一条命令"这个约束。
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS dsh_hosts (
    host_id    INTEGER PRIMARY KEY,
    command    TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
)`); err != nil {
		return err
	}

	var legacy string
	err = tx.QueryRow(`SELECT value FROM meta WHERE key = ?`, legacyDshCommandKey).Scan(&legacy)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 全新库，或用户从未配过命令：没有要搬的东西。
	case err != nil:
		return err
	case strings.TrimSpace(legacy) != "":
		// DO NOTHING 而不是 DO UPDATE：万一这个迁移在某次异常中被重跑，
		// 用户此后手填的命令不该被旧值覆盖回去。
		if _, err := tx.Exec(
			`INSERT INTO dsh_hosts(host_id, command, updated_at) VALUES(0, ?, ?)
			 ON CONFLICT(host_id) DO NOTHING`,
			legacy, time.Now().Unix(),
		); err != nil {
			return err
		}
	}

	// 无论旧键有没有值都删掉：它的存在本身就是"还有一份全局真值"的暗示。
	if _, err := tx.Exec(`DELETE FROM meta WHERE key = ?`, legacyDshCommandKey); err != nil {
		return err
	}
	return tx.Commit()
}

// migrateAISessions（版本 3）把"助手只有一条对话"改成"多个会话各存一份"。
//
// 旧表的形状是 id INTEGER PRIMARY KEY，固定一行；新形状必须以 session_id
// 作主键并外键指向 ai_sessions。SQLite 改不了主键，只能整表重建。
//
// 存量那一条对话不能被丢掉：它是用户此前唯一的助手记录。只要它有内容，
// 就搬进一个会话里，标题留空（下一轮对话结束时会按第一句话补上）；
// 没有内容则什么也不建，新库自然就是"还没有任何会话"。
func migrateAISessions(s *Store) error {
	hasSession, err := s.hasColumn("ai_conversation", "session_id")
	if err != nil {
		return err
	}
	if hasSession {
		// 全新库（基础 schema 已是新形状），或这个迁移已经跑过。
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		historyJSON string
		statusJSON  string
		updatedAt   int64
	)
	err = tx.QueryRow(
		`SELECT history_json, tool_status_json, updated_at FROM ai_conversation WHERE id = 1`).
		Scan(&historyJSON, &statusJSON, &updatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 从没聊过：没有要搬的东西。
	case err != nil:
		return err
	}

	if _, err := tx.Exec(`CREATE TABLE ai_conversation_new (
    session_id       INTEGER PRIMARY KEY REFERENCES ai_sessions(id) ON DELETE CASCADE,
    history_json     TEXT    NOT NULL DEFAULT '',
    tool_status_json TEXT    NOT NULL DEFAULT '{}',
    updated_at       INTEGER NOT NULL
)`); err != nil {
		return err
	}

	if strings.TrimSpace(historyJSON) != "" {
		if updatedAt == 0 {
			updatedAt = time.Now().Unix()
		}
		res, err := tx.Exec(
			`INSERT INTO ai_sessions(title, created_at, updated_at) VALUES('', ?, ?)`,
			updatedAt, updatedAt)
		if err != nil {
			return err
		}
		sessionID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO ai_conversation_new(session_id, history_json, tool_status_json, updated_at)
			 VALUES(?, ?, ?, ?)`,
			sessionID, historyJSON, statusJSON, updatedAt); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(`DROP TABLE ai_conversation`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE ai_conversation_new RENAME TO ai_conversation`); err != nil {
		return err
	}
	return tx.Commit()
}

// tableExists 判断表是否已存在。
func (s *Store) tableExists(table string) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(1) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ensureColumn 在列不存在时执行一次 ALTER。
func (s *Store) ensureColumn(table, column, alter string) error {
	exists, err := s.hasColumn(table, column)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = s.db.Exec(alter)
	return err
}

// hasColumn 判断表里是否已经有这一列。
//
// 用 PRAGMA 查，而不是去匹配 "duplicate column name" 这个驱动错误文案：
// 文案随驱动版本变化，会让启动直接失败。
func (s *Store) hasColumn(table, column string) (bool, error) {
	rows, err := s.db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notNull   int
			dfltValue any
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// dropColumn 删掉一列；列不存在时什么都不做。
//
// 失败被吞掉是有意的：SQLite 3.35 以下不支持 DROP COLUMN，而留着一列代码
// 已经不再读写的字段，代价远小于让整个服务起不来。表名与列名都来自本文件的
// 字面量，不存在把外部输入拼进 SQL 的问题。
func (s *Store) dropColumn(table, column string) {
	exists, err := s.hasColumn(table, column)
	if err != nil || !exists {
		return
	}
	_, _ = s.db.Exec(fmt.Sprintf(`ALTER TABLE %s DROP COLUMN %s`, table, column))
}

// splitList / joinList 负责能力列表在单列中的序列化。
// 能力名本身不含逗号，因此用逗号分隔是安全的。
func splitList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{}
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func joinList(items []string) string { return strings.Join(items, ",") }

// ---------- 元数据 ----------

// GetMeta 读取元数据；键不存在时返回空串而不是错误 ——
// 调用方关心的是"有没有值"，不是"查不到"。
func (s *Store) GetMeta(key string) (string, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return value, nil
}

// SetMeta 写入元数据，键已存在则覆盖。
func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO meta(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	return err
}

// ---------- 用户 ----------

// User 是面板的登录账号。
type User struct {
	ID           int64
	Username     string
	PasswordHash string
}

// CountUsers 返回账号总数，用于判断是否需要初始化管理员。
func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(1) FROM users`).Scan(&n)
	return n, err
}

// CreateUser 新增账号，passwordHash 应为 bcrypt 结果。
func (s *Store) CreateUser(username, passwordHash string) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO users(username, password_hash, created_at) VALUES(?,?,?)`,
		username, passwordHash, time.Now().Unix(),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetUserByUsername 按用户名查询账号。
func (s *Store) GetUserByUsername(username string) (*User, error) {
	return s.scanUser(s.db.QueryRow(
		`SELECT id, username, password_hash FROM users WHERE username = ?`, username))
}

// GetUserByID 按主键查询账号。
func (s *Store) GetUserByID(id int64) (*User, error) {
	return s.scanUser(s.db.QueryRow(
		`SELECT id, username, password_hash FROM users WHERE id = ?`, id))
}

func (s *Store) scanUser(row *sql.Row) (*User, error) {
	u := &User{}
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return u, nil
}

// UpdateUserPassword 更新账号密码哈希，目标不存在时返回 ErrNotFound。
func (s *Store) UpdateUserPassword(id int64, passwordHash string) error {
	res, err := s.db.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ChangePasswordAndSetMeta 在同一个事务里更新密码哈希并写入一条 meta。
//
// 两者必须原子：改密码后要轮换会话签名密钥（epoch 存在 meta 里），
// 这属于"一次改密操作"的两个部分。分开写时，若第二步失败会留下
// "密码已改、但旧会话仍然全部有效"的不一致状态 —— 用户看到 500 以为没改成，
// 实际上密码已经变了，而该失效的历史会话一个都没失效。
func (s *Store) ChangePasswordAndSetMeta(id int64, passwordHash, metaKey, metaValue string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	// 出错路径统一回滚；已提交时 Rollback 是无害的。
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}

	if _, err := tx.Exec(
		`INSERT INTO meta(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		metaKey, metaValue,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------- 桌面布局 ----------

// GetDesktopLayout 读取用户的桌面布局 JSON。
//
// 没有记录时返回空串而不是错误：调用方据此走默认布局，
// "新用户还没改过桌面"不是异常。
func (s *Store) GetDesktopLayout(userID int64) (string, error) {
	var layout string
	err := s.db.QueryRow(`SELECT layout FROM desktop_layouts WHERE user_id = ?`, userID).Scan(&layout)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return layout, nil
}

// SetDesktopLayout 覆盖用户的桌面布局。
func (s *Store) SetDesktopLayout(userID int64, layout string) error {
	_, err := s.db.Exec(
		`INSERT INTO desktop_layouts(user_id, layout, updated_at) VALUES(?,?,?)
		 ON CONFLICT(user_id) DO UPDATE SET layout = excluded.layout, updated_at = excluded.updated_at`,
		userID, layout, time.Now().Unix(),
	)
	return err
}

// ---------- 主机 ----------

// Host 是对外暴露的主机信息，不包含任何凭据。
type Host struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Addr     string `json:"addr"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	AuthType string `json:"authType"`
	// HasSecret 表示这台主机是否已经配了登录凭据。
	//
	// 新增主机允许先只填地址、端口、用户名，凭据以后再补 —— 先把机器登记进来、
	// 确认网络可达，是真实存在的需求（机器还没交付、凭据还在别人手里）。
	// 但"有没有凭据"必须让界面知道：否则连接按钮看起来可用，点下去才报错。
	// 这里只暴露布尔，凭据本身仍然不出库。
	HasSecret bool      `json:"hasSecret"`
	Group     string    `json:"group"`
	Remark    string    `json:"remark"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// HostInput 是写入主机时的字段集合。
// SecretEnc / EscalateEnc 传空串时，更新语义为"保持原值不变"。
type HostInput struct {
	Name        string
	Addr        string
	Port        int
	Username    string
	AuthType    string
	SecretEnc   string
	EscalateEnc string
	Group       string
	Remark      string
}

// ListHosts 返回全部主机，按分组与名称排序。
func (s *Store) ListHosts() ([]Host, error) {
	rows, err := s.db.Query(`
		SELECT id, name, addr, port, username, auth_type, secret_enc <> '',
		       group_name, remark, created_at, updated_at
		FROM hosts ORDER BY group_name, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	hosts := make([]Host, 0)
	for rows.Next() {
		var h Host
		var created, updated int64
		if err := rows.Scan(&h.ID, &h.Name, &h.Addr, &h.Port, &h.Username, &h.AuthType,
			&h.HasSecret, &h.Group, &h.Remark, &created, &updated); err != nil {
			return nil, err
		}
		h.CreatedAt = time.Unix(created, 0)
		h.UpdatedAt = time.Unix(updated, 0)
		hosts = append(hosts, h)
	}
	return hosts, rows.Err()
}

// GetHost 按主键查询主机。
func (s *Store) GetHost(id int64) (*Host, error) {
	var h Host
	var created, updated int64
	err := s.db.QueryRow(`
		SELECT id, name, addr, port, username, auth_type, secret_enc <> '',
		       group_name, remark, created_at, updated_at
		FROM hosts WHERE id = ?`, id).
		Scan(&h.ID, &h.Name, &h.Addr, &h.Port, &h.Username, &h.AuthType,
			&h.HasSecret, &h.Group, &h.Remark, &created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	h.CreatedAt = time.Unix(created, 0)
	h.UpdatedAt = time.Unix(updated, 0)
	return &h, nil
}

// GetHostSecrets 返回主机两种加密后的口令：登录凭据与提权口令。
//
// 一次取两列而不是拆成两个方法：调用方本来就要判断"提权口令为空则退回
// 登录凭据"，分两次查既多一次往返，也让"两份口令属于同一行"只能靠约定保证。
func (s *Store) GetHostSecrets(id int64) (loginEnc, escalateEnc string, err error) {
	err = s.db.QueryRow(`SELECT secret_enc, escalate_enc FROM hosts WHERE id = ?`, id).
		Scan(&loginEnc, &escalateEnc)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", ErrNotFound
		}
		return "", "", err
	}
	return loginEnc, escalateEnc, nil
}

// GetLocalPrivilege 返回宿主机的提权账号与口令密文。
//
// 没配过时返回空串而不是 ErrNotFound：调用方只关心"有没有可用口令"，
// 把"没配"当成正常状态处理，比每次先判错误更直接。
func (s *Store) GetLocalPrivilege() (account, secretEnc string, err error) {
	err = s.db.QueryRow(`SELECT account, secret_enc FROM local_privilege WHERE id = 1`).
		Scan(&account, &secretEnc)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	return account, secretEnc, nil
}

// SaveLocalPrivilege 写入宿主机的提权账号，单行覆盖。
func (s *Store) SaveLocalPrivilege(account, secretEnc string) error {
	_, err := s.db.Exec(`
		INSERT INTO local_privilege(id, account, secret_enc, updated_at)
		VALUES(1, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			account = excluded.account,
			secret_enc = excluded.secret_enc,
			updated_at = excluded.updated_at`,
		account, secretEnc, time.Now().Unix())
	return err
}

// ClearLocalPrivilege 移除宿主机的提权账号。
//
// 删除而不是写空串：空口令意味着"没配置"，用一行空记录表达只会让
// GetLocalPrivilege 的调用方多一个判断。
func (s *Store) ClearLocalPrivilege() error {
	_, err := s.db.Exec(`DELETE FROM local_privilege WHERE id = 1`)
	return err
}

// CreateHost 新增主机。
func (s *Store) CreateHost(in HostInput) (int64, error) {
	now := time.Now().Unix()
	res, err := s.db.Exec(`
		INSERT INTO hosts(name, addr, port, username, auth_type, secret_enc, escalate_enc, group_name, remark, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		in.Name, in.Addr, in.Port, in.Username, in.AuthType, in.SecretEnc, in.EscalateEnc,
		in.Group, in.Remark, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateHost 更新主机；in.SecretEnc / in.EscalateEnc 为空串表示保留原值。
//
// 两份口令各自独立判断"空串即保留"，用 NULLIF 把空串折成 NULL、再由
// COALESCE 落回原列，一条语句覆盖四种组合 —— 否则"只改登录凭据"和
// "只改提权口令"要各复制一条 SQL 出来。代价是空串永远写不进去，
// 而这正是调用方要的语义（与登录凭据的既有行为一致）。
//
// 目标不存在时返回 ErrNotFound：UPDATE 影响 0 行并不会报错，
// 必须显式检查 RowsAffected，否则调用方无从区分"更新成功"与"记录不存在"。
func (s *Store) UpdateHost(id int64, in HostInput) error {
	res, err := s.db.Exec(`
		UPDATE hosts SET
			name=?, addr=?, port=?, username=?, auth_type=?, group_name=?, remark=?,
			secret_enc   = COALESCE(NULLIF(?, ''), secret_enc),
			escalate_enc = COALESCE(NULLIF(?, ''), escalate_enc),
			updated_at=?
		WHERE id=?`,
		in.Name, in.Addr, in.Port, in.Username, in.AuthType, in.Group, in.Remark,
		in.SecretEnc, in.EscalateEnc, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteHost 删除主机，目标不存在时返回 ErrNotFound。
func (s *Store) DeleteHost(id int64) error {
	res, err := s.db.Exec(`DELETE FROM hosts WHERE id = ?`, id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------- 插件 ----------

// Plugin 是"某台主机上装的一份插件"的注册信息。
type Plugin struct {
	ID           int64     `json:"id"`
	Org          string    `json:"org"`
	Name         string    `json:"name"`
	DisplayName  string    `json:"displayName"`
	Description  string    `json:"description"`
	Version      string    `json:"version"`
	Entry        string    `json:"entry"`
	Icon         string    `json:"icon"`
	Source       string    `json:"source"`
	Capabilities []string  `json:"capabilities"` // 清单里申请的
	Granted      []string  `json:"granted"`      // 实际授予的
	Enabled      bool      `json:"enabled"`
	HostID       int64     `json:"hostId"` // 0 是宿主机
	InstalledAt  time.Time `json:"installedAt"`
}

// PluginInput 是写入插件记录所需的字段。
type PluginInput struct {
	Org          string
	Name         string
	DisplayName  string
	Description  string
	Version      string
	Entry        string
	Icon         string
	Source       string
	Capabilities []string
	Granted      []string
	HostID       int64
}

const pluginColumns = `id, org, name, display_name, description, version, entry, icon, source, capabilities, granted, enabled, host_id, installed_at`

// rowScanner 兼容 *sql.Row 与 *sql.Rows 两种扫描来源。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanPlugin(scanner rowScanner) (*Plugin, error) {
	var (
		item         Plugin
		capabilities string
		granted      string
		enabled      int
		installedAt  int64
	)
	if err := scanner.Scan(&item.ID, &item.Org, &item.Name, &item.DisplayName, &item.Description,
		&item.Version, &item.Entry, &item.Icon, &item.Source, &capabilities, &granted, &enabled,
		&item.HostID, &installedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	item.Capabilities = splitList(capabilities)
	item.Granted = splitList(granted)
	item.Enabled = enabled != 0
	item.InstalledAt = time.Unix(installedAt, 0)
	return &item, nil
}

// ListPlugins 返回全部主机上的已安装插件。
func (s *Store) ListPlugins() ([]Plugin, error) {
	rows, err := s.db.Query(`SELECT ` + pluginColumns + ` FROM plugins ORDER BY host_id, display_name, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]Plugin, 0)
	for rows.Next() {
		item, err := scanPlugin(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

// GetPlugin 按 (org, name, hostID) 查询插件。
func (s *Store) GetPlugin(org, name string, hostID int64) (*Plugin, error) {
	return scanPlugin(s.db.QueryRow(
		`SELECT `+pluginColumns+` FROM plugins WHERE org = ? AND name = ? AND host_id = ?`,
		org, name, hostID))
}

// GetPluginByID 按主键查询插件。
func (s *Store) GetPluginByID(id int64) (*Plugin, error) {
	return scanPlugin(s.db.QueryRow(
		`SELECT `+pluginColumns+` FROM plugins WHERE id = ?`, id))
}

// ListPluginsOnHost 返回某台主机上的已安装插件。
func (s *Store) ListPluginsOnHost(hostID int64) ([]Plugin, error) {
	rows, err := s.db.Query(
		`SELECT `+pluginColumns+` FROM plugins WHERE host_id = ? ORDER BY display_name, name`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]Plugin, 0)
	for rows.Next() {
		item, err := scanPlugin(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

// ListPluginsByName 返回同一个插件在所有主机上的安装记录。
//
// 静态资源要先在磁盘上定位目录才能读出来，而这个目录是按"插件 + 版本"组织的、
// 与主机无关（本地镜像只保留最新落位的那一份），所以需要跨主机看同一插件的全部记录：
// 靠 InstalledAt 找出本地镜像对应的版本，靠"有没有任意一台仍启用"决定是否放行。
func (s *Store) ListPluginsByName(org, name string) ([]Plugin, error) {
	rows, err := s.db.Query(
		`SELECT `+pluginColumns+` FROM plugins WHERE org = ? AND name = ? ORDER BY installed_at`, org, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]Plugin, 0)
	for rows.Next() {
		item, err := scanPlugin(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

// UpsertPlugin 写入或更新某台主机上的插件记录。
//
// 冲突时刻意不改动 enabled：升级不该把一个被管理员停用的插件悄悄重新启用。
//
// 写入与回读放在同一个事务里：分成两条独立语句时，并发安装同一插件可能读到
// 别人的中间状态，返回一条并不属于本次写入的记录。
func (s *Store) UpsertPlugin(in PluginInput) (*Plugin, error) {
	source := in.Source
	if source == "" {
		source = "archive"
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		INSERT INTO plugins(org, name, display_name, description, version, entry, icon, source, capabilities, granted, enabled, host_id, installed_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,1,?,?)
		ON CONFLICT(org, name, host_id) DO UPDATE SET
			display_name = excluded.display_name,
			description  = excluded.description,
			version      = excluded.version,
			entry        = excluded.entry,
			icon         = excluded.icon,
			source       = excluded.source,
			capabilities = excluded.capabilities,
			granted      = excluded.granted,
			installed_at = excluded.installed_at`,
		in.Org, in.Name, in.DisplayName, in.Description, in.Version, in.Entry, in.Icon,
		source, joinList(in.Capabilities), joinList(in.Granted), in.HostID, time.Now().Unix()); err != nil {
		return nil, err
	}

	item, err := scanPlugin(tx.QueryRow(
		`SELECT `+pluginColumns+` FROM plugins WHERE org = ? AND name = ? AND host_id = ?`,
		in.Org, in.Name, in.HostID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return item, nil
}

// DeletePlugin 删除某台主机上的插件记录。
func (s *Store) DeletePlugin(org, name string, hostID int64) error {
	res, err := s.db.Exec(
		`DELETE FROM plugins WHERE org = ? AND name = ? AND host_id = ?`, org, name, hostID)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetPluginEnabled 启用或停用插件。
func (s *Store) SetPluginEnabled(id int64, enabled bool) error {
	value := 0
	if enabled {
		value = 1
	}
	res, err := s.db.Exec(`UPDATE plugins SET enabled = ? WHERE id = ?`, value, id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetPluginGranted 更新已授予的能力。
func (s *Store) SetPluginGranted(id int64, granted []string) error {
	res, err := s.db.Exec(`UPDATE plugins SET granted = ? WHERE id = ?`, joinList(granted), id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------- DSH 工作台的每主机启动命令 ----------

// GetDshCommand 读取某台主机上 dsh 的启动命令。
//
// 没配过时返回空串而不是错误：调用方关心的是"配没配"，
// 不是"查不到"（与 GetMeta 同一约定）。
func (s *Store) GetDshCommand(hostID int64) (string, error) {
	var command string
	err := s.db.QueryRow(`SELECT command FROM dsh_hosts WHERE host_id = ?`, hostID).Scan(&command)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return command, nil
}

// SetDshCommand 写入某台主机的启动命令，已有则覆盖。
func (s *Store) SetDshCommand(hostID int64, command string) error {
	_, err := s.db.Exec(
		`INSERT INTO dsh_hosts(host_id, command, updated_at) VALUES(?, ?, ?)
		 ON CONFLICT(host_id) DO UPDATE SET command = excluded.command,
		                                     updated_at = excluded.updated_at`,
		hostID, command, time.Now().Unix(),
	)
	return err
}

// DeleteDshCommand 删掉某台主机的启动命令，供卸载插件时清理。
//
// 记录不存在时什么都不做：卸载要的是"结果是没有命令"这个事实，
// 而不是"必须删掉一行"。
func (s *Store) DeleteDshCommand(hostID int64) error {
	_, err := s.db.Exec(`DELETE FROM dsh_hosts WHERE host_id = ?`, hostID)
	return err
}

// ---------- 批量任务 ----------

// 任务状态。
const (
	TaskRunning = "running"
	TaskDone    = "done"
	// TaskInterrupted 表示任务没有正常收尾：或是面板进程重启时残留，
	// 或是客户端中途断开导致后半程的主机没跑。
	TaskInterrupted = "interrupted"
)

// 单台主机的执行结果。取值与前端 BatchWindow 的 Status 同名，
// 前端拿到后可以直接喂给同一套状态展示逻辑。
const (
	ResultSuccess = "success"
	ResultFailed  = "failed"
	ResultError   = "error"
)

// 任务种类。
//
// batch 是"多台主机跑同一条命令"，其余都是文件操作。两者共用一张表而不是
// 各建一张：任务记录窗口、计数聚合、状态配色都只有一套，列表按 kind 换个
// 措辞即可，历史也只有一个入口。
const (
	TaskKindBatch    = "batch"
	TaskKindHash     = "hash"
	TaskKindCompress = "compress"
	TaskKindExtract  = "extract"
	TaskKindUpload   = "upload"
	TaskKindDownload = "download"
	// TaskKindTransfer 是跨主机复制 / 移动：源端与目标端是两台不同的机器。
	TaskKindTransfer = "transfer"
)

// Task 是一次任务：批量执行，或单台主机上的一次文件操作。
//
// 三个计数由 SQL 聚合随列表一起返回，而不是让前端逐条去数结果表：
// 列表一屏二十条任务、每条可能几百个结果，逐个拉取就是典型的 N+1。
type Task struct {
	ID           int64     `json:"id"`
	Command      string    `json:"command"`
	TimeoutSec   int       `json:"timeoutSec"`
	Status       string    `json:"status"`
	Total        int       `json:"total"`
	SuccessCount int       `json:"successCount"`
	FailedCount  int       `json:"failedCount"`
	ErrorCount   int       `json:"errorCount"`
	CreatedAt    time.Time `json:"createdAt"`
	FinishedAt   time.Time `json:"finishedAt"`
	// Kind 是任务种类，取值见 TaskKind* 常量。
	Kind string `json:"kind"`
	// HostName 只在文件类任务上有值：列表页要显示"对哪台机器做的"。
	HostName string `json:"hostName"`
	// Algo 是哈希任务的算法名（sha256 等），其余种类为空。
	Algo string `json:"algo"`
}

// TaskResult 是某台主机在本次任务中的结果。不含命令输出。
type TaskResult struct {
	HostID     int64  `json:"hostId"`
	HostName   string `json:"hostName"`
	Status     string `json:"status"`
	ExitCode   int    `json:"exitCode"`
	Error      string `json:"error"`
	Note       string `json:"note"`
	DurationMS int64  `json:"durationMs"`
}

// TaskDetail 是任务详情：元数据加全部主机结果与文件明细。
type TaskDetail struct {
	Task    Task         `json:"task"`
	Results []TaskResult `json:"results"`
	// Items 是文件类任务的逐条明细；批量执行恒为空数组（不是 null，
	// 前端可以无条件地按数组渲染）。
	Items []TaskItem `json:"items"`
}

// TaskItem 是文件类任务的一条明细：一个被处理的文件。
//
// 它对批量执行没有意义（那边的结果是"哪台机器成没成"），但哈希的清单、
// 压缩包里有哪些条目，正是用户回看历史时想看到的东西。
type TaskItem struct {
	Path string `json:"path"`
	// Size 为文件字节数；-1 表示这条路拿不到大小（走目标机命令算哈希时
	// sha256sum 不输出大小），界面据此显示"—"而不是一个假的 0。
	Size  int64  `json:"size"`
	Hash  string `json:"hash"`
	Error string `json:"error"`
}

// CreateTask 登记一次即将开始的批量执行，返回任务 ID。
func (s *Store) CreateTask(command string, timeoutSec, total int) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO tasks(command, timeout_sec, status, total, created_at)
		VALUES(?,?,?,?,?)`,
		command, timeoutSec, TaskRunning, total, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AddTaskResult 写入一台主机的执行结果。
func (s *Store) AddTaskResult(taskID int64, r TaskResult) error {
	_, err := s.db.Exec(`
		INSERT INTO task_results(task_id, host_id, host_name, status, exit_code, error, note, duration_ms)
		VALUES(?,?,?,?,?,?,?,?)`,
		taskID, r.HostID, r.HostName, r.Status, r.ExitCode, r.Error, r.Note, r.DurationMS)
	return err
}

// FinishTask 给任务收尾。
func (s *Store) FinishTask(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE tasks SET status = ?, finished_at = ? WHERE id = ?`,
		status, time.Now().Unix(), id)
	return err
}

// CreateFileTask 登记一次文件操作（哈希 / 压缩 / 解压 / 上传 / 下载）。
//
// 与 CreateTask 的差别：这类操作只作用在一台主机上，没有"每台机器跑一条命令"
// 的概念。summary 是列表页直接展示的一句话（如"压缩 /var/log"），存进 command 列 ——
// 两处共用同一个字段，列表渲染才不必分两套。
//
// total 先记 0，收尾时由 FinishFileTask 写成明细条数：登记的时候还不知道要处理多少项。
func (s *Store) CreateFileTask(kind, hostName, summary, algo string) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO tasks(command, timeout_sec, kind, host_name, algo, status, total, created_at)
		VALUES(?,?,?,?,?,?,0,?)`,
		summary, 0, kind, hostName, algo, TaskRunning, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishFileTask 给文件类任务收尾：状态、明细条数与结束时间一次写完。
//
// 不复用 FinishTask：文件类任务的 total 在收尾前根本不存在（登记时还不知道要
// 处理多少项），多一次 UPDATE 只会让"条数为 0"的中间态被别处读到。
func (s *Store) FinishFileTask(id int64, status string, total int) error {
	_, err := s.db.Exec(`UPDATE tasks SET status = ?, total = ?, finished_at = ? WHERE id = ?`,
		status, total, time.Now().Unix(), id)
	return err
}

// AddTaskItems 批量写入文件任务的明细。
//
// 一次事务写完：压缩一个大目录会产生几万条明细，逐条提交时每条都要刷一次 WAL，
// 整体耗时可能超过打包本身。
func (s *Store) AddTaskItems(taskID int64, items []TaskItem) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`
		INSERT INTO task_items(task_id, path, size, hash, error) VALUES(?,?,?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()

	for _, item := range items {
		if _, err := stmt.Exec(taskID, item.Path, item.Size, item.Hash, item.Error); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// interruptRunningTasks 把上次进程退出时残留的 running 任务改写为 interrupted。
//
// finished_at 记成此刻而非保留 0：这个任务确实是在这里被终结的，
// 留 0 会让详情页把它显示成 1970 年。
func (s *Store) interruptRunningTasks() error {
	_, err := s.db.Exec(`UPDATE tasks SET status = ?, finished_at = ? WHERE status = ?`,
		TaskInterrupted, time.Now().Unix(), TaskRunning)
	return err
}

// ListTasks 按时间倒序返回最近的任务，并带上各状态的计数。
func (s *Store) ListTasks(limit int) ([]Task, error) {
	rows, err := s.db.Query(`
		SELECT t.id, t.command, t.timeout_sec, t.kind, t.host_name, t.algo,
		       t.status, t.total, t.created_at, t.finished_at,
		       COALESCE(SUM(CASE WHEN r.status = ? THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN r.status = ? THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN r.status = ? THEN 1 ELSE 0 END), 0)
		FROM tasks t
		LEFT JOIN task_results r ON r.task_id = t.id
		GROUP BY t.id
		ORDER BY t.id DESC
		LIMIT ?`,
		ResultSuccess, ResultFailed, ResultError, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tasks := make([]Task, 0)
	for rows.Next() {
		var t Task
		var created, finished int64
		if err := rows.Scan(&t.ID, &t.Command, &t.TimeoutSec, &t.Kind, &t.HostName, &t.Algo,
			&t.Status, &t.Total, &created, &finished,
			&t.SuccessCount, &t.FailedCount, &t.ErrorCount); err != nil {
			return nil, err
		}
		t.CreatedAt = time.Unix(created, 0)
		t.FinishedAt = time.Unix(finished, 0)
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// GetTask 返回任务详情：元数据加每台主机的结果与文件明细。
func (s *Store) GetTask(id int64) (*TaskDetail, error) {
	var t Task
	var created, finished int64
	err := s.db.QueryRow(`
		SELECT id, command, timeout_sec, kind, host_name, algo, status, total, created_at, finished_at
		FROM tasks WHERE id = ?`, id).
		Scan(&t.ID, &t.Command, &t.TimeoutSec, &t.Kind, &t.HostName, &t.Algo,
			&t.Status, &t.Total, &created, &finished)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	t.CreatedAt = time.Unix(created, 0)
	t.FinishedAt = time.Unix(finished, 0)

	rows, err := s.db.Query(`
		SELECT host_id, host_name, status, exit_code, error, note, duration_ms
		FROM task_results WHERE task_id = ? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	results := make([]TaskResult, 0)
	for rows.Next() {
		var r TaskResult
		if err := rows.Scan(&r.HostID, &r.HostName, &r.Status, &r.ExitCode,
			&r.Error, &r.Note, &r.DurationMS); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 详情页的计数从结果集现算，与列表页的 SQL 聚合口径保持一致。
	for _, r := range results {
		switch r.Status {
		case ResultSuccess:
			t.SuccessCount++
		case ResultFailed:
			t.FailedCount++
		case ResultError:
			t.ErrorCount++
		}
	}

	items, err := s.taskItems(id)
	if err != nil {
		return nil, err
	}

	return &TaskDetail{Task: t, Results: results, Items: items}, nil
}

// taskItems 读取一个任务的明细。批量执行没有明细，返回空切片而不是 nil。
func (s *Store) taskItems(taskID int64) ([]TaskItem, error) {
	rows, err := s.db.Query(`
		SELECT path, size, hash, error FROM task_items WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]TaskItem, 0)
	for rows.Next() {
		var item TaskItem
		if err := rows.Scan(&item.Path, &item.Size, &item.Hash, &item.Error); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ---------- 通知 ----------

// 通知来源。前端据此分类展示，并决定点击一条通知时该打开什么。
const (
	NotifyTask     = "task"
	NotifyHost     = "host"
	NotifySecurity = "security"
)

// 通知严重程度，只影响配色。
const (
	LevelInfo    = "info"
	LevelSuccess = "success"
	LevelWarning = "warning"
	LevelError   = "error"
)

// Notification 是一条通知。
type Notification struct {
	ID        int64     `json:"id"`
	Category  string    `json:"category"`
	Level     string    `json:"level"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Ref       int64     `json:"ref"`
	Read      bool      `json:"read"`
	CreatedAt time.Time `json:"createdAt"`
}

// NotificationInput 是写入通知时的字段集合。
type NotificationInput struct {
	Category string
	Level    string
	Title    string
	Body     string
	Ref      int64
}

// CreateNotification 写入一条通知，返回其 ID。
func (s *Store) CreateNotification(in NotificationInput) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO notifications(category, level, title, body, ref, created_at)
		VALUES(?,?,?,?,?,?)`,
		in.Category, in.Level, in.Title, in.Body, in.Ref, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListNotifications 按时间倒序返回最近的通知。
//
// 必须带上限：通知永久保留，表会一直增长，而面板一次只需要看最新的一屏。
// 全表拉取会让这个接口随运行时间越来越慢。
func (s *Store) ListNotifications(limit int) ([]Notification, error) {
	rows, err := s.db.Query(`
		SELECT id, category, level, title, body, ref, read, created_at
		FROM notifications ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]Notification, 0)
	for rows.Next() {
		var n Notification
		var read, created int64
		if err := rows.Scan(&n.ID, &n.Category, &n.Level, &n.Title, &n.Body,
			&n.Ref, &read, &created); err != nil {
			return nil, err
		}
		n.Read = read != 0
		n.CreatedAt = time.Unix(created, 0)
		items = append(items, n)
	}
	return items, rows.Err()
}

// CountUnreadNotifications 返回未读通知数，供铃铛角标轮询。
func (s *Store) CountUnreadNotifications() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM notifications WHERE read = 0`).Scan(&count)
	return count, err
}

// MarkNotificationsRead 把指定通知标为已读。
// ids 为空表示全部已读 —— 后者是"清空角标"这个动作的常见形态。
func (s *Store) MarkNotificationsRead(ids []int64) error {
	if len(ids) == 0 {
		_, err := s.db.Exec(`UPDATE notifications SET read = 1 WHERE read = 0`)
		return err
	}

	// 逐个执行而不是拼 IN (...)：ids 来自前端，拼字符串要么需要转义，
	// 要么要按数量动态生成占位符，都不如固定的预编译语句稳。
	// 单次标记的条数就是用户勾选的那几条，量级上完全没必要优化。
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(`UPDATE notifications SET read = 1 WHERE id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, id := range ids {
		if _, err := stmt.Exec(id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
