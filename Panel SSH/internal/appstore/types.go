// Package appstore 实现"从 GitHub 拉取应用包"的客户端与本地缓存。
//
// 应用包是一组声明式资源：app.json（元数据、动作、配置路径）、ui.json
// （通用窗口的布局）、icon.svg 与可选的 config 模板。动作只允许是
// 带占位符的命令模板，参数由面板校验后做 shell 引号替换；这样远程包
// 不会把任意代码直接拼进命令，执行面仍然只有包内声明的那几条动作。
//
// 安全边界：
//   - 索引与包文件都从固定 HTTPS 源拉取；
//   - 索引里带每个文件的 SHA-256，落盘前逐文件校验；
//   - 安装需要管理员显式确认（接口层负责），更新同理；
//   - 包体上限、文件名白名单、路径穿越在这里统一拒绝。
package appstore

// SchemaVersion 是当前应用包格式版本。格式不兼容时直接拒绝，
// 而不是"尽力解析出一半"，避免界面展示与执行面不一致。
const SchemaVersion = 1

// FileRef 是索引里的一个包内文件与它的内容摘要。
type FileRef struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Entry 是索引里的一个应用条目。
type Entry struct {
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Category    string    `json:"category"`
	Description string    `json:"description"`
	Icon        string    `json:"icon"`
	Manifest    string    `json:"manifest"`
	UI          string    `json:"ui"`
	Files       []FileRef `json:"files"`
	// UIPreview 是随索引一起下发的界面结构预览。
	//
	// ui.json 里的动作只有 id，详情页要在安装之前就把窗口结构画出来，
	// 为此在索引里多带一份解析好的结构与动作名称，省掉逐包下载。
	UIPreview *UIPreview `json:"uiPreview,omitempty"`
}

// UIPreviewAction 是界面预览里的一个动作：ID 用于对齐运行时，Label 用于展示。
type UIPreviewAction struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// UIPreviewSection 是界面预览里的一个动作分组。
type UIPreviewSection struct {
	Title   string            `json:"title"`
	Actions []UIPreviewAction `json:"actions"`
}

// UIPreview 是 ui.json 的可展示版本。
type UIPreview struct {
	Layout   string             `json:"layout"`
	Overview []UIPreviewAction  `json:"overview,omitempty"`
	Sections []UIPreviewSection `json:"sections,omitempty"`
	Hint     string             `json:"hint,omitempty"`
}

// Index 是 apps/index.json 的结构。
type Index struct {
	SchemaVersion int     `json:"schemaVersion"`
	GeneratedAt   string  `json:"generatedAt"`
	Apps          []Entry `json:"apps"`
}

// Field 是动作表单字段。
type Field struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Type        string   `json:"type"` // text / number / select / password / textarea
	Placeholder string   `json:"placeholder,omitempty"`
	Default     string   `json:"default,omitempty"`
	Options     []string `json:"options,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Help        string   `json:"help,omitempty"`
	// Pattern 是可选的正则校验（Go RE2 语法）。留空表示只做长度校验。
	Pattern string `json:"pattern,omitempty"`
	MaxLen  int    `json:"maxLen,omitempty"`
}

// Action 是应用包声明的一个动作。
//
// Builtin 为真表示这个动作由面板编译期实现（GnuPG 密钥生成这类需要
// 临时文件与多步编排的动作），包只负责元数据与界面；否则 Command
// 是带 {{field}} 占位符的命令模板。
type Action struct {
	ID          string  `json:"id"`
	Label       string  `json:"label"`
	Description string  `json:"description,omitempty"`
	Group       string  `json:"group,omitempty"` // inspect / manage
	Fields      []Field `json:"fields,omitempty"`
	Confirm     string  `json:"confirm,omitempty"`
	Danger      bool    `json:"danger,omitempty"`
	NeedRoot    bool    `json:"needRoot,omitempty"`
	TimeoutSec  int     `json:"timeoutSec,omitempty"`
	Command     string  `json:"command,omitempty"`
	Builtin     bool    `json:"builtin,omitempty"`
}

// ConfigPath 是应用声明的一份可编辑配置。
type ConfigPath struct {
	Path  string `json:"path"`
	IsDir bool   `json:"isDir,omitempty"`
}

// Manifest 是 app.json 的结构。
type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	Slug          string       `json:"slug"`
	Name          string       `json:"name"`
	Version       string       `json:"version"`
	Category      string       `json:"category"`
	Description   string       `json:"description"`
	UseCase       string       `json:"useCase,omitempty"`
	Note          string       `json:"note,omitempty"`
	Icon          string       `json:"icon"`
	DefaultAction string       `json:"defaultAction,omitempty"`
	Config        []ConfigPath `json:"config,omitempty"`
	Actions       []Action     `json:"actions"`
}

// Section 是 ui.json 里的一个动作分组。
type Section struct {
	Title   string   `json:"title"`
	Actions []string `json:"actions"`
}

// UI 是 ui.json 的结构。
//
// 目前通用壳只消费 Sections 与 Hint；Overview 预留给后续"打开即跑"的
// 多步概览，不影响现有行为。
type UI struct {
	SchemaVersion int       `json:"schemaVersion"`
	Layout        string    `json:"layout"`
	Overview      []string  `json:"overview,omitempty"`
	Sections      []Section `json:"sections,omitempty"`
	Hint          string    `json:"hint,omitempty"`
}
