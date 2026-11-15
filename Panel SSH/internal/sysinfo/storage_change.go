package sysinfo

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// 支持的存储操作类型。
//
// 前四个只影响"谁挂在哪里"，后五个会动到盘上的数据 —— 后者统一走
// storage_disk.go 的破坏性操作门槛，不在这里做特殊处理。
const (
	// StorageMountLocal 挂载宿主机上的块设备。
	StorageMountLocal = "mount-local"
	// StorageMountNFS 挂载 NFS 导出。
	StorageMountNFS = "mount-nfs"
	// StorageMountCIFS 挂载 SMB/CIFS 共享。
	StorageMountCIFS = "mount-cifs"
	// StorageUnmount 卸载一个挂载点。
	StorageUnmount = "unmount"
	// StorageFormat 把设备格式化成指定的文件系统（清空设备上的数据）。
	StorageFormat = "format"
	// StoragePartLabel 创建分区表（清空整盘的分区信息）。
	StoragePartLabel = "part-label"
	// StoragePartCreate 在磁盘上新建分区。
	StoragePartCreate = "part-create"
	// StoragePartDelete 删除分区。
	StoragePartDelete = "part-delete"
	// StorageFstabRemove 只删掉一条 fstab 条目，不挂载也不卸载。
	StorageFstabRemove = "fstab-remove"
)

// StorageRequest 是一次存储操作的请求参数。
//
// 与网络变更同样是"不可信输入"：所有字段最终都会进入远端命令，
// 校验是这个函数的第一职责。
type StorageRequest struct {
	Action string `json:"action"`
	// Device 是块设备路径（mount-local 用）。
	Device string `json:"device"`
	// Target 是挂载点目录。
	Target string `json:"target"`
	// FSType 留空表示让 mount 自行探测。
	FSType string `json:"fstype"`
	// Options 是用户附加的挂载选项，例如 ro、noatime。
	Options []string `json:"options"`
	// Server 与 Share 用于网络存储：NFS 是「服务器:导出路径」，
	// CIFS 是「服务器 / 共享名」。
	Server   string `json:"server"`
	Share    string `json:"share"`
	Username string `json:"username"`
	Password string `json:"password"`
	// Persist 为真时把条目写入 /etc/fstab，使其重启后仍生效。
	Persist bool `json:"persist"`
	// Label 是格式化时写入的卷标，可留空。
	Label string `json:"label"`
	// Confirm 是破坏性操作的二次确认，必须与 Device 完全一致。
	// 这不是仪式感：磁盘工具最典型的事故就是"在错误的设备上按下了确认"，
	// 而服务端必须自己独立校验它，不能指望前端替它把关。
	Confirm string `json:"confirm"`
	// PartTable 要创建的分区表类型：gpt 或 msdos。
	PartTable string `json:"partTable"`
	// PartStart / PartEnd 是新建分区的起止位置。收下 parted 的两种写法：
	// 百分比（0%、100%）与带单位的偏移（512MiB、10GiB）。
	PartStart string `json:"partStart"`
	PartEnd   string `json:"partEnd"`
	// PartNumber 是要删除的分区号。
	PartNumber int `json:"partNumber"`
}

// StoragePlan 是一次存储操作的执行计划。
//
// 分成四段而不是一串命令，是为了让调用方能在每一步之后决定是否收手：
// 挂载失败要清理凭据，写 fstab 失败还要额外卸载 —— 混在一条命令里
// 只能靠 `&&` 串联，一旦中间某步失败，前面的动作就没人善后了。
type StoragePlan struct {
	Summary string `json:"summary"`
	// Prepare 是挂载前的准备：建立挂载点目录、写入凭据文件。
	Prepare []string `json:"prepare"`
	// Mount 是真正让文件系统挂上的命令。
	Mount string `json:"mount"`
	// Persist 是写入 /etc/fstab 的命令（不持久化时为空）。
	Persist []string `json:"persist"`
	// Finish 是挂载成功后的收尾，例如删掉一次性的凭据文件。
	Finish []string `json:"finish"`
	// Cleanup 是失败时按顺序执行的清理：卸载、删凭据、还原 fstab。
	Cleanup []string `json:"cleanup"`
	// CredentialPath 非空表示这次操作会在目标机留下一个凭据文件。
	CredentialPath string `json:"credentialPath"`
	// Expect 描述操作成功后应看到的状态，供服务端复核。
	Expect *StorageExpect `json:"-"`
}

