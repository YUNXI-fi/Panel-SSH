package sysinfo

import (
	"strings"
	"testing"
)

// 破坏性操作的测试重点不在"命令拼得对不对"，而在"该拦的有没有拦住"：
// 这一类操作出错没有回滚的机会，所以拒绝用例比成功用例更重要。

// requireRejected 断言一次请求必须被校验拦下。
func requireRejected(t *testing.T, snapshot *StorageSnapshot, name string, req StorageRequest) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		if _, err := PlanStorageChange(req, snapshot); err == nil {
			t.Fatal("应当被拒绝，实际通过了校验")
		}
	})
}

// ---------- 格式化 ----------

func TestPlanFormat(t *testing.T) {
	snapshot := storageSnapshot(t)

	// /dev/sdc 是一块没有分区、也没有文件系统的空盘 —— 格式化的典型对象。
	plan, err := PlanStorageChange(StorageRequest{
		Action:  StorageFormat,
		Device:  "/dev/sdc",
		FSType:  "ext4",
		Label:   "data",
		Confirm: "/dev/sdc",
	}, snapshot)
	if err != nil {
		t.Fatalf("生成格式化计划失败：%v", err)
	}

	if plan.Mount != "mkfs.ext4 -F -L 'data' '/dev/sdc'" {
		t.Fatalf("格式化命令错误：%s", plan.Mount)
	}
	// 格式化后必须刷新 udev：否则紧随其后的复核会读到旧的超级块，
	// 把一次成功的操作报成"命令成功但状态不对"。
	if joined := strings.Join(plan.Finish, " "); !strings.Contains(joined, "udevadm settle") {
		t.Fatalf("缺少 udev 刷新步骤：%v", plan.Finish)
	}
	if plan.Expect.Action != StorageFormat || plan.Expect.WantFSType != "ext4" {
		t.Fatalf("复核条件错误：%+v", plan.Expect)
	}
}

func TestPlanFormatRejections(t *testing.T) {
	snapshot := storageSnapshot(t)

	cases := []struct {
		name string
		req  StorageRequest
	}{
		{
			name: "没有二次确认",
			req:  StorageRequest{Action: StorageFormat, Device: "/dev/sdc", FSType: "ext4"},
		},
		{
			name: "确认的是另一块设备",
			req: StorageRequest{
				Action: StorageFormat, Device: "/dev/sdc", FSType: "ext4", Confirm: "/dev/sdb",
			},
		},
		{
			// 系统盘上挂着 /：必须先被"承载操作系统"拦下，
			// 而不是被后面的"还有分区"挡回来。
			name: "目标是系统盘",
			req: StorageRequest{
				Action: StorageFormat, Device: "/dev/nvme0n1", FSType: "ext4", Confirm: "/dev/nvme0n1",
			},
		},
		{
			name: "目标是已挂载的分区",
			req: StorageRequest{
				Action: StorageFormat, Device: "/dev/nvme1n1p1", FSType: "ext4",
				Confirm: "/dev/nvme1n1p1",
			},
		},
		{
			name: "整盘上还有分区",
			req: StorageRequest{
				Action: StorageFormat, Device: "/dev/sdb", FSType: "ext4", Confirm: "/dev/sdb",
			},
		},
		{
			name: "设备不存在",
			req: StorageRequest{
				Action: StorageFormat, Device: "/dev/sdz", FSType: "ext4", Confirm: "/dev/sdz",
			},
		},
		{
			name: "不支持的文件系统",
			req: StorageRequest{
				Action: StorageFormat, Device: "/dev/sdc", FSType: "zfs", Confirm: "/dev/sdc",
			},
		},
		{
			name: "卷标里带命令分隔符",
			req: StorageRequest{
				Action: StorageFormat, Device: "/dev/sdc", FSType: "ext4",
				Label: "x; reboot", Confirm: "/dev/sdc",
			},
		},
	}

	for _, tc := range cases {
		requireRejected(t, snapshot, tc.name, tc.req)
	}
}

// ---------- 分区表与分区 ----------

