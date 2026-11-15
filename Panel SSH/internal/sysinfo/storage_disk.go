package sysinfo

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// 磁盘级操作：格式化、分区表与分区。
//
// 单独成文件，是因为它们的风险等级和前四个操作不同：挂载搞错了，结果只是
// "这次没成功"；而 mkfs 与 parted 一旦执行，盘上的数据就没了。这里的每一条
// 校验都对应一种实际会被误伤的情况，不是形式上的完备。
//
// 两条贯穿始终的原则：
//  1. 只认服务端刚采集到的快照，绝不相信请求里对设备状态的描述；
//  2. 会毁数据的操作必须带 Confirm，且要和目标设备逐字符相同。

// ---------- 破坏性操作的共用门槛 ----------

// ensureDestructible 是"会毁数据"的操作共用的前置检查。
//
// verb 用来拼出贴合语境的提示（"格式化" / "重新分区" / "删除分区"）。
func ensureDestructible(device *BlockDevice, verb string) error {
	if device.System {
		mounted := strings.Join(deviceMountPoints(device), "、")
		if mounted == "" {
			mounted = "系统目录"
		}
		return fmt.Errorf("%s 承载着操作系统（%s），不允许%s", device.Path, mounted, verb)
	}
	// 注意这里查的是"自身 + 所有子设备"的挂载点：整盘上的某个分区还挂着时，
	// 只看整盘自己的挂载点会把这种情况整个漏掉。
	if points := deviceMountPoints(device); len(points) > 0 {
		return fmt.Errorf("%s 正在使用中（挂载于 %s），请先在「挂载点」里卸载再%s",
			device.Path, strings.Join(points, "、"), verb)
	}
	if device.Busy {
		return fmt.Errorf("%s 仍被占用（可能作为 swap 或 RAID 成员），请先释放再%s", device.Path, verb)
	}
	if device.ReadOnly {
		return fmt.Errorf("%s 处于只读状态，无法%s", device.Path, verb)
	}
	return nil
}

// deviceMountPoints 收集设备自己以及它全部子设备的挂载点。
func deviceMountPoints(device *BlockDevice) []string {
	points := append([]string{}, device.MountPoints...)
	for i := range device.Children {
		points = append(points, deviceMountPoints(&device.Children[i])...)
	}
	return points
}

// confirmMatches 校验破坏性操作的二次确认。
//
// 服务端必须自己独立校验这一步：前端那道确认框是给"人会看"用的，
// 而误操作往往正发生在"人没看"的时候。
func confirmMatches(req StorageRequest, device *BlockDevice) error {
	if strings.TrimSpace(req.Confirm) != device.Path {
		return fmt.Errorf("确认信息不匹配：请在确认框里原样输入 %s", device.Path)
	}
	return nil
}

// ---------- 格式化 ----------

// formatTarget 是一种可以格式化成的文件系统。
type formatTarget struct {
	// FSType 是类型名，同时用于复核与展示。
	FSType string
	// Tool 是 mkfs 命令名，也用来在目标机上探测是否已安装。
	Tool string
	// ForceArgs 让 mkfs 不再询问"设备上已有文件系统，确定继续吗"。
	// 面板已经做过二次确认了；再让命令停下来等一个永远不会到来的输入，
	// 只会把 SSH 会话挂到超时。
	ForceArgs []string
	// LabelFlag 是设置卷标的参数，为空表示该文件系统不接受卷标。
	LabelFlag string
}

// formatTargets 按常用程度排序，前端下拉框直接用这个顺序。
var formatTargets = []formatTarget{
	{FSType: "ext4", Tool: "mkfs.ext4", ForceArgs: []string{"-F"}, LabelFlag: "-L"},
	{FSType: "xfs", Tool: "mkfs.xfs", ForceArgs: []string{"-f"}, LabelFlag: "-L"},
	{FSType: "btrfs", Tool: "mkfs.btrfs", ForceArgs: []string{"-f"}, LabelFlag: "-L"},
	{FSType: "vfat", Tool: "mkfs.vfat", LabelFlag: "-n"},
	{FSType: "exfat", Tool: "mkfs.exfat", LabelFlag: "-n"},
	{FSType: "f2fs", Tool: "mkfs.f2fs", ForceArgs: []string{"-f"}, LabelFlag: "-l"},
	{FSType: "ntfs", Tool: "mkfs.ntfs", ForceArgs: []string{"-F"}, LabelFlag: "-L"},
}

