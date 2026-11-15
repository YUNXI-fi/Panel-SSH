// Package sysinfo 中的存储部分：块设备、挂载点与 fstab 的采集与解析。
//
// 与网络采集同样的取舍：优先用工具的 JSON 输出（lsblk -J / findmnt -J），
// 而不是解析 df、mount 的文本。df 的表头会随语言变化 —— 中文环境下它输出
// "文件系统 / 1024的块 / 已用"，按列位解析的代码会在这种机器上悄悄给出错数据。
package sysinfo

import (
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 存储信息的分节标记，需与 storageCommand 保持一致。
const (
	secStoTools = "###STOTOOLS"
	secLSBLK    = "###LSBLK"
	secFindmnt  = "###FINDMNT"
	secFstab    = "###FSTAB"
	secMdstat   = "###MDSTAT"
	secNfsUtil  = "###NFSUTIL"
)

// storageCommand 一次采集全部存储信息。
//
// 末尾的 `; true` 与指标采集同理：目标机上缺某个工具不应该让整次采集失败。
//
// lsblk 的列名列表刻意带了一次回退：`-o` 里出现它不认识的列时，
// lsblk 会整个失败并且 stdout 为空 —— 老一点的 util-linux 不认识
// PTTYPE/PARTN/START（这三个决定分区管理能不能用），没有回退就会连带
// 把设备、挂载点、fstab 一起变成空白。回退后它们只是留空，其余照常。
const storageCommand = `echo '###STOTOOLS'; for t in lsblk findmnt blkid smartctl parted mkfs.ext4 mkfs.xfs mkfs.btrfs mkfs.vfat mkfs.exfat mkfs.f2fs mkfs.ntfs; do command -v "$t" >/dev/null 2>&1 && echo "$t"; done
echo '###LSBLK'; lsblk -J -b -o NAME,KNAME,PATH,TYPE,SIZE,FSTYPE,LABEL,UUID,MOUNTPOINTS,RO,RM,MODEL,SERIAL,TRAN,ROTA,PTTYPE,PARTN,START 2>/dev/null || lsblk -J -b -o NAME,KNAME,PATH,TYPE,SIZE,FSTYPE,LABEL,UUID,MOUNTPOINTS,RO,RM,MODEL,SERIAL,TRAN,ROTA 2>/dev/null
echo '###FINDMNT'; findmnt -J -b -o TARGET,SOURCE,FSTYPE,OPTIONS,SIZE,USED,AVAIL,USE%,UUID,LABEL 2>/dev/null
echo '###FSTAB'; cat /etc/fstab 2>/dev/null
echo '###MDSTAT'; cat /proc/mdstat 2>/dev/null
echo '###NFSUTIL'; command -v mount.nfs >/dev/null 2>&1 && echo nfs; command -v mount.cifs >/dev/null 2>&1 && echo cifs
true`

// StorageCommand 返回存储信息采集命令。
func StorageCommand() string { return storageCommand }

// BlockDevice 是一个块设备：整盘、分区、逻辑卷或软 RAID 成员。
type BlockDevice struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// Type: disk / part / lvm / raid / rom / loop
	Type string `json:"type"`
	Size uint64 `json:"size"`
	// FSType / Label / UUID 为空表示设备上还没有文件系统（尚未格式化）。
	FSType      string   `json:"fstype"`
	Label       string   `json:"label"`
	UUID        string   `json:"uuid"`
	MountPoints []string `json:"mountPoints"`
	ReadOnly    bool     `json:"readOnly"`
	// Removable 是外接设备（U 盘、移动硬盘、读卡器）。
	Removable bool   `json:"removable"`
	Model     string `json:"model"`
	Serial    string `json:"serial"`
	// Transport: usb / sata / nvme / mmc ...
	Transport  string `json:"transport"`
	Rotational bool   `json:"rotational"`
	// Children 是分区或逻辑卷。整盘上的分区挂在这里，与 lsblk 的树形一致。
	Children []BlockDevice `json:"children"`
	// System 表示这台设备承载着操作系统（/、/boot、/usr 等）。
	// 它是"能否被格式化/分区"这类破坏性操作的第一个门槛。
	System bool `json:"system"`
	// Busy 表示设备（或它的子设备）正在被挂载，或正被用作 swap。
	Busy bool `json:"busy"`
	// PartitionTable 是所在磁盘的分区表类型（gpt / dos / loop）。
	// 分区操作需要它来判断"这块盘是否已经初始化过"。
	PartitionTable string `json:"partitionTable"`
	// PartNumber 是分区号，整盘与逻辑卷为 0。
	// 删除分区要按编号下命令，而不是按 /dev/sdb1 这样的名字。
	PartNumber int `json:"partNumber"`
	// Start 是分区在磁盘上的起始偏移（字节），用于算出空闲区间。
	Start uint64 `json:"start"`
}

