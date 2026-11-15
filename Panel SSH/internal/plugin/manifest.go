// Package plugin 定义插件清单（plugin.json）并提供安装包解压能力。
//
// 安全前提：org、name、entry 都会参与磁盘路径拼接，因此必须在这里严格约束，
// 不能等到写文件时再"尽力而为"地过滤。
package plugin

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// 能力（capability）是插件可申请的最小权限单元。
//
// 与 GMSSH 让插件进程直接拿到整套 SSH 能力不同，这里把能力拆细：
// 插件在清单里声明需要什么，管理员在安装时确认，运行时由能力桥按实际授予项收缩。
const (
	CapHostList      = "host:list"
	CapHostExec      = "host:exec"
	CapHostFileRead  = "host:file:read"
	CapHostFileWrite = "host:file:write"
	CapHostPkg       = "host:pkg"
	CapHostService   = "host:service"
	CapHostTunnel    = "host:tunnel"
)

// knownCapabilities 是全部已知能力。清单里出现未知能力直接拒绝，
// 避免插件声明一个我们根本没实现的权限却被记入授予列表。
var knownCapabilities = map[string]bool{
	CapHostList:      true,
	CapHostExec:      true,
	CapHostFileRead:  true,
	CapHostFileWrite: true,
	CapHostPkg:       true,
	CapHostService:   true,
	CapHostTunnel:    true,
}

// 能力的风险等级。
//
// 授权界面按它排序：读操作排在前面，能改变目标机的排在后面 ——
// 一个"什么都想要"的插件，它的高危请求会落在列表末尾，而不是混在中间被顺手勾上。
const (
	RiskLow    = "low"
	RiskMedium = "medium"
	RiskHigh   = "high"
)

// CapabilityRisk 给出每个能力的风险等级。
var CapabilityRisk = map[string]string{
	CapHostList:      RiskLow,
	CapHostFileRead:  RiskLow,
	CapHostFileWrite: RiskMedium,
	CapHostExec:      RiskMedium,
	CapHostService:   RiskMedium,
	CapHostPkg:       RiskHigh,
	CapHostTunnel:    RiskHigh,
}

// CapabilityOrder 给出能力的展示顺序：按风险从低到高，同级保持读、写、执行的自然顺序。
var CapabilityOrder = []string{
	CapHostList,
	CapHostFileRead,
	CapHostFileWrite,
	CapHostExec,
	CapHostService,
	CapHostPkg,
	CapHostTunnel,
}

// CapabilityLabel 是能力的界面文案。
//
// 文案说人话：写"在主机上安装软件包"而不是"可以提权执行任意命令" ——
// 后者没人敢授权，而它描述的其实是同一件事。
var CapabilityLabel = map[string]string{
	CapHostList:      "读取主机清单",
	CapHostExec:      "在主机上执行命令",
	CapHostFileRead:  "读取主机文件",
	CapHostFileWrite: "写入主机文件",
	CapHostPkg:       "在主机上安装软件包",
	CapHostService:   "启停主机服务",
	CapHostTunnel:    "建立到主机的隧道",
}

// IsKnownCapability 判断能力名是否受支持。
func IsKnownCapability(capability string) bool { return knownCapabilities[capability] }

// org/name 只允许小写字母、数字与连字符：这样它们天然不可能包含
// 路径分隔符或 ".."，路径穿越在校验阶段就被排除。
var identifierPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// 插件来源。
//
// archive 是用户上传的 zip 包，代码来自第三方，运行时必须待在沙箱里。
// embedded 是随面板分发的内置插件：它没有静态文件，入口是面板内部地址，
// 能力由面板自己实现，属于第一方代码，因此不套沙箱。
// 两者都要用户显式安装才会出现在桌面与插件列表里。
const (
	SourceArchive  = "archive"
	SourceEmbedded = "embedded"
)

// Builtin 描述一个随面板分发、由用户自行决定是否安装的内置插件。
type Builtin struct {
	Org         string
	Name        string
	DisplayName string
	Description string
	Version     string
	// URL 是面板内的入口地址（如 /dsh/），安装后由插件窗口直接加载。
	URL string
}

// ID 返回内置插件的唯一标识 {org}/{name}。
func (b Builtin) ID() string { return b.Org + "/" + b.Name }