func TestPlanPartition(t *testing.T) {
	snapshot := storageSnapshot(t)

	// /dev/sdb 上已经有分区表（lsblk 报 dos，统一成 parted 的叫法 msdos）。
	create, err := PlanStorageChange(StorageRequest{
		Action:    StoragePartCreate,
		Device:    "/dev/sdb",
		PartStart: "0%",
		PartEnd:   "100%",
	}, snapshot)
	if err != nil {
		t.Fatalf("生成新建分区计划失败：%v", err)
	}
	if create.Mount != "parted -s -a optimal '/dev/sdb' mkpart primary '0%' '100%'" {
		t.Fatalf("新建分区命令错误：%s", create.Mount)
	}
	// 新分区的编号由 parted 决定，所以复核的是"数量 +1"而不是某个具体编号。
	if create.Expect.WantPartDelta != 1 {
		t.Fatalf("复核条件错误：%+v", create.Expect)
	}

	// 删除分区要把描述里的"数据会丢"说清楚。
	del, err := PlanStorageChange(StorageRequest{
		Action: StoragePartDelete, Device: "/dev/sdb", PartNumber: 1, Confirm: "/dev/sdb",
	}, snapshot)
	if err != nil {
		t.Fatalf("生成删除分区计划失败：%v", err)
	}
	if del.Mount != "parted -s '/dev/sdb' rm 1" {
		t.Fatalf("删除分区命令错误：%s", del.Mount)
	}
	if !strings.Contains(del.Summary, "数据将一并丢失") {
		t.Fatalf("删除摘要缺少数据丢失提示：%s", del.Summary)
	}
	if del.Expect.WantPartDelta != -1 || del.Expect.PartNumber != 1 {
		t.Fatalf("复核条件错误：%+v", del.Expect)
	}

	// 重建分区表：摘要必须点明原有分区会一起消失。
	label, err := PlanStorageChange(StorageRequest{
		Action: StoragePartLabel, Device: "/dev/sdb", PartTable: "gpt", Confirm: "/dev/sdb",
	}, snapshot)
	if err != nil {
		t.Fatalf("生成分区表计划失败：%v", err)
	}
	if label.Mount != "parted -s '/dev/sdb' mklabel 'gpt'" {
		t.Fatalf("分区表命令错误：%s", label.Mount)
	}
	if !strings.Contains(label.Summary, "全部丢失") {
		t.Fatalf("分区表摘要缺少数据丢失提示：%s", label.Summary)
	}
}

func TestPlanPartitionRejections(t *testing.T) {
	snapshot := storageSnapshot(t)

	cases := []struct {
		name string
		req  StorageRequest
	}{
		{
			name: "对分区本身新建分区",
			req:  StorageRequest{Action: StoragePartCreate, Device: "/dev/sdb1", PartStart: "0%", PartEnd: "100%"},
		},
		{
			name: "盘上还没有分区表",
			req:  StorageRequest{Action: StoragePartCreate, Device: "/dev/sdc", PartStart: "0%", PartEnd: "100%"},
		},
		{
			name: "起止位置写得不是位置",
			req:  StorageRequest{Action: StoragePartCreate, Device: "/dev/sdb", PartStart: "开头", PartEnd: "100%"},
		},
		{
			name: "起止位置带命令分隔符",
			req: StorageRequest{
				Action: StoragePartCreate, Device: "/dev/sdb", PartStart: "0%", PartEnd: "100%; reboot",
			},
		},
		{
			name: "起止位置为空",
			req:  StorageRequest{Action: StoragePartCreate, Device: "/dev/sdb", PartEnd: "100%"},
		},
		{
			name: "删除不存在的分区号",
			req:  StorageRequest{Action: StoragePartDelete, Device: "/dev/sdb", PartNumber: 9, Confirm: "/dev/sdb"},
		},
		{
			name: "删除分区没做二次确认",
			req:  StorageRequest{Action: StoragePartDelete, Device: "/dev/sdb", PartNumber: 1},
		},
		{
			name: "分区号缺失",
			req:  StorageRequest{Action: StoragePartDelete, Device: "/dev/sdb", Confirm: "/dev/sdb"},
		},
		{
			name: "分区表类型不支持",
			req:  StorageRequest{Action: StoragePartLabel, Device: "/dev/sdc", PartTable: "apm", Confirm: "/dev/sdc"},
		},
		{
			name: "重建系统盘的分区表",
			req:  StorageRequest{Action: StoragePartLabel, Device: "/dev/nvme0n1", PartTable: "gpt", Confirm: "/dev/nvme0n1"},
		},
		{
			// 盘上分区还挂着：必须先卸载，否则内核会拒绝这条 parted 命令。
			name: "对已挂载分区的盘重建分区表",
			req:  StorageRequest{Action: StoragePartLabel, Device: "/dev/nvme1n1", PartTable: "gpt", Confirm: "/dev/nvme1n1"},
		},
	}

	for _, tc := range cases {
		requireRejected(t, snapshot, tc.name, tc.req)
	}
}