// Mount 是一个已挂载的文件系统。
type Mount struct {
	Target  string `json:"target"`
	Source  string `json:"source"`
	FSType  string `json:"fstype"`
	Options string `json:"options"`
	Total   uint64 `json:"total"`
	Used    uint64 `json:"used"`
	Avail   uint64 `json:"avail"`
	// UsePercent 为 -1 表示没有容量概念（伪文件系统、网络存储未报告容量）。
	UsePercent float64 `json:"usePercent"`
	UUID       string  `json:"uuid"`
	Label      string  `json:"label"`
	// Virtual 表示这是内核提供的伪文件系统（sysfs、cgroup、tmpfs…），
	// 它没有对应的存储设备，与"存储管理"无关。
	Virtual bool `json:"virtual"`
	// Persistent 表示 /etc/fstab 里有对应条目：重启后仍会自动挂载。
	Persistent bool `json:"persistent"`
	// Kind: disk / network / virtual
	Kind string `json:"kind"`
}

// FstabEntry 是 /etc/fstab 里的一条记录。
type FstabEntry struct {
	Source  string `json:"source"`
	Target  string `json:"target"`
	FSType  string `json:"fstype"`
	Options string `json:"options"`
	Raw     string `json:"raw"`
}

// RaidArray 是一个软 RAID 阵列（/proc/mdstat 里的一个 mdX 段）。
type RaidArray struct {
	Name string `json:"name"`
	// Level 是阵列级别：raid0 / raid1 / raid5 / raid6 / raid10 / linear。
	Level string `json:"level"`
	// State 是内核给出的状态词：active / inactive / auto-read-only 等。
	State string `json:"state"`
	// Members 是成员块设备名，已去掉 [序号] 与 (F) 故障标记。
	Members []string `json:"members"`
	// Total 与 Working 是"应有成员数 / 当前在线成员数"。
	Total   int `json:"total"`
	Working int `json:"working"`
	// Blocks 是阵列容量，单位与 /proc/mdstat 一致（1K 块）。
	Blocks uint64 `json:"blocks"`
	// Healthy 为真表示成员齐全。
	Healthy bool `json:"healthy"`
	// Degraded 表示阵列还能用但已经不完整。
	//
	// 单独拎出来，是因为这是最容易被忽略的状态：机器还在正常服务，
	// 用户没有任何体感，直到第二块盘也坏掉。面板必须主动把它摆出来。
	Degraded bool `json:"degraded"`
}

// StorageSnapshot 是一次存储信息采集的结果。
type StorageSnapshot struct {
	Devices []BlockDevice `json:"devices"`
	Mounts  []Mount       `json:"mounts"`
	Fstab   []FstabEntry  `json:"fstab"`
	// Tools 是目标机上存在的相关命令。缺 mount.nfs / mount.cifs 时
	// 网络存储挂载注定失败，面板据此直接给出可执行的提示；
	// 缺 mkfs.* 或 parted 时，对应的格式化与分区操作会被提前拦下。
	Tools []string `json:"tools"`
	// FormatTargets 是支持的文件系统类型 -> 对应的 mkfs 命令名。
	//
	// 由服务端给出而不是让前端自己记一份：两边各记一份的结果，
	// 迟早是面板列出一个这台机器根本不支持的选项。
	FormatTargets map[string]string `json:"formatTargets"`
	// Raid 是 /proc/mdstat 里的软 RAID 阵列。
	Raid      []RaidArray `json:"raid"`
	SampledAt time.Time   `json:"sampledAt"`
}