// StorageExpect 是"操作成功后应该看到什么"。
type StorageExpect struct {
	Action string
	Target string
	// WantMounted 为真表示期望该挂载点存在，为假表示期望它消失。
	WantMounted bool
	// WantPersistent 表示期望它出现在 fstab 里。
	WantPersistent bool
	// WantFSType 非空时，Target 是设备路径，期望它的文件系统变成这个类型。
	WantFSType string
	// WantPartTable 非空时，Target 是整盘路径，期望它的分区表变成这个类型。
	WantPartTable string
	// WantPartDelta 非零时，Target 是整盘路径，期望它上面的分区数量变化这么多
	// （新建 +1、删除 -1）。用增量而不是具体编号，是因为新分区的编号由 parted 决定。
	WantPartDelta int
	// PartNumber 与 WantPartDelta 配合，确认这个编号的分区确实已经不在了。
	PartNumber int
}

// 名字类字段的白名单。
var (
	// 挂载点：绝对路径，允许中文与常见符号，但不允许空格（fstab 用空格分隔字段）
	// 与任何 shell 元字符。
	mountPointPattern = regexp.MustCompile(`^/[\p{L}\p{N}._@+-]*(/[\p{L}\p{N}._@+-]+)*$`)
	// 服务器地址：主机名、IPv4、IPv6。
	serverPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,255}$`)
	// NFS 导出路径。
	exportPattern = regexp.MustCompile(`^/[\p{L}\p{N}._@+-]*(/[\p{L}\p{N}._@+-]+)*$`)
	// SMB 共享名。
	sharePattern = regexp.MustCompile(`^[\p{L}\p{N}._@+$-]{1,64}$`)
	// 挂载选项：形如 ro / noatime / uid=1000 / iocharset=utf8。
	mountOptionPattern = regexp.MustCompile(`^[A-Za-z0-9._=,:+-]{1,64}$`)
	// 文件系统类型。
	fstypePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)
)

// 挂载点不能落在这些位置。它们要么是系统的命脉，要么本身就是一个挂载挂接点。
var forbiddenMountTargets = map[string]bool{
	"/":     true,
	"/boot": true,
	"/usr":  true,
	"/etc":  true,
	"/var":  true,
	"/bin":  true,
	"/sbin": true,
	"/lib":  true,
	"/opt":  true,
	"/root": true,
	"/dev":  true,
	"/proc": true,
	"/sys":  true,
	"/run":  true,
	"/tmp":  true,
}

// PlanStorageChange 校验请求并生成执行计划。
func PlanStorageChange(req StorageRequest, snapshot *StorageSnapshot) (*StoragePlan, error) {
	if snapshot == nil {
		return nil, errors.New("没有该主机的存储快照，无法校验操作目标")
	}

	switch req.Action {
	case StorageMountLocal:
		return planMountLocal(req, snapshot)
	case StorageMountNFS:
		return planMountNFS(req, snapshot)
	case StorageMountCIFS:
		return planMountCIFS(req, snapshot)
	case StorageUnmount:
		return planUnmount(req, snapshot)
	case StorageFormat:
		return planFormat(req, snapshot)
	case StoragePartLabel:
		return planPartLabel(req, snapshot)
	case StoragePartCreate:
		return planPartCreate(req, snapshot)
	case StoragePartDelete:
		return planPartDelete(req, snapshot)
	case StorageFstabRemove:
		return planFstabRemove(req, snapshot)
	default:
		return nil, fmt.Errorf("不支持的存储操作：%s", req.Action)
	}
}

// ---------- 本地设备挂载 ----------

func planMountLocal(req StorageRequest, snapshot *StorageSnapshot) (*StoragePlan, error) {
	target, err := validateMountTarget(req.Target, snapshot)
	if err != nil {
		return nil, err
	}
	if err := ensureNotMounted(target, snapshot); err != nil {
		return nil, err
	}

	device := findDevice(snapshot.Devices, req.Device)
	if device == nil {
		return nil, fmt.Errorf("主机上不存在设备 %s", req.Device)
	}
	if device.System {
		return nil, fmt.Errorf("%s 承载着操作系统（%s），不允许在这里操作", device.Path, strings.Join(device.MountPoints, "、"))
	}
	if device.Busy {
		return nil, fmt.Errorf("%s 已经挂载在 %s，请先卸载", device.Path, strings.Join(device.MountPoints, "、"))
	}
	if device.FSType == "" {
		return nil, fmt.Errorf("%s 上还没有文件系统，请先在「分区与格式化」里创建", device.Path)
	}
	if device.ReadOnly {
		return nil, fmt.Errorf("%s 处于只读状态，无法挂载", device.Path)
	}

	fstype := strings.TrimSpace(req.FSType)
	if fstype == "" {
		fstype = device.FSType
	}
	if !fstypePattern.MatchString(fstype) {
		return nil, fmt.Errorf("文件系统类型 %q 不合法", fstype)
	}

	options, err := validateOptions(req.Options)
	if err != nil {
		return nil, err
	}

	mountArgs := mountArgsString(options)
	mountCommand := fmt.Sprintf("mount -t %s -o %s -- %s %s",
		shellQuote(fstype), shellQuote(mountArgs), shellQuote(device.Path), shellQuote(target))

	plan := &StoragePlan{
		Summary: fmt.Sprintf("把 %s 挂载到 %s", device.Path, target),
		Prepare: []string{fmt.Sprintf("mkdir -p -- %s", shellQuote(target))},
		Mount:   mountCommand,
		Cleanup: []string{
			fmt.Sprintf("umount -- %s 2>/dev/null || true", shellQuote(target)),
			fmt.Sprintf("rmdir -- %s 2>/dev/null || true", shellQuote(target)),
		},
		Expect: &StorageExpect{Action: StorageMountLocal, Target: target, WantMounted: true},
	}

	if req.Persist {
		// 用户没填选项时补上 defaults：fstab 里只写 nofail 虽然功能等价，
		// 但和机器上其它条目风格不一致，日后维护时容易让人以为漏了什么。
		fstabOptions := options
		if len(fstabOptions) == 0 {
			fstabOptions = []string{"defaults"}
		}
		entry := buildFstabEntry(deviceFstabSource(device), target, fstype,
			append(append([]string{}, fstabOptions...), "nofail"), 0, 2)
		plan.Persist = fstabAppendCommands(entry)
		plan.Cleanup = append(plan.Cleanup, fmt.Sprintf("umount -- %s 2>/dev/null || true", shellQuote(target)))
		plan.Expect.WantPersistent = true
		plan.Summary += "（并写入 fstab，重启后自动挂载）"
	}

	return plan, nil
}

// deviceFstabSource 给出 fstab 里引用设备的写法。
//
// 优先用 UUID：设备名会随插拔顺序变化（今天 sdb 明天 sdc），
// 按 /dev/sdb 写进 fstab 的条目迟早会挂到错误的盘上。
func deviceFstabSource(device *BlockDevice) string {
	if device.UUID != "" {
		return "UUID=" + device.UUID
	}
	if device.Label != "" {
		// 标签里可能有空格，fstab 用 \040 转义。
		return "LABEL=" + strings.ReplaceAll(device.Label, " ", "\\040")
	}
	return device.Path
}

// ---------- 网络存储 ----------

func planMountNFS(req StorageRequest, snapshot *StorageSnapshot) (*StoragePlan, error) {
	if !hasTool(snapshot, "nfs") {
		return nil, errors.New("目标主机上没有 mount.nfs（安装 nfs-common / nfs-utils 后可用）")
	}
	target, err := validateMountTarget(req.Target, snapshot)
	if err != nil {
		return nil, err
	}
	if err := ensureNotMounted(target, snapshot); err != nil {
		return nil, err
	}

	server, err := validateServer(req.Server)
	if err != nil {
		return nil, err
	}
	if strings.Contains(server, ":") && !strings.Contains(server, "[") {
		// IPv6 字面量在 NFS 的 server:/path 写法里必须加方括号，否则冒号会被
		// 当成分隔符。这里直接说清楚，而不是让 mount 抛一句难懂的错。
		return nil, errors.New("IPv6 地址请用 [地址] 形式，例如 [fd00::1]:/export")
	}
	export := strings.TrimSpace(req.Share)
	if !exportPattern.MatchString(export) {
		return nil, fmt.Errorf("NFS 导出路径 %q 不合法（应以 / 开头的绝对路径）", export)
	}

	options, err := validateOptions(req.Options)
	if err != nil {
		return nil, err
	}
	if len(options) == 0 {
		options = []string{"vers=4.1"}
	}

	source := server + ":" + export
	mountCommand := fmt.Sprintf("mount -t nfs -o %s -- %s %s",
		shellQuote(mountArgsString(options)), shellQuote(source), shellQuote(target))

	plan := &StoragePlan{
		Summary: fmt.Sprintf("挂载 NFS %s 到 %s", source, target),
		Prepare: []string{fmt.Sprintf("mkdir -p -- %s", shellQuote(target))},
		Mount:   mountCommand,
		Cleanup: []string{
			fmt.Sprintf("umount -- %s 2>/dev/null || true", shellQuote(target)),
			fmt.Sprintf("rmdir -- %s 2>/dev/null || true", shellQuote(target)),
		},
		Expect: &StorageExpect{Action: StorageMountNFS, Target: target, WantMounted: true},
	}

	if req.Persist {
		// _netdev 让 systemd 等到网络就绪后再挂；nofail 保证网络存储不可达时
		// 机器仍能正常启动 —— 少了这两个选项，一次交换机故障就会让所有
		// 挂载了 NFS 的机器卡在开机过程中。
		entry := buildFstabEntry(source, target, "nfs",
			append(append([]string{}, options...), "nofail", "_netdev"), 0, 0)
		plan.Persist = fstabAppendCommands(entry)
		plan.Cleanup = append(plan.Cleanup, fmt.Sprintf("umount -- %s 2>/dev/null || true", shellQuote(target)))
		plan.Expect.WantPersistent = true
		plan.Summary += "（并写入 fstab）"
	}

	return plan, nil
}

func planMountCIFS(req StorageRequest, snapshot *StorageSnapshot) (*StoragePlan, error) {
	if !hasTool(snapshot, "cifs") {
		return nil, errors.New("目标主机上没有 mount.cifs（安装 cifs-utils 后可用）")
	}
	target, err := validateMountTarget(req.Target, snapshot)
	if err != nil {
		return nil, err
	}
	if err := ensureNotMounted(target, snapshot); err != nil {
		return nil, err
	}

	server, err := validateServer(req.Server)
	if err != nil {
		return nil, err
	}
	share := strings.TrimSpace(req.Share)
	if !sharePattern.MatchString(share) {
		return nil, fmt.Errorf("共享名 %q 不合法", share)
	}

	username, err := validateCredentialField(req.Username, "用户名")
	if err != nil {
		return nil, err
	}
	password, err := validateCredentialField(req.Password, "密码")
	if err != nil {
		return nil, err
	}

	options, err := validateOptions(req.Options)
	if err != nil {
		return nil, err
	}
	if len(options) == 0 {
		options = []string{"vers=3.0"}
	}

	source := "//" + server + "/" + share
	credentialPath := cifsCredentialPath(server, share)

	plan := &StoragePlan{
		Summary:        fmt.Sprintf("挂载 SMB %s 到 %s", source, target),
		CredentialPath: credentialPath,
		Prepare: []string{
			fmt.Sprintf("mkdir -p -- %s", shellQuote(target)),
			writeCredentialCommand(credentialPath, username, password),
		},
		Mount: fmt.Sprintf("mount -t cifs -o %s -- %s %s",
			shellQuote(mountArgsString(append([]string{"credentials=" + credentialPath}, options...))),
			shellQuote(source), shellQuote(target)),
		Cleanup: []string{
			fmt.Sprintf("umount -- %s 2>/dev/null || true", shellQuote(target)),
			fmt.Sprintf("rm -f -- %s", shellQuote(credentialPath)),
			fmt.Sprintf("rmdir -- %s 2>/dev/null || true", shellQuote(target)),
		},
		Expect: &StorageExpect{Action: StorageMountCIFS, Target: target, WantMounted: true},
	}

	if req.Persist {
		// 凭据文件被 fstab 条目引用，必须留在机器上，因此从失败清理里摘掉它。
		plan.Cleanup = []string{
			fmt.Sprintf("umount -- %s 2>/dev/null || true", shellQuote(target)),
			fmt.Sprintf("rmdir -- %s 2>/dev/null || true", shellQuote(target)),
		}
		entry := buildFstabEntry(source, target, "cifs",
			append(append([]string{"credentials=" + credentialPath}, options...), "nofail", "_netdev"), 0, 0)
		plan.Persist = fstabAppendCommands(entry)
		plan.Cleanup = append(plan.Cleanup, fmt.Sprintf("umount -- %s 2>/dev/null || true", shellQuote(target)))
		plan.Expect.WantPersistent = true
		plan.Summary += "（并写入 fstab）"
	} else {
		// 不写 fstab 时这个文件只是一次性的：挂载成功后内核已持有凭据，
		// 把密码继续留在目标机上没有任何好处。
		plan.Finish = []string{fmt.Sprintf("rm -f -- %s", shellQuote(credentialPath))}
	}

	return plan, nil
}

// cifsCredentialPath 给出凭据文件路径。
//
// 只保留服务器与共享名里的安全字符：这个路径会进入 fstab 与命令行，
// 拼接前必须确保它不可能带出任何额外语义。
func cifsCredentialPath(server, share string) string {
	sanitize := func(value string) string {
		var sb strings.Builder
		for _, r := range value {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
				sb.WriteRune(r)
			case r == '.' || r == '-' || r == '_':
				sb.WriteRune(r)
			default:
				sb.WriteRune('_')
			}
		}
		return sb.String()
	}
	return "/etc/panelssh-cifs-" + sanitize(server) + "-" + sanitize(share) + ".cred"
}

// writeCredentialCommand 写入 SMB 凭据文件。
//
// 用 heredoc 而不是 `echo` 或命令行参数：把密码写进命令行参数的话，
// 目标机上任何一个用户都能在 `ps` 的瞬间看到它，也会留在 shell 历史里。
func writeCredentialCommand(path, username, password string) string {
	var sb strings.Builder
	sb.WriteString("(umask 077 && cat > " + shellQuote(path) + " <<'PANELSSH_CIFS_CRED_EOF'\n")
	sb.WriteString("username=" + username + "\n")
	sb.WriteString("password=" + password + "\n")
	sb.WriteString("PANELSSH_CIFS_CRED_EOF\n)\n")
	return sb.String()
}

// ---------- 卸载 ----------

func planUnmount(req StorageRequest, snapshot *StorageSnapshot) (*StoragePlan, error) {
	target, err := validateMountTarget(req.Target, snapshot)
	if err != nil {
		return nil, err
	}

	var mounted *Mount
	for i := range snapshot.Mounts {
		if snapshot.Mounts[i].Target == target {
			mounted = &snapshot.Mounts[i]
			break
		}
	}
	if mounted == nil {
		return nil, fmt.Errorf("%s 当前没有被挂载", target)
	}
	if mounted.Virtual {
		return nil, fmt.Errorf("%s 是系统提供的虚拟文件系统，不能卸载", target)
	}

	plan := &StoragePlan{
		Summary: fmt.Sprintf("卸载 %s（%s）", target, mounted.Source),
		Mount:   fmt.Sprintf("umount -- %s", shellQuote(target)),
		Expect:  &StorageExpect{Action: StorageUnmount, Target: target, WantMounted: false},
	}

	if mounted.Persistent {
		// 只卸载不删 fstab 条目，用户下次重启会在同一个目录上看到它又冒出来 ——
		// 那是个很难理解的诡异现象，所以这里必须一并处理。
		plan.Persist = fstabRemoveCommands(target)
		plan.Expect.WantPersistent = true
		plan.Summary += "（同时移除 fstab 条目）"
	}

	return plan, nil
}

// ---------- fstab 编辑 ----------

// fstabBackupPath 是 /etc/fstab 的备份文件。固定名字、每次覆盖：
// 面板只保证"改错了能退回上一步"，不做版本历史，避免备份文件无限堆积。
const fstabBackupPath = "/etc/fstab.panelssh.bak"

func buildFstabEntry(source, target, fstype string, options []string, dump, pass int) string {
	return fmt.Sprintf("%s %s %s %s %d %d",
		source, target, fstype, strings.Join(options, ","), dump, pass)
}

// fstabAppendCommands 生成"备份 → 追加 → 校验 → 失败则还原"的命令序列。
func fstabAppendCommands(entry string) []string {
	return []string{
		fmt.Sprintf("cp -a /etc/fstab %s", shellQuote(fstabBackupPath)),
		fmt.Sprintf("printf '\\n# added by PanelSSH\\n%%s\\n' %s >> /etc/fstab", shellQuote(entry)),
		verifyFstabCommand(),
	}
}

// fstabRemoveCommands 生成按挂载点删除条目的命令序列。
func fstabRemoveCommands(target string) []string {
	// 用 awk 按第二个字段精确匹配，而不是 sed 替换文本：
	// 挂载点出现在其它条目或注释里时，文本替换会误伤。
	awk := fmt.Sprintf(
		"awk -v t=%s '$1 ~ /^#/ { print; next } $2 == t { next } { print }' /etc/fstab > /tmp/panelssh-fstab.new && cat /tmp/panelssh-fstab.new > /etc/fstab && rm -f /tmp/panelssh-fstab.new",
		shellQuote(target))
	return []string{
		fmt.Sprintf("cp -a /etc/fstab %s", shellQuote(fstabBackupPath)),
		awk,
		verifyFstabCommand(),
	}
}

// verifyFstabCommand 校验刚改过的 fstab，不合法就用备份还原。
//
// findmnt --verify 只做语法与设备存在性检查，不会真的挂载任何东西 ——
// 用 `mount -a` 校验的话，机器上任何一条本来就坏的条目都会让它失败，
// 从而把"本次改动没问题"误判成"本次改动有问题"。
func verifyFstabCommand() string {
	return fmt.Sprintf(
		"if command -v findmnt >/dev/null 2>&1 && ! findmnt --verify >/dev/null 2>&1; then cp -a %s /etc/fstab; echo 'fstab 校验未通过，已还原到修改前的状态' >&2; exit 1; fi",
		shellQuote(fstabBackupPath))
}

// ---------- 校验辅助 ----------

// validateMountTarget 校验挂载点路径本身是否可用。
//
// 刻意不检查"是否已被挂载"：卸载操作的 Target 正是当前挂着的那个目录。
// 那项检查属于挂载路径自己的前置条件，见 ensureNotMounted。
func validateMountTarget(raw string, _ *StorageSnapshot) (string, error) {
	target := strings.TrimSpace(raw)
	if target == "" {
		return "", errors.New("请填写挂载点目录")
	}
	if forbiddenMountTargets[target] {
		return "", fmt.Errorf("%s 是系统目录，不能作为挂载点", target)
	}
	if !mountPointPattern.MatchString(target) {
		return "", fmt.Errorf("挂载点 %q 不合法：需要是绝对路径，且不能包含空格或特殊字符", target)
	}
	if len(target) > 200 {
		return "", errors.New("挂载点路径过长")
	}
	return target, nil
}

// ensureNotMounted 确认该目录当前没有文件系统挂着。
//
// 往一个已经挂着东西的目录上再挂一次，会把原有内容整片遮住 ——
// 用户以为自己在挂一块新盘，其实上面的数据还在原地，只是看不见了。
func ensureNotMounted(target string, snapshot *StorageSnapshot) error {
	for _, mount := range snapshot.Mounts {
		if mount.Target == target {
			return fmt.Errorf("%s 上已经挂载了 %s，请先卸载", target, mount.Source)
		}
	}
	return nil
}

func validateServer(raw string) (string, error) {
	server := strings.TrimSpace(raw)
	if server == "" {
		return "", errors.New("请填写服务器地址")
	}
	if !serverPattern.MatchString(server) {
		return "", fmt.Errorf("服务器地址 %q 不合法", server)
	}
	return server, nil
}

// validateCredentialField 校验会写进凭据文件的字段。
//
// 换行必须拒绝：凭据文件是逐行解析的，字段里混入换行就能凭空插入
// 任意一行（例如再写一个 domain=），那已经不是"填错"而是注入。
func validateCredentialField(raw, label string) (string, error) {
	value := strings.TrimSpace(raw)
	if len(value) > 128 {
		return "", fmt.Errorf("%s 过长", label)
	}
	if strings.ContainsAny(value, "\n\r\x00") {
		return "", fmt.Errorf("%s 不能包含换行或控制字符", label)
	}
	if strings.Contains(value, "PANELSSH_CIFS_CRED_EOF") {
		return "", fmt.Errorf("%s 不能包含凭据文件的分隔标记", label)
	}
	return value, nil
}

func validateOptions(raw []string) ([]string, error) {
	options := make([]string, 0, len(raw)+2)
	for _, item := range raw {
		option := strings.TrimSpace(item)
		if option == "" {
			continue
		}
		if !mountOptionPattern.MatchString(option) {
			return nil, fmt.Errorf("挂载选项 %q 不合法", option)
		}
		options = append(options, option)
	}
	if len(options) > 10 {
		return nil, errors.New("挂载选项过多")
	}
	return options, nil
}

// mountArgsString 把选项拼成 mount -o 的参数。
//
// nofail 与 _netdev 是 fstab 专用选项，mount 命令不认识它们，
// 直接照搬会让挂载失败并报"unknown option"。
func mountArgsString(options []string) string {
	filtered := make([]string, 0, len(options))
	for _, option := range options {
		if option == "nofail" || option == "_netdev" {
			continue
		}
		filtered = append(filtered, option)
	}
	if len(filtered) == 0 {
		return "defaults"
	}
	return strings.Join(filtered, ",")
}

func findDevice(devices []BlockDevice, path string) *BlockDevice {
	path = strings.TrimSpace(path)
	for i := range devices {
		if found := findDeviceIn(devices[i], path); found != nil {
			return found
		}
	}
	return nil
}

// FindDevicePath 确认一个设备路径确实存在于快照里，返回它规范化的写法。
//
// 即便是只读接口也要走这一步：调用方拿到的应当是"来自快照的可信路径"，
// 而不是请求里那个字符串。少了这道转换，一个查询接口就成了把用户输入
// 原样拼进远端命令的入口。
func FindDevicePath(snapshot *StorageSnapshot, path string) string {
	if snapshot == nil {
		return ""
	}
	device := findDevice(snapshot.Devices, path)
	if device == nil {
		return ""
	}
	return device.Path
}

func findDeviceIn(device BlockDevice, path string) *BlockDevice {
	if device.Path == path {
		return &device
	}
	for i := range device.Children {
		if found := findDeviceIn(device.Children[i], path); found != nil {
			return found
		}
	}
	return nil
}

func hasTool(snapshot *StorageSnapshot, name string) bool {
	for _, tool := range snapshot.Tools {
		if tool == name {
			return true
		}
	}
	return false
}

// ---------- 操作结果复核 ----------

// Verify 检查操作后是否达到了预期状态。
//
// 与网络变更同样的理由：umount 报成功但目录仍挂着、写 fstab 被别的东西
// 覆盖，这些都不该被当成"操作成功"报给用户。
func (e *StorageExpect) Verify(after *StorageSnapshot) error {
	if after == nil {
		return errors.New("操作后无法读取存储状态")
	}

	// 磁盘级操作复核的是设备自身，与"哪个挂载点存在"无关，先分流出去。
	switch e.Action {
	case StorageFormat:
		return e.verifyFormat(after)
	case StoragePartLabel, StoragePartCreate, StoragePartDelete:
		return e.verifyPartitionOp(after)
	}

	var mounted *Mount
	for i := range after.Mounts {
		if after.Mounts[i].Target == e.Target {
			mounted = &after.Mounts[i]
			break
		}
	}

	if e.WantMounted {
		if mounted == nil {
			return fmt.Errorf("%s 上没有检测到挂载", e.Target)
		}
		if e.WantPersistent && !mounted.Persistent {
			return fmt.Errorf("%s 已挂载，但 /etc/fstab 里没有生效的条目", e.Target)
		}
		return nil
	}

	if e.WantPersistent {
		for _, entry := range after.Fstab {
			if entry.Target == e.Target {
				return fmt.Errorf("/etc/fstab 里仍留有 %s 的条目", e.Target)
			}
		}
		return nil
	}

	if mounted != nil {
		return fmt.Errorf("%s 仍然处于挂载状态", e.Target)
	}
	return nil
}

// verifyFormat 复核格式化结果：设备上的文件系统类型确实换成了目标类型。
//
// 只查类型不查卷标：不同 mkfs 对过长或含多字节字符的卷标处理各异
// （截断、转写、或直接拒绝），拿它当成功条件会把一次成功的格式化误判成失败。
func (e *StorageExpect) verifyFormat(after *StorageSnapshot) error {
	device := findDevice(after.Devices, e.Target)
	if device == nil {
		return fmt.Errorf("复核时找不到设备 %s", e.Target)
	}
	if device.FSType != e.WantFSType {
		actual := device.FSType
		if actual == "" {
			actual = "空"
		}
		return fmt.Errorf("%s 的文件系统仍是 %s，没有变成 %s", e.Target, actual, e.WantFSType)
	}
	return nil
}

// verifyPartitionOp 复核分区表与分区变更。
func (e *StorageExpect) verifyPartitionOp(after *StorageSnapshot) error {
	disk := findDevice(after.Devices, e.Target)
	if disk == nil {
		return fmt.Errorf("复核时找不到磁盘 %s", e.Target)
	}

	if e.WantPartTable != "" && disk.PartitionTable != e.WantPartTable {
		return fmt.Errorf("%s 的分区表是 %q，不是期望的 %s", e.Target, disk.PartitionTable, e.WantPartTable)
	}

	if e.WantPartDelta != 0 {
		// 与快照里的分区数对比没有意义（复核时手上只有"之后"的状态），
		// 所以删除分区额外确认编号已消失，新建分区则确认子分区列表非空。
		if e.PartNumber > 0 {
			if findPartitionNumber(disk, e.PartNumber) != nil {
				return fmt.Errorf("%s 上编号 %d 的分区仍然存在", e.Target, e.PartNumber)
			}
			return nil
		}
		if e.WantPartDelta > 0 && len(disk.Children) == 0 {
			return fmt.Errorf("%s 上没有检测到新建的分区", e.Target)
		}
	}

	return nil
}