// ---------- fstab 条目单独删除 ----------

func TestPlanFstabRemove(t *testing.T) {
	snapshot := storageSnapshot(t)

	plan, err := PlanStorageChange(StorageRequest{
		Action: StorageFstabRemove,
		Target: "/mnt/nas",
	}, snapshot)
	if err != nil {
		t.Fatalf("生成 fstab 删除计划失败：%v", err)
	}

	// 这个操作没有"主命令"：全部内容都在 Persist 里（备份 → 改 → 校验）。
	if plan.Mount != "" {
		t.Fatalf("删除 fstab 条目不该产生主命令：%s", plan.Mount)
	}
	joined := strings.Join(plan.Persist, "\n")
	if !strings.Contains(joined, "cp -a /etc/fstab") || !strings.Contains(joined, "findmnt --verify") {
		t.Fatalf("fstab 改动缺少备份或校验：\n%s", joined)
	}
	if !plan.Expect.WantPersistent || plan.Expect.WantMounted {
		t.Fatalf("复核条件错误：%+v", plan.Expect)
	}
	// /mnt/nas 此刻还挂着，摘要必须点明重启后它不会自己回来。
	if !strings.Contains(plan.Summary, "重启后不会自动挂载") {
		t.Fatalf("摘要缺少提醒：%s", plan.Summary)
	}
}

func TestPlanFstabRemoveRejections(t *testing.T) {
	snapshot := storageSnapshot(t)

	cases := []struct {
		name string
		req  StorageRequest
	}{
		{name: "挂载点为空", req: StorageRequest{Action: StorageFstabRemove}},
		{name: "fstab 里没有这个条目", req: StorageRequest{Action: StorageFstabRemove, Target: "/mnt/nothing"}},
	}

	for _, tc := range cases {
		requireRejected(t, snapshot, tc.name, tc.req)
	}
}

// ---------- 磁盘操作的复核 ----------

func TestVerifyFormatAndPartition(t *testing.T) {
	// 格式化：文件系统类型对上才算成功。
	format := &StorageExpect{Action: StorageFormat, Target: "/dev/sdc", WantFSType: "ext4"}
	if err := format.Verify(&StorageSnapshot{
		Devices: []BlockDevice{{Path: "/dev/sdc", FSType: "ext4"}},
	}); err != nil {
		t.Fatalf("类型已变成 ext4，应当算成功：%v", err)
	}
	if err := format.Verify(&StorageSnapshot{
		Devices: []BlockDevice{{Path: "/dev/sdc", FSType: ""}},
	}); err == nil {
		t.Fatal("设备上仍然没有文件系统，不该算成功")
	}

	// 删除分区：编号真的消失了才算成功。
	remove := &StorageExpect{
		Action: StoragePartDelete, Target: "/dev/sdb", WantPartDelta: -1, PartNumber: 1,
	}
	if err := remove.Verify(&StorageSnapshot{
		Devices: []BlockDevice{{Path: "/dev/sdb", Children: []BlockDevice{}}},
	}); err != nil {
		t.Fatalf("分区已消失，应当算成功：%v", err)
	}
	if err := remove.Verify(&StorageSnapshot{
		Devices: []BlockDevice{
			{Path: "/dev/sdb", Children: []BlockDevice{{Path: "/dev/sdb1", PartNumber: 1}}},
		},
	}); err == nil {
		t.Fatal("分区仍然存在，不该算成功")
	}

	// 新建分区：子分区列表非空才算成功。
	create := &StorageExpect{Action: StoragePartCreate, Target: "/dev/sdc", WantPartDelta: 1}
	if err := create.Verify(&StorageSnapshot{
		Devices: []BlockDevice{{Path: "/dev/sdc", Children: []BlockDevice{}}},
	}); err == nil {
		t.Fatal("没有检测到新分区，不该算成功")
	}
	if err := create.Verify(&StorageSnapshot{
		Devices: []BlockDevice{
			{Path: "/dev/sdc", Children: []BlockDevice{{Path: "/dev/sdc1", PartNumber: 1}}},
		},
	}); err != nil {
		t.Fatalf("分区已建立，应当算成功：%v", err)
	}

	// 分区表类型不对时不算成功。
	label := &StorageExpect{Action: StoragePartLabel, Target: "/dev/sdc", WantPartTable: "gpt"}
	if err := label.Verify(&StorageSnapshot{
		Devices: []BlockDevice{{Path: "/dev/sdc", PartitionTable: "msdos"}},
	}); err == nil {
		t.Fatal("分区表类型不符，不该算成功")
	}
}