// CollectStorage 解析远端输出并给出存储快照。
func CollectStorage(raw string, now time.Time) (*StorageSnapshot, error) {
	sections := splitSections(raw)

	devices, err := parseLSBLK(sections[secLSBLK])
	if err != nil {
		return nil, err
	}

	mounts, err := parseFindmnt(sections[secFindmnt])
	if err != nil {
		return nil, err
	}

	fstab := parseFstab(sections[secFstab])
	markPersistent(mounts, fstab)

	// 网络存储的客户端不叫 mount.nfs 这个"命令"，它的可执行文件名固定在
	// /sbin 下且各发行版路径不同，所以单独用一节探测，结果并入工具列表。
	tools := append(parseTools(sections[secStoTools]), parseTools(sections[secNfsUtil])...)
	sort.Strings(tools)

	snapshot := &StorageSnapshot{
		Devices:       devices,
		Mounts:        mounts,
		Fstab:         fstab,
		Tools:         tools,
		FormatTargets: FormatTargets(),
		Raid:          parseMdstat(sections[secMdstat]),
		SampledAt:     now,
	}
	markDeviceUsage(snapshot.Devices)
	return snapshot, nil
}

// ---------- lsblk ----------

type lsblkDevice struct {
	Name        string   `json:"name"`
	Path        string   `json:"path"`
	Type        string   `json:"type"`
	Size        uint64   `json:"size"`
	FSType      *string  `json:"fstype"`
	Label       *string  `json:"label"`
	UUID        *string  `json:"uuid"`
	MountPoints []string `json:"mountpoints"`
	RO          bool     `json:"ro"`
	RM          bool     `json:"rm"`
	Model       *string  `json:"model"`
	Serial      *string  `json:"serial"`
	Tran        *string  `json:"tran"`
	Rota        bool     `json:"rota"`
	// 以下三个字段来自较新的 util-linux。缺失时保持零值，
	// 分区管理会因此提示"还没有分区表"，而不是给出一个错误的判断。
	PtType *string `json:"pttype"`
	PartN  *int    `json:"partn"`
	Start  *uint64 `json:"start"`
	// Children 是分区或逻辑卷。
	Children []lsblkDevice `json:"children"`
}

type lsblkOutput struct {
	BlockDevices []lsblkDevice `json:"blockdevices"`
}

func parseLSBLK(section string) ([]BlockDevice, error) {
	trimmed := strings.TrimSpace(section)
	if trimmed == "" {
		// 与网络采集同一态度：工具不支持 JSON 输出时明确报错，
		// 而不是返回"这台机器上没有磁盘"。
		return nil, errors.New("目标系统的 lsblk 不支持 JSON 输出（lsblk -J），请升级 util-linux 后重试")
	}

	var parsed lsblkOutput
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return nil, errors.New("无法解析 lsblk 输出：" + err.Error())
	}

	devices := make([]BlockDevice, 0, len(parsed.BlockDevices))
	for _, item := range parsed.BlockDevices {
		devices = append(devices, convertLSBLKDevice(item))
	}
	return devices, nil
}