func findFormatTarget(fstype string) (formatTarget, bool) {
	for _, target := range formatTargets {
		if target.FSType == fstype {
			return target, true
		}
	}
	return formatTarget{}, false
}

// FormatTargets 返回支持格式化的文件系统类型与各自的 mkfs 命令，
// 供前端生成下拉选项、并在目标机缺少工具时提前禁用。
func FormatTargets() map[string]string {
	tools := make(map[string]string, len(formatTargets))
	for _, target := range formatTargets {
		tools[target.FSType] = target.Tool
	}
	return tools
}

// volumeLabelPattern 限制卷标字符集。
//
// 各种文件系统对卷标的规定并不一致，取交集（字母、数字、空格与 . _ -）
// 最省事，也顺带保证标签写进 fstab 时不需要额外转义。
var volumeLabelPattern = regexp.MustCompile(`^[\p{L}\p{N}._ -]{1,32}$`)

func validateVolumeLabel(raw string) (string, error) {
	label := strings.TrimSpace(raw)
	if label == "" {
		return "", nil
	}
	if !volumeLabelPattern.MatchString(label) {
		return "", errors.New("卷标只能用字母、数字、空格与 . _ -，且不超过 32 个字符")
	}
	return label, nil
}

func planFormat(req StorageRequest, snapshot *StorageSnapshot) (*StoragePlan, error) {
	device := findDevice(snapshot.Devices, req.Device)
	if device == nil {
		return nil, fmt.Errorf("主机上不存在设备 %s", req.Device)
	}
	if err := confirmMatches(req, device); err != nil {
		return nil, err
	}

	fstype := strings.ToLower(strings.TrimSpace(req.FSType))
	target, ok := findFormatTarget(fstype)
	if !ok {
		return nil, fmt.Errorf("不支持格式化成 %q（可选：%s）",
			req.FSType, strings.Join(sortedFormatNames(), "、"))
	}

	// 顺序要紧：系统盘往往同时"有分区"，先报后者会让用户以为把分区删光就能格式化，
	// 于是照做，然后在真正动手时才被拦下 —— 那一次多余的误操作本可以避免。
	if err := ensureDestructible(device, "格式化"); err != nil {
		return nil, err
	}

	// 整盘上还有分区时不给格式化：那种盘该格式化的是分区，而不是整盘。
	// 这个判断与前端"能否挂载"的口径保持一致，避免出现"整盘能格式化却挂不上"的错位。
	if device.Type == "disk" && len(device.Children) > 0 {
		return nil, fmt.Errorf("%s 上还有 %d 个分区，请对该分区格式化，或先删除分区",
			device.Path, len(device.Children))
	}

	if !hasTool(snapshot, target.Tool) {
		return nil, fmt.Errorf("目标主机上没有 %s，装好对应工具后再试", target.Tool)
	}

	label, err := validateVolumeLabel(req.Label)
	if err != nil {
		return nil, err
	}

	summary := fmt.Sprintf("把 %s 格式化成 %s", device.Path, fstype)
	if label != "" {
		summary += fmt.Sprintf("（卷标 %s）", label)
	}

	return &StoragePlan{
		Summary: summary,
		Mount:   buildMkfsCommand(target, label, device.Path),
		Finish: []string{
			// 让 udev 认下新的超级块，否则紧接着的复核可能还读到旧的文件系统类型，
			// 把一次成功的格式化报成"命令成功但状态不对"。
			"udevadm settle 2>/dev/null || true",
		},
		Expect: &StorageExpect{Action: StorageFormat, Target: device.Path, WantFSType: fstype},
	}, nil
}

func sortedFormatNames() []string {
	names := make([]string, 0, len(formatTargets))
	for _, target := range formatTargets {
		names = append(names, target.FSType)
	}
	return names
}

// buildMkfsCommand 拼出格式化命令。
//
// 设备路径取自刚刚采集的快照而不是请求，所以这里的 shellQuote 是防呆而非必需；
// 保留它是为了把"任何进入远端命令的值都要转义"这条规矩贯穿到底，不留例外。
func buildMkfsCommand(target formatTarget, label, device string) string {
	args := []string{target.Tool}
	args = append(args, target.ForceArgs...)
	if label != "" && target.LabelFlag != "" {
		args = append(args, target.LabelFlag, shellQuote(label))
	}
	args = append(args, shellQuote(device))
	return strings.Join(args, " ")
}