// 内置 DSH 工作台插件的标识。各处需要判断"是不是它"时统一引用这里，
// 避免同一个字符串散落在插件目录、安装接口和 dsh 入口三处。
const (
	BuiltinDshWorkspaceOrg  = "panelssh"
	BuiltinDshWorkspaceName = "dsh-workspace"
	BuiltinDshWorkspaceID   = BuiltinDshWorkspaceOrg + "/" + BuiltinDshWorkspaceName
)

// Builtins 是随面板分发的内置插件目录。
var Builtins = []Builtin{
	{
		Org:         BuiltinDshWorkspaceOrg,
		Name:        BuiltinDshWorkspaceName,
		DisplayName: "DSH 工作台",
		Description: "DeepSeek Harness 的网页工作台。面板负责拉起本地 dsh 进程并把界面挂到 /dsh/ 下；需要你自行准备 dsh 本体。",
		Version:     "0.1.0",
		URL:         "/dsh/",
	},
}

// FindBuiltin 按 {org}/{name} 查找内置插件。
func FindBuiltin(org, name string) (Builtin, bool) {
	for _, item := range Builtins {
		if item.Org == org && item.Name == name {
			return item, true
		}
	}
	return Builtin{}, false
}

// 版本号限定为三段数字，避免出现被当作路径使用的怪值。
var versionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

const (
	// maxCapabilities 限制声明的能力数量。
	maxCapabilities = 16
	defaultEntry    = "index.html"
)

// 插件运行时类型。
//
// static 是缺省值：插件只是静态资源，行为与引入 runtime 段之前完全一致。
// process 表示插件带一个自己的后端进程，由面板负责拉起、代理与回收。
const (
	RuntimeStatic  = "static"
	RuntimeProcess = "process"
)

// ProtocolJSONRPC 是目前唯一支持的插件前后端通信协议。
const ProtocolJSONRPC = "jsonrpc-2.0"

const (
	defaultRuntimeSocket  = "app.sock"
	defaultRuntimeHealth  = "health"
	defaultStopTimeoutSec = 10
	// maxRuntimeArgs / maxRuntimeArgLen 限制启动参数，避免清单把命令行长到无法诊断。
	maxRuntimeArgs   = 32
	maxRuntimeArgLen = 256
	// maxIdleStopSec 是空闲回收的上限（24 小时），0 表示常驻不回收。
	maxIdleStopSec = 86400
	// 停止等待的上下限，单位秒。
	minStopTimeoutSec = 1
	maxStopTimeoutSec = 60
	maxRPCMethodLen   = 64
)

// JSON-RPC 方法名规则：小写字母开头的点分命名空间，例如 connection.list、sql.query。
//
// 方法名会参与转发请求的路径拼接，因此规则在这里定义一次：
// 清单里的 runtime.healthPath 与转发接口共用同一套校验，避免两处漂移。
var rpcMethodPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)*$`)

// IsValidRPCMethod 判断一个 JSON-RPC 方法名是否合法。
func IsValidRPCMethod(method string) bool {
	return len(method) <= maxRPCMethodLen && rpcMethodPattern.MatchString(method)
}

// RuntimeSpec 是清单里可选的 runtime 段，描述插件的后端进程。
//
// 清单里没有这一段（或 kind 为 static）即为纯静态插件，面板不会为它拉任何进程。
type RuntimeSpec struct {
	// Kind 取 static（缺省）或 process。
	Kind string `json:"kind,omitempty"`
	// Exec 是相对安装包根目录的可执行文件路径，例如 bin/mysql-manager。
	// 面板直接 exec 它而不经 shell，参数边界由内核保证。
	Exec string `json:"exec,omitempty"`
	// Args 是启动参数，支持 {socket}、{dataDir}、{hostDir} 三个占位符。
	Args []string `json:"args,omitempty"`
	// Socket 是 Unix Socket 的相对路径。
	//
	// 它相对插件的运行时数据目录解析，而不是安装包目录：socket 是运行时产物，
	// 必须与插件实例同生命周期，不能随版本目录一起被替换或清理。
	Socket string `json:"socket,omitempty"`
	// Protocol 目前只支持 jsonrpc-2.0。
	Protocol string `json:"protocol,omitempty"`
	// AutoStart 为 nil 表示缺省，即安装完成后自动拉起。
	// 用指针是为了区分"没写"和"显式写了 false"。
	AutoStart *bool `json:"autoStart,omitempty"`
	// IdleStopSec 是空闲多久后回收进程，0 表示常驻。
	IdleStopSec int `json:"idleStopSec,omitempty"`
	// HealthPath 是就绪探测调用的 JSON-RPC 方法名，默认 health。
	HealthPath string `json:"healthPath,omitempty"`
	// StopTimeoutSec 是 SIGTERM 后等待退出的秒数，超时转 SIGKILL。
	StopTimeoutSec int `json:"stopTimeoutSec,omitempty"`
}

// AutoStartOn 返回是否需要在安装完成后自动拉起进程。
//
// 对 nil 接收者返回 true，这样调用方不必先判 Runtime == nil：
// "没有运行时就不需要启动"与"缺省自动启动"在这里是同一个答案，
// 但要由是否真的拉进程的那段代码去判断。
func (r *RuntimeSpec) AutoStartOn() bool {
	return r == nil || r.AutoStart == nil || *r.AutoStart
}

// Manifest 对应安装包根目录下的 plugin.json。
type Manifest struct {
	Org          string   `json:"org"`
	Name         string   `json:"name"`
	DisplayName  string   `json:"displayName"`
	Description  string   `json:"description"`
	Version      string   `json:"version"`
	Icon         string   `json:"icon,omitempty"`
	Entry        string   `json:"entry,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`

	// Runtime 是可选的后端进程声明，缺省表示纯静态插件。
	Runtime *RuntimeSpec `json:"runtime,omitempty"`
}