func convertLSBLKDevice(item lsblkDevice) BlockDevice {
	device := BlockDevice{
		Name:           item.Name,
		Path:           item.Path,
		Type:           item.Type,
		Size:           item.Size,
		FSType:         derefString(item.FSType),
		Label:          derefString(item.Label),
		UUID:           derefString(item.UUID),
		ReadOnly:       item.RO,
		Removable:      item.RM,
		Model:          derefString(item.Model),
		Serial:         derefString(item.Serial),
		Transport:      derefString(item.Tran),
		Rotational:     item.Rota,
		PartitionTable: normalizePartTable(derefString(item.PtType)),
		Children:       make([]BlockDevice, 0, len(item.Children)),
		MountPoints:    make([]string, 0, len(item.MountPoints)),
	}
	if item.PartN != nil {
		device.PartNumber = *item.PartN
	}
	if item.Start != nil {
		// lsblk 的 START 固定以 512 字节扇区为单位（-b 也只影响 SIZE），
		// 在这里就换算成字节，免得下游各自再记一遍这个 512。
		device.Start = *item.Start * 512
	}
	for _, point := range item.MountPoints {
		// lsblk 用 "[SWAP]" 表示该设备正在被用作交换分区，它不是真正的挂载点。
		if point != "" {
			device.MountPoints = append(device.MountPoints, point)
		}
	}
	for _, child := range item.Children {
		device.Children = append(device.Children, convertLSBLKDevice(child))
	}
	return device
}

// normalizePartTable 统一分区表类型的写法。
//
// lsblk 把 MBR 叫作 "dos"，parted 叫它 "msdos" —— 不统一的话，
// "这块盘已经初始化过吗"的判断会永远不成立，用户会一直被告知需要先建分区表。
func normalizePartTable(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "dos", "msdos", "mbr":
		return "msdos"
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

// markDeviceUsage 标记每个设备是否承载系统、是否正在被使用。
//
// 两者都要往下递归：判断"整盘是否在用"必须看它的分区 ——
// 一块盘本身没有挂载点，但它的 sda1 分区挂着 /，那就是系统盘。
func markDeviceUsage(devices []BlockDevice) {
	for i := range devices {
		markDeviceLevel(devices[i].Children)
		devices[i].System = deviceIsSystem(devices[i])
		devices[i].Busy = deviceIsBusy(devices[i])
	}
}

// markDeviceLevel 递归标记下一层。必须先把子级算完再算自己：
// deviceIsSystem/deviceIsBusy 都要读取子级的结果。
func markDeviceLevel(devices []BlockDevice) {
	for i := range devices {
		markDeviceLevel(devices[i].Children)
		devices[i].System = deviceIsSystem(devices[i])
		devices[i].Busy = deviceIsBusy(devices[i])
	}
}

func deviceIsSystem(device BlockDevice) bool {
	for _, point := range device.MountPoints {
		if isSystemMountPoint(point) {
			return true
		}
	}
	if strings.EqualFold(device.FSType, "swap") {
		return true
	}
	for _, child := range device.Children {
		if child.System {
			return true
		}
	}
	return false
}

func deviceIsBusy(device BlockDevice) bool {
	if len(device.MountPoints) > 0 || strings.EqualFold(device.FSType, "swap") {
		return true
	}
	for _, child := range device.Children {
		if child.Busy {
			return true
		}
	}
	return false
}

// systemMountPrefixes 是需要保护的挂载区域。
//
// 判定刻意保守：除了明确的数据目录（/mnt、/media、/srv），其余都当作系统区域。
// 漏判一个系统盘的代价是"用户把操作系统格了"，误判一个数据盘的代价只是
// "多问一句" —— 两者完全不对等。
var systemMountPoints = map[string]bool{
	"/":         true,
	"/boot":     true,
	"/boot/efi": true,
	"/usr":      true,
	"/var":      true,
	"/etc":      true,
	"/opt":      true,
	"/home":     true,
	"/root":     true,
	"/sbin":     true,
	"/bin":      true,
	"/lib":      true,
}

var systemMountPrefixes = []string{"/boot/", "/usr/", "/var/", "/etc/", "/lib/", "/opt/"}

func isSystemMountPoint(point string) bool {
	if systemMountPoints[point] {
		return true
	}
	for _, prefix := range systemMountPrefixes {
		if strings.HasPrefix(point, prefix) {
			return true
		}
	}
	return false
}

// ---------- findmnt ----------

type findmntEntry struct {
	Target   string         `json:"target"`
	Source   string         `json:"source"`
	FSType   string         `json:"fstype"`
	Options  string         `json:"options"`
	Size     uint64         `json:"size"`
	Used     uint64         `json:"used"`
	Avail    uint64         `json:"avail"`
	UsePct   string         `json:"use%"`
	UUID     *string        `json:"uuid"`
	Label    *string        `json:"label"`
	Children []findmntEntry `json:"children"`
}

type findmntOutput struct {
	Filesystems []findmntEntry `json:"filesystems"`
}

// virtualFilesystems 是内核提供的伪文件系统，没有对应的存储设备。
//
// 不排除的话，一台普通机器上会列出几十个 /sys/fs/cgroup 之类的挂载点，
// 把真正的数据盘挤到看不见的地方。
var virtualFilesystems = map[string]bool{
	"sysfs": true, "proc": true, "procfs": true, "cgroup": true, "cgroup2": true,
	"tmpfs": true, "devtmpfs": true, "devpts": true, "securityfs": true,
	"pstore": true, "bpf": true, "tracefs": true, "debugfs": true, "configfs": true,
	"fusectl": true, "hugetlbfs": true, "mqueue": true, "ramfs": true, "autofs": true,
	"efivarfs": true, "binfmt_misc": true, "nsfs": true, "rpc_pipefs": true,
	"nfsd": true, "fuse.gvfsd-fuse": true, "fuse.portal": true, "overlay": true,
	"selinuxfs": true, "systemd-1": true, "squashfs": true,
}

// networkFilesystems 是需要网络才可用的文件系统类型。
var networkFilesystems = map[string]bool{
	"nfs": true, "nfs4": true, "cifs": true, "smb3": true, "smbfs": true,
	"fuse.sshfs": true, "fuse.rclone": true, "9p": true, "glusterfs": true,
	"ceph": true, "afs": true, "ncpfs": true, "davfs": true,
}

func parseFindmnt(section string) ([]Mount, error) {
	trimmed := strings.TrimSpace(section)
	if trimmed == "" {
		return nil, errors.New("目标系统的 findmnt 不支持 JSON 输出（findmnt -J），请升级 util-linux 后重试")
	}

	var parsed findmntOutput
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return nil, errors.New("无法解析 findmnt 输出：" + err.Error())
	}

	mounts := make([]Mount, 0, len(parsed.Filesystems))
	flattenMounts(parsed.Filesystems, &mounts)

	// 按挂载点排序：用户在这个窗口里找的是某个目录挂在哪，
	// 而 findmnt 的输出顺序取决于挂载发生的先后，完全没有规律。
	sort.SliceStable(mounts, func(i, j int) bool { return mounts[i].Target < mounts[j].Target })
	return mounts, nil
}