// ---------- 分区表与分区 ----------

// partTablePattern 只放开这两种分区表：gpt 用于新盘，msdos 用于需要与老系统共存时。
var partTablePattern = regexp.MustCompile(`^(gpt|msdos)$`)

// partBoundPattern 收下 parted 的两种位置写法：百分比（0%、100%）
// 与带单位的偏移（512MiB、1GiB、2048s）。
var partBoundPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(%|[kKmMgGtTpP]i?[bB]|[bBsS])$`)

func validatePartBound(raw, label string) (string, error) {
	bound := strings.TrimSpace(raw)
	if bound == "" {
		return "", fmt.Errorf("请填写分区的%s", label)
	}
	if !partBoundPattern.MatchString(bound) {
		return "", fmt.Errorf("分区%s %q 不合法，可写 0%%、100%%、512MiB 或 2048s", label, raw)
	}
	return bound, nil
}

// findDisk 取出并确认这是一个整盘 —— 分区表与分区操作只能落在整盘上。
func findDisk(snapshot *StorageSnapshot, path string) (*BlockDevice, error) {
	device := findDevice(snapshot.Devices, path)
	if device == nil {
		return nil, fmt.Errorf("主机上不存在设备 %s", path)
	}
	if device.Type != "disk" {
		return nil, fmt.Errorf("%s 不是整盘（类型 %s），分区操作只能作用于整盘", device.Path, device.Type)
	}
	return device, nil
}

func planPartLabel(req StorageRequest, snapshot *StorageSnapshot) (*StoragePlan, error) {
	device, err := findDisk(snapshot, req.Device)
	if err != nil {
		return nil, err
	}
	if err := confirmMatches(req, device); err != nil {
		return nil, err
	}

	table := strings.ToLower(strings.TrimSpace(req.PartTable))
	if !partTablePattern.MatchString(table) {
		return nil, fmt.Errorf("分区表类型 %q 不支持（可选 gpt 或 msdos）", req.PartTable)
	}
	if err := ensureDestructible(device, "重新分区"); err != nil {
		return nil, err
	}
	if !hasTool(snapshot, "parted") {
		return nil, errors.New("目标主机上没有 parted，无法创建分区表")
	}

	summary := fmt.Sprintf("把 %s 的分区表重建为 %s", device.Path, table)
	if len(device.Children) > 0 {
		// 重建分区表等于把所有分区一起抹掉，这一点必须在摘要里说破。
		summary += fmt.Sprintf("，原有 %d 个分区及其数据将全部丢失", len(device.Children))
	}

	return &StoragePlan{
		Summary: summary,
		Mount:   fmt.Sprintf("parted -s %s mklabel %s", shellQuote(device.Path), shellQuote(table)),
		Finish:  partitionRefreshCommands(),
		Expect:  &StorageExpect{Action: StoragePartLabel, Target: device.Path, WantPartTable: table},
	}, nil
}

func planPartCreate(req StorageRequest, snapshot *StorageSnapshot) (*StoragePlan, error) {
	device, err := findDisk(snapshot, req.Device)
	if err != nil {
		return nil, err
	}
	if device.PartitionTable == "" {
		return nil, fmt.Errorf("%s 上还没有分区表，请先执行「初始化分区表」", device.Path)
	}
	if err := ensureDestructible(device, "建立分区"); err != nil {
		return nil, err
	}
	if !hasTool(snapshot, "parted") {
		return nil, errors.New("目标主机上没有 parted，无法新建分区")
	}

	start, err := validatePartBound(req.PartStart, "起始位置")
	if err != nil {
		return nil, err
	}
	end, err := validatePartBound(req.PartEnd, "结束位置")
	if err != nil {
		return nil, err
	}

	return &StoragePlan{
		Summary: fmt.Sprintf("在 %s 上新建分区（%s – %s）", device.Path, start, end),
		Mount: fmt.Sprintf("parted -s -a optimal %s mkpart primary %s %s",
			shellQuote(device.Path), shellQuote(start), shellQuote(end)),
		Finish: partitionRefreshCommands(),
		// 新分区的编号由 parted 决定，所以复核的是"分区数量 +1"而不是某个具体编号。
		Expect: &StorageExpect{Action: StoragePartCreate, Target: device.Path, WantPartDelta: 1},
	}, nil
}

func planPartDelete(req StorageRequest, snapshot *StorageSnapshot) (*StoragePlan, error) {
	device, err := findDisk(snapshot, req.Device)
	if err != nil {
		return nil, err
	}
	if err := confirmMatches(req, device); err != nil {
		return nil, err
	}
	if req.PartNumber <= 0 {
		return nil, errors.New("请指定要删除的分区号")
	}
	// 先确认整盘本身能动，再看具体分区：盘都动不了的时候，
	// 讨论"编号存不存在"只会把真正的原因盖掉。
	if err := ensureDestructible(device, "删除分区"); err != nil {
		return nil, err
	}

	existing := findPartitionNumber(device, req.PartNumber)
	if existing == nil {
		return nil, fmt.Errorf("%s 上没有编号 %d 的分区", device.Path, req.PartNumber)
	}
	// 删一个还挂着的分区，内核会直接拒绝。与其让用户对着 "Device or resource busy"
	// 猜，不如在这里说清楚该先做什么。
	if points := deviceMountPoints(existing); len(points) > 0 {
		return nil, fmt.Errorf("分区 %s 正挂载在 %s，请先卸载", existing.Path, strings.Join(points, "、"))
	}
	if !hasTool(snapshot, "parted") {
		return nil, errors.New("目标主机上没有 parted，无法删除分区")
	}

	summary := fmt.Sprintf("删除 %s 上的分区 %d（%s）", device.Path, req.PartNumber, existing.Path)
	if existing.FSType != "" {
		summary += "，该分区上的数据将一并丢失"
	}

	return &StoragePlan{
		Summary: summary,
		Mount:   fmt.Sprintf("parted -s %s rm %d", shellQuote(device.Path), req.PartNumber),
		Finish:  partitionRefreshCommands(),
		Expect: &StorageExpect{
			Action: StoragePartDelete, Target: device.Path,
			WantPartDelta: -1, PartNumber: req.PartNumber,
		},
	}, nil
}

// partitionRefreshCommands 让内核与 udev 认下刚改过的分区表。
//
// 少了这一步，紧随其后的复核会读到旧的分区列表，把成功的操作报成失败。
func partitionRefreshCommands() []string {
	return []string{
		"udevadm settle 2>/dev/null || true",
		"partprobe 2>/dev/null || true",
	}
}

// findPartitionNumber 在磁盘的分区里按分区号查找。
func findPartitionNumber(device *BlockDevice, number int) *BlockDevice {
	for i := range device.Children {
		child := &device.Children[i]
		if child.PartNumber == number {
			return child
		}
		if found := findPartitionNumber(child, number); found != nil {
			return found
		}
	}
	return nil
}

// ---------- fstab 条目单独删除 ----------

func planFstabRemove(req StorageRequest, snapshot *StorageSnapshot) (*StoragePlan, error) {
	target := strings.TrimSpace(req.Target)
	if target == "" {
		return nil, errors.New("请指定要移除的挂载点")
	}

	var entry *FstabEntry
	for i := range snapshot.Fstab {
		if snapshot.Fstab[i].Target == target {
			entry = &snapshot.Fstab[i]
			break
		}
	}
	if entry == nil {
		return nil, fmt.Errorf("/etc/fstab 里没有 %s 的条目", target)
	}

	summary := fmt.Sprintf("从 /etc/fstab 移除 %s（%s）的条目", target, entry.Source)
	// 目录还挂着却把条目删了，重启后它就不会自己回来 —— 多数人并不是这个意思，
	// 所以这句提醒必须给。
	for _, mount := range snapshot.Mounts {
		if mount.Target == target {
			summary += "。注意：该目录当前仍处于挂载状态，重启后不会自动挂载"
			break
		}
	}

	return &StoragePlan{
		Summary: summary,
		// 没有"主操作"：三步改 fstab 的命令本身就是全部内容（备份 → 改 → 校验）。
		Persist: fstabRemoveCommands(target),
		Expect:  &StorageExpect{Action: StorageFstabRemove, Target: target, WantPersistent: true},
	}, nil
}