// ID 返回插件的唯一标识 {org}/{name}。
func (m *Manifest) ID() string { return m.Org + "/" + m.Name }

// Dir 返回该插件在插件根目录下的相对路径。
func (m *Manifest) Dir() string { return path.Join(m.Org, m.Name, m.Version) }

// Normalize 补齐默认值并校验，返回校验后的清单。
func (m *Manifest) Normalize() (*Manifest, error) {
	m.Org = strings.TrimSpace(m.Org)
	m.Name = strings.TrimSpace(m.Name)
	m.DisplayName = strings.TrimSpace(m.DisplayName)
	m.Description = strings.TrimSpace(m.Description)
	m.Version = strings.TrimSpace(m.Version)

	if !identifierPattern.MatchString(m.Org) {
		return nil, errors.New("org 只能由小写字母、数字与连字符组成，且不超过 32 个字符")
	}
	if !identifierPattern.MatchString(m.Name) {
		return nil, errors.New("name 只能由小写字母、数字与连字符组成，且不超过 32 个字符")
	}
	if !versionPattern.MatchString(m.Version) {
		return nil, errors.New("version 必须形如 1.0.0")
	}
	if m.DisplayName == "" {
		m.DisplayName = m.Name
	}
	if len(m.DisplayName) > 64 {
		return nil, errors.New("displayName 不能超过 64 个字符")
	}
	if len(m.Description) > 500 {
		return nil, errors.New("description 不能超过 500 个字符")
	}

	entry, err := normalizeRelativePath(m.Entry, defaultEntry)
	if err != nil {
		return nil, fmt.Errorf("entry 非法：%w", err)
	}
	if !strings.HasSuffix(strings.ToLower(entry), ".html") {
		return nil, errors.New("entry 必须是 .html 文件")
	}
	m.Entry = entry

	if m.Icon != "" {
		icon, err := normalizeRelativePath(m.Icon, "")
		if err != nil {
			return nil, fmt.Errorf("icon 非法：%w", err)
		}
		m.Icon = icon
	}

	if len(m.Capabilities) > maxCapabilities {
		return nil, fmt.Errorf("声明的能力不能超过 %d 项", maxCapabilities)
	}

	seen := make(map[string]bool, len(m.Capabilities))
	normalized := make([]string, 0, len(m.Capabilities))
	for _, capability := range m.Capabilities {
		capability = strings.TrimSpace(capability)
		if capability == "" || seen[capability] {
			continue
		}
		if !knownCapabilities[capability] {
			return nil, fmt.Errorf("声明了未知能力：%s", capability)
		}
		seen[capability] = true
		normalized = append(normalized, capability)
	}
	m.Capabilities = normalized

	rt, err := normalizeRuntime(m.Runtime)
	if err != nil {
		return nil, err
	}
	m.Runtime = rt

	return m, nil
}