func flattenMounts(entries []findmntEntry, out *[]Mount) {
	for _, entry := range entries {
		// 伪文件系统整棵子树都跳过：/sys 下面还会挂着 /sys/fs/cgroup、
		// /sys/kernel/security 等等，逐个判断不如直接剪掉这一枝。
		if virtualFilesystems[entry.FSType] {
			continue
		}

		mount := Mount{
			Target:  entry.Target,
			Source:  entry.Source,
			FSType:  entry.FSType,
			Options: entry.Options,
			Total:   entry.Size,
			Used:    entry.Used,
			Avail:   entry.Avail,
			UUID:    derefString(entry.UUID),
			Label:   derefString(entry.Label),
			// "-" 是 findmnt 在"没有容量概念"时的占位符，不能当成 0%。
			UsePercent: -1,
			Kind:       "disk",
		}
		if percent := strings.TrimSuffix(strings.TrimSpace(entry.UsePct), "%"); percent != "" && percent != "-" {
			if value, err := strconv.ParseFloat(percent, 64); err == nil {
				mount.UsePercent = value
			}
		}
		if networkFilesystems[entry.FSType] {
			mount.Kind = "network"
		}

		*out = append(*out, mount)
		flattenMounts(entry.Children, out)
	}
}

// ---------- fstab ----------

func parseFstab(section string) []FstabEntry {
	entries := make([]FstabEntry, 0, 8)
	for _, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 4 {
			// 字段不足的行可能是被注释掉的半截内容或格式错误，跳过不影响展示。
			continue
		}
		entries = append(entries, FstabEntry{
			Source:  fields[0],
			Target:  fields[1],
			FSType:  fields[2],
			Options: fields[3],
			Raw:     trimmed,
		})
	}
	return entries
}

// markPersistent 把"这台机器重启后还会不会自动挂载"标在挂载点上。
//
// 用户最常困惑的正是这一点：面板里挂上了，重启后却没了。
func markPersistent(mounts []Mount, fstab []FstabEntry) {
	for i := range mounts {
		for _, entry := range fstab {
			if entry.Target == mounts[i].Target {
				mounts[i].Persistent = true
				break
			}
		}
	}
}

// ---------- 其它 ----------

// /proc/mdstat 里的正则。
//
// 头部形如 `md0 : active raid1 sdb1[0] sdc1[1]`，紧随其后的缩进行给出
// 容量与成员计数：`1953382464 blocks super 1.2 [2/2] [UU]`。
var (
	mdstatHeader = regexp.MustCompile(`^(md\d+)\s*:\s*(.*)$`)
	mdstatCounts = regexp.MustCompile(`\[(\d+)/(\d+)\]`)
	mdstatMember = regexp.MustCompile(`^([A-Za-z0-9_-]+)\[\d+\](\(F\))?$`)
	mdstatBlocks = regexp.MustCompile(`^(\d+)\s+blocks`)
)

// parseMdstat 解析 /proc/mdstat。
//
// 只取头部那一行是不够的：阵列健不健康写在紧随其后的 `[2/1] [U_]` 里，
// 而"少了一个成员"恰恰是这块屏最该报出来的事。
func parseMdstat(section string) []RaidArray {
	arrays := make([]RaidArray, 0, 2)
	current := -1

	for _, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		if match := mdstatHeader.FindStringSubmatch(trimmed); match != nil {
			arrays = append(arrays, parseMdstatHead(match[1], match[2]))
			current = len(arrays) - 1
			continue
		}

		// 缩进行属于上一个阵列；非缩进的行（unused devices: …）说明它的段落结束了。
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			current = -1
			continue
		}
		if current < 0 {
			continue
		}
		applyMdstatDetail(&arrays[current], trimmed)
	}

	for i := range arrays {
		arrays[i].Healthy = arrays[i].Total > 0 && arrays[i].Total == arrays[i].Working
		arrays[i].Degraded = arrays[i].Total > 0 && arrays[i].Working < arrays[i].Total
	}
	return arrays
}

func parseMdstatHead(name, rest string) RaidArray {
	array := RaidArray{Name: name}

	fields := strings.Fields(rest)
	if len(fields) > 0 {
		array.State = fields[0]
	}
	for _, field := range fields[1:] {
		if isRaidLevel(field) {
			array.Level = field
			continue
		}
		// 成员写成 `sdb1[0]`，掉线的会带 `(F)` 后缀。
		if match := mdstatMember.FindStringSubmatch(field); match != nil {
			array.Members = append(array.Members, match[1])
		}
	}
	return array
}

func applyMdstatDetail(array *RaidArray, line string) {
	if match := mdstatBlocks.FindStringSubmatch(line); match != nil {
		if blocks, err := strconv.ParseUint(match[1], 10, 64); err == nil {
			array.Blocks = blocks
		}
	}
	if match := mdstatCounts.FindStringSubmatch(line); match != nil {
		total, _ := strconv.Atoi(match[1])
		working, _ := strconv.Atoi(match[2])
		array.Total = total
		array.Working = working
	}
}

func isRaidLevel(field string) bool {
	for _, prefix := range []string{"raid", "linear", "multipath"} {
		if strings.HasPrefix(field, prefix) {
			return true
		}
	}
	return false
}