// normalizeRuntime 校验并补齐 runtime 段；清单里没有这一段时返回 nil。
//
// 这里只做纯校验，不碰磁盘：exec 是否真的存在、是否带可执行位，
// 要等落位到具体版本目录之后才能判断，属于启动前的检查。
func normalizeRuntime(spec *RuntimeSpec) (*RuntimeSpec, error) {
	if spec == nil {
		return nil, nil
	}

	kind := strings.TrimSpace(spec.Kind)
	if kind == "" {
		kind = RuntimeStatic
	}
	switch kind {
	case RuntimeStatic:
		// kind=static 时其余字段一律忽略：它们不会生效，
		// 为一份不生效的配置报错只会让人困惑。
		return nil, nil
	case RuntimeProcess:
	default:
		return nil, fmt.Errorf("runtime.kind 只能是 %s 或 %s", RuntimeStatic, RuntimeProcess)
	}

	exec, err := normalizeRelativePath(spec.Exec, "")
	if err != nil {
		return nil, fmt.Errorf("runtime.exec 非法：%w", err)
	}
	if exec == "" {
		return nil, errors.New("runtime.exec 在 kind=process 时必填")
	}

	if len(spec.Args) > maxRuntimeArgs {
		return nil, fmt.Errorf("runtime.args 不能超过 %d 项", maxRuntimeArgs)
	}
	args := make([]string, 0, len(spec.Args))
	for _, arg := range spec.Args {
		if len(arg) > maxRuntimeArgLen {
			return nil, fmt.Errorf("runtime.args 单项不能超过 %d 个字符", maxRuntimeArgLen)
		}
		args = append(args, arg)
	}

	socket, err := normalizeRelativePath(spec.Socket, defaultRuntimeSocket)
	if err != nil {
		return nil, fmt.Errorf("runtime.socket 非法：%w", err)
	}

	protocol := strings.TrimSpace(spec.Protocol)
	if protocol == "" {
		protocol = ProtocolJSONRPC
	}
	if protocol != ProtocolJSONRPC {
		return nil, fmt.Errorf("runtime.protocol 目前只支持 %s", ProtocolJSONRPC)
	}

	if spec.IdleStopSec < 0 || spec.IdleStopSec > maxIdleStopSec {
		return nil, fmt.Errorf("runtime.idleStopSec 必须在 0 到 %d 秒之间（0 表示常驻）", maxIdleStopSec)
	}

	health := strings.TrimSpace(spec.HealthPath)
	if health == "" {
		health = defaultRuntimeHealth
	}
	if !IsValidRPCMethod(health) {
		return nil, fmt.Errorf("runtime.healthPath 不是合法的 JSON-RPC 方法名：%s", health)
	}

	stopTimeout := spec.StopTimeoutSec
	if stopTimeout == 0 {
		stopTimeout = defaultStopTimeoutSec
	}
	if stopTimeout < minStopTimeoutSec || stopTimeout > maxStopTimeoutSec {
		return nil, fmt.Errorf("runtime.stopTimeoutSec 必须在 %d 到 %d 秒之间", minStopTimeoutSec, maxStopTimeoutSec)
	}

	autoStart := true
	if spec.AutoStart != nil {
		autoStart = *spec.AutoStart
	}

	return &RuntimeSpec{
		Kind:           RuntimeProcess,
		Exec:           exec,
		Args:           args,
		Socket:         socket,
		Protocol:       protocol,
		AutoStart:      &autoStart,
		IdleStopSec:    spec.IdleStopSec,
		HealthPath:     health,
		StopTimeoutSec: stopTimeout,
	}, nil
}

// normalizeRelativePath 校验一个包内相对路径。
// fallback 在入参为空时启用；传空串表示该字段可以不填。
func normalizeRelativePath(raw, fallback string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if fallback == "" {
			return "", nil
		}
		return fallback, nil
	}
	if path.IsAbs(raw) || strings.Contains(raw, "\\") {
		return "", errors.New("必须是包内的相对路径")
	}

	clean := path.Clean(raw)
	// Clean 之后仍以 .. 开头，说明它指向包外。
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("不能指向安装包之外的路径")
	}
	return clean, nil
}
