package sysinfo

import (
	"strings"
	"testing"
	"time"
)

// sampleStorageOutput 仿照一台装了系统盘 + 数据盘 + 一块未挂载 USB 盘的机器。
// 覆盖几个关键分支：swap 分区、被过滤的伪文件系统、已在 fstab 中的网络挂载、
// 以及"整盘没有挂载点但分区挂着 /"这种必须靠递归才能判对的系统盘。
const sampleStorageOutput = `###STOTOOLS
blkid
findmnt
lsblk
mkfs.ext4
parted
smartctl
###LSBLK
{"blockdevices":[
{"name":"nvme0n1","path":"/dev/nvme0n1","type":"disk","size":256060514304,"fstype":null,"label":null,"uuid":null,"mountpoints":[],"ro":false,"rm":false,"model":"SAMSUNG MZVLB256HBHQ","serial":"S4GGNX0N","tran":"nvme","rota":false,"pttype":"gpt",
 "children":[
  {"name":"nvme0n1p1","path":"/dev/nvme0n1p1","type":"part","size":1023410176,"fstype":"vfat","label":null,"uuid":"4B47-BA37","mountpoints":["/boot/efi"],"ro":false,"rm":false,"model":null,"serial":null,"tran":"nvme","rota":false,"pttype":"gpt","partn":1,"start":2048},
  {"name":"nvme0n1p2","path":"/dev/nvme0n1p2","type":"part","size":241684062208,"fstype":"ext4","label":null,"uuid":"e63dc214-cbb3-4215-80e0-8f18ff176ae7","mountpoints":["/"],"ro":false,"rm":false,"model":null,"serial":null,"tran":"nvme","rota":false,"pttype":"gpt","partn":2,"start":2002048},
  {"name":"nvme0n1p3","path":"/dev/nvme0n1p3","type":"part","size":13630406656,"fstype":"swap","label":null,"uuid":"2ccf9525-4f11-49c0-976a-4510effea28a","mountpoints":["[SWAP]"],"ro":false,"rm":false,"model":null,"serial":null,"tran":"nvme","rota":false,"pttype":"gpt","partn":3,"start":474318848}
 ]},
{"name":"nvme1n1","path":"/dev/nvme1n1","type":"disk","size":256060514304,"fstype":null,"label":null,"uuid":null,"mountpoints":[],"ro":false,"rm":false,"model":"UMIS RPJTJ256MEE1OWX","serial":"SS0L25189","tran":"nvme","rota":false,"pttype":"gpt",
 "children":[
  {"name":"nvme1n1p1","path":"/dev/nvme1n1p1","type":"part","size":256059113472,"fstype":"exfat","label":null,"uuid":"FE73-D3B5","mountpoints":["/media/yunxi/FE73-D3B5"],"ro":false,"rm":false,"model":null,"serial":null,"tran":"nvme","rota":false,"pttype":"gpt","partn":1,"start":2048}
 ]},
{"name":"sdb","path":"/dev/sdb","type":"disk","size":1000204886016,"fstype":null,"label":null,"uuid":null,"mountpoints":[],"ro":false,"rm":true,"model":"External USB 3.0","serial":"20240101000000","tran":"usb","rota":true,"pttype":"dos",
 "children":[
  {"name":"sdb1","path":"/dev/sdb1","type":"part","size":1000203091968,"fstype":"ext4","label":"backup","uuid":"aaaa1111-bbbb-2222-cccc-333344445555","mountpoints":[],"ro":false,"rm":true,"model":null,"serial":null,"tran":"usb","rota":true,"pttype":"dos","partn":1,"start":2048}
 ]},
{"name":"sdc","path":"/dev/sdc","type":"disk","size":500107862016,"fstype":null,"label":null,"uuid":null,"mountpoints":[],"ro":false,"rm":false,"model":"WDC WD5000LPCX","serial":"WD-WX11A11","tran":"sata","rota":true,"pttype":null,
 "children":[]}
]}
###FINDMNT
{"filesystems":[
{"target":"/","source":"/dev/nvme0n1p2","fstype":"ext4","options":"rw,relatime,errors=remount-ro","size":241684062208,"used":62822936576,"avail":166509740032,"use%":"26%","uuid":"e63dc214-cbb3-4215-80e0-8f18ff176ae7","label":null,
 "children":[
  {"target":"/sys","source":"sysfs","fstype":"sysfs","options":"rw,nosuid","size":0,"used":0,"avail":0,"use%":"-","uuid":null,"label":null,
   "children":[
    {"target":"/sys/fs/cgroup","source":"cgroup2","fstype":"cgroup2","options":"rw","size":0,"used":0,"avail":0,"use%":"-","uuid":null,"label":null}
   ]}
 ]},
{"target":"/boot/efi","source":"/dev/nvme0n1p1","fstype":"vfat","options":"rw,relatime","size":1023410176,"used":21839872,"avail":999592960,"use%":"3%","uuid":"4B47-BA37","label":null},
{"target":"/media/yunxi/FE73-D3B5","source":"/dev/nvme1n1p1","fstype":"exfat","options":"rw,nosuid,nodev,relatime","size":256059113472,"used":39500940288,"avail":216558173184,"use%":"16%","uuid":"FE73-D3B5","label":null},
{"target":"/mnt/nas","source":"192.168.1.5:/export/data","fstype":"nfs4","options":"rw,relatime,vers=4.2","size":0,"used":0,"avail":0,"use%":"-","uuid":null,"label":null}
]}
###FSTAB
# /etc/fstab: static file system information.
#
UUID=e63dc214-cbb3-4215-80e0-8f18ff176ae7 / ext4 errors=remount-ro 0 1
UUID=4B47-BA37  /boot/efi	 vfat	 umask=0077	 0	 1
# swap was on /dev/nvme0n1p3 during installation
UUID=2ccf9525-4f11-49c0-976a-4510effea28a none swap sw 0 0
192.168.1.5:/export/data /mnt/nas nfs4 defaults,nofail,_netdev 0 0
###MDSTAT
Personalities : [raid1]
md0 : active raid1 sdc1[0] sdd1[1]
      1048512 blocks super 1.2 [2/2] [UU]

###NFSUTIL
nfs
cifs
`

func storageSnapshot(t *testing.T) *StorageSnapshot {
	t.Helper()
	snapshot, err := CollectStorage(sampleStorageOutput, time.Now())
	if err != nil {
		t.Fatalf("解析存储样例失败：%v", err)
	}
	return snapshot
}

func findTestDevice(t *testing.T, snapshot *StorageSnapshot, path string) *BlockDevice {
	t.Helper()
	device := findDevice(snapshot.Devices, path)
	if device == nil {
		t.Fatalf("样例里没有设备 %s", path)
	}
	return device
}

func findTestMount(t *testing.T, snapshot *StorageSnapshot, target string) *Mount {
	t.Helper()
	for i := range snapshot.Mounts {
		if snapshot.Mounts[i].Target == target {
			return &snapshot.Mounts[i]
		}
	}
	t.Fatalf("样例里没有挂载点 %s", target)
	return nil
}

func TestCollectStorageDevices(t *testing.T) {
	snapshot := storageSnapshot(t)

	// 系统盘：它自己没有任何挂载点，只有分区挂着 / —— 必须递归到子级才能判定。
	system := findTestDevice(t, snapshot, "/dev/nvme0n1")
	if !system.System {
		t.Fatal("挂着 / 的盘应被标记为系统设备")
	}
	if !system.Busy {
		t.Fatal("有分区挂载中的盘应被标记为使用中")
	}
	if system.Model != "SAMSUNG MZVLB256HBHQ" || system.Transport != "nvme" {
		t.Fatalf("整盘信息缺失：%+v", system)
	}

	// swap 分区没有任何挂载点，但它同样属于系统的一部分。
	swap := findTestDevice(t, snapshot, "/dev/nvme0n1p3")
	if !swap.System {
		t.Fatal("swap 分区应被标记为系统设备")
	}

	// 外接 USB 盘：未挂载、可移除、非系统盘 —— 正是可以挂载/格式化的对象。
	usb := findTestDevice(t, snapshot, "/dev/sdb")
	if usb.System || usb.Busy {
		t.Fatalf("未挂载的外接盘不应被标记为系统或使用中：%+v", usb)
	}
	if !usb.Removable || usb.Transport != "usb" {
		t.Fatalf("外接盘属性缺失：%+v", usb)
	}

	usbPart := findTestDevice(t, snapshot, "/dev/sdb1")
	if usbPart.FSType != "ext4" || usbPart.Label != "backup" {
		t.Fatalf("分区信息错误：%+v", usbPart)
	}
	if len(usbPart.MountPoints) != 0 {
		t.Fatalf("未挂载的分区不应有挂载点：%+v", usbPart.MountPoints)
	}
}

func TestCollectStorageMounts(t *testing.T) {
	snapshot := storageSnapshot(t)

	// 伪文件系统整棵子树都要被剪掉：sysfs 与它下面的 cgroup2 都不该出现，
	// 否则一台普通机器上会列出几十个与存储无关的挂载点。
	for _, mount := range snapshot.Mounts {
		if mount.FSType == "sysfs" || mount.FSType == "cgroup2" {
			t.Fatalf("伪文件系统 %s 不应出现在挂载列表里", mount.Target)
		}
	}

	root := findTestMount(t, snapshot, "/")
	if root.UsePercent != 26 || root.Total != 241684062208 {
		t.Fatalf("根挂载点解析错误：%+v", root)
	}
	if !root.Persistent {
		t.Fatal("/ 在 fstab 里，应标记为持久挂载")
	}
	if root.Kind != "disk" {
		t.Fatalf("本地盘的 Kind 应为 disk，实际 %s", root.Kind)
	}

	// 数据盘没写进 fstab：这正是用户最常见的困惑"为什么重启后不见了"。
	data := findTestMount(t, snapshot, "/media/yunxi/FE73-D3B5")
	if data.Persistent {
		t.Fatal("不在 fstab 里的挂载点不该被标记为持久")
	}

	// 网络挂载的两个特征：Kind 为 network，use% 为 "-" 时不能当成 0%。
	nas := findTestMount(t, snapshot, "/mnt/nas")
	if nas.Kind != "network" {
		t.Fatalf("NFS 挂载的 Kind 应为 network，实际 %s", nas.Kind)
	}
	if nas.UsePercent != -1 {
		t.Fatalf("没有容量概念时 UsePercent 应为 -1，实际 %v", nas.UsePercent)
	}
	if !nas.Persistent {
		t.Fatal("fstab 里有 /mnt/nas 的条目")
	}
}

func TestCollectStorageFstabAndMisc(t *testing.T) {
	snapshot := storageSnapshot(t)

	// 注释行与空行都要跳过，只留 4 条真实条目。
	if len(snapshot.Fstab) != 4 {
		t.Fatalf("fstab 条目数量错误：%d", len(snapshot.Fstab))
	}
	// 制表符分隔的条目同样要能解析。
	var efi *FstabEntry
	for i := range snapshot.Fstab {
		if snapshot.Fstab[i].Target == "/boot/efi" {
			efi = &snapshot.Fstab[i]
		}
	}
	if efi == nil || efi.FSType != "vfat" || efi.Options != "umask=0077" {
		t.Fatalf("制表符分隔的 fstab 条目解析错误：%+v", efi)
	}

	if len(snapshot.Raid) != 1 {
		t.Fatalf("mdstat 解析错误：%v", snapshot.Raid)
	}
	raid := snapshot.Raid[0]
	if raid.Name != "md0" || raid.Level != "raid1" || raid.State != "active" {
		t.Fatalf("阵列头部解析错误：%+v", raid)
	}
	// 成员计数写在缩进的那一行里，只解析头部会把它整个丢掉 ——
	// 而"少没少成员"正是这个面板最该报出来的事。
	if raid.Total != 2 || raid.Working != 2 || !raid.Healthy || raid.Degraded {
		t.Fatalf("阵列成员状态解析错误：%+v", raid)
	}
	if len(raid.Members) != 2 || raid.Members[0] != "sdc1" || raid.Members[1] != "sdd1" {
		t.Fatalf("阵列成员解析错误：%v", raid.Members)
	}
	if raid.Blocks != 1048512 {
		t.Fatalf("阵列容量解析错误：%d", raid.Blocks)
	}

	// 工具列表要把网络存储客户端的探测结果并进来，否则挂载 NFS/SMB
	// 会被误判成"目标主机没装客户端"。
	want := []string{"blkid", "cifs", "findmnt", "lsblk", "mkfs.ext4", "nfs", "parted", "smartctl"}
	if len(snapshot.Tools) != len(want) {
		t.Fatalf("工具列表错误：%v", snapshot.Tools)
	}
	for i := range want {
		if snapshot.Tools[i] != want[i] {
			t.Fatalf("工具列表错误：%v", snapshot.Tools)
		}
	}
}

func TestPlanMountLocal(t *testing.T) {
	snapshot := storageSnapshot(t)

	plan, err := PlanStorageChange(StorageRequest{
		Action:  StorageMountLocal,
		Device:  "/dev/sdb1",
		Target:  "/mnt/backup",
		Persist: true,
	}, snapshot)
	if err != nil {
		t.Fatalf("生成计划失败：%v", err)
	}

	if len(plan.Prepare) != 1 || !strings.Contains(plan.Prepare[0], "mkdir -p -- '/mnt/backup'") {
		t.Fatalf("准备步骤错误：%v", plan.Prepare)
	}
	if plan.Mount != "mount -t 'ext4' -o 'defaults' -- '/dev/sdb1' '/mnt/backup'" {
		t.Fatalf("挂载命令错误：%s", plan.Mount)
	}

	// 持久化必须用 UUID：设备名会随插拔顺序变化，写 /dev/sdb1 迟早挂错盘。
	persist := strings.Join(plan.Persist, "\n")
	if !strings.Contains(persist, "UUID=aaaa1111-bbbb-2222-cccc-333344445555 /mnt/backup ext4 defaults,nofail 0 2") {
		t.Fatalf("fstab 条目错误：\n%s", persist)
	}
	if !strings.Contains(persist, "cp -a /etc/fstab") || !strings.Contains(persist, "findmnt --verify") {
		t.Fatalf("fstab 写入缺少备份或校验：\n%s", persist)
	}
	if plan.Expect.WantPersistent != true {
		t.Fatal("持久化时应当校验 fstab 条目")
	}
}

func TestPlanMountRejections(t *testing.T) {
	snapshot := storageSnapshot(t)

	cases := []struct {
		name string
		req  StorageRequest
	}{
		{
			name: "挂载点是根目录",
			req:  StorageRequest{Action: StorageMountLocal, Device: "/dev/sdb1", Target: "/"},
		},
		{
			name: "挂载点是系统目录",
			req:  StorageRequest{Action: StorageMountLocal, Device: "/dev/sdb1", Target: "/etc"},
		},
		{
			name: "挂载点不是绝对路径",
			req:  StorageRequest{Action: StorageMountLocal, Device: "/dev/sdb1", Target: "mnt/backup"},
		},
		{
			name: "挂载点含命令分隔符",
			req:  StorageRequest{Action: StorageMountLocal, Device: "/dev/sdb1", Target: "/mnt/x; reboot"},
		},
		{
			name: "挂载点里带空格",
			req:  StorageRequest{Action: StorageMountLocal, Device: "/dev/sdb1", Target: "/mnt/my disk"},
		},
		{
			name: "目标是系统盘",
			req:  StorageRequest{Action: StorageMountLocal, Device: "/dev/nvme0n1p2", Target: "/mnt/x"},
		},
		{
			name: "目标已经挂载",
			req:  StorageRequest{Action: StorageMountLocal, Device: "/dev/nvme1n1p1", Target: "/mnt/x"},
		},
		{
			name: "设备不存在",
			req:  StorageRequest{Action: StorageMountLocal, Device: "/dev/sdz1", Target: "/mnt/x"},
		},
		{
			name: "挂载点已被占用",
			req:  StorageRequest{Action: StorageMountLocal, Device: "/dev/sdb1", Target: "/mnt/nas"},
		},
		{
			name: "设备还没有文件系统",
			req:  StorageRequest{Action: StorageMountLocal, Device: "/dev/sdb", Target: "/mnt/x"},
		},
		{
			name: "挂载选项不合法",
			req: StorageRequest{
				Action: StorageMountLocal, Device: "/dev/sdb1", Target: "/mnt/x",
				Options: []string{"defaults; reboot"},
			},
		},
		{
			name: "卸载一个没挂载的路径",
			req:  StorageRequest{Action: StorageUnmount, Target: "/mnt/nothing"},
		},
		{
			name: "卸载系统挂载点",
			req:  StorageRequest{Action: StorageUnmount, Target: "/"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := PlanStorageChange(tc.req, snapshot); err == nil {
				t.Fatal("应当被拒绝，实际通过了校验")
			}
		})
	}
}

func TestPlanMountNetwork(t *testing.T) {
	snapshot := storageSnapshot(t)

	nfs, err := PlanStorageChange(StorageRequest{
		Action:  StorageMountNFS,
		Server:  "192.168.1.5",
		Share:   "/export/media",
		Target:  "/mnt/media",
		Persist: true,
	}, snapshot)
	if err != nil {
		t.Fatalf("NFS 计划生成失败：%v", err)
	}
	if !strings.Contains(nfs.Mount, "'192.168.1.5:/export/media'") || !strings.Contains(nfs.Mount, "-t nfs") {
		t.Fatalf("NFS 挂载命令错误：%s", nfs.Mount)
	}
	// 网络存储的 fstab 条目必须带 _netdev 与 nofail：少了它们，一次网络故障
	// 就会让机器卡在启动过程中。
	persist := strings.Join(nfs.Persist, "\n")
	if !strings.Contains(persist, "nofail,_netdev") {
		t.Fatalf("NFS fstab 条目缺少 nofail/_netdev：\n%s", persist)
	}

	cifs, err := PlanStorageChange(StorageRequest{
		Action:   StorageMountCIFS,
		Server:   "nas.local",
		Share:    "share1",
		Target:   "/mnt/share1",
		Username: "alice",
		Password: "s3cret",
	}, snapshot)
	if err != nil {
		t.Fatalf("CIFS 计划生成失败：%v", err)
	}
	if cifs.CredentialPath != "/etc/panelssh-cifs-nas.local-share1.cred" {
		t.Fatalf("凭据文件路径错误：%s", cifs.CredentialPath)
	}
	// 密码绝不能出现在挂载命令里：那会被目标机上任何用户从 ps 里看到。
	if strings.Contains(cifs.Mount, "s3cret") {
		t.Fatalf("挂载命令里出现了明文密码：%s", cifs.Mount)
	}
	if !strings.Contains(strings.Join(cifs.Prepare, "\n"), "password=s3cret") {
		t.Fatal("凭据文件内容不正确")
	}
	// 不持久化时凭据文件应当在成功后被清理。
	if len(cifs.Finish) != 1 || !strings.Contains(cifs.Finish[0], cifs.CredentialPath) {
		t.Fatalf("一次性凭据文件未被清理：%v", cifs.Finish)
	}

	// 密码里带换行可以凭空往凭据文件里插入一行（比如再写一个 domain=）。
	if _, err := PlanStorageChange(StorageRequest{
		Action: StorageMountCIFS, Server: "nas.local", Share: "share1", Target: "/mnt/share1",
		Username: "alice", Password: "x\ndomain=evil",
	}, snapshot); err == nil {
		t.Fatal("凭据里含换行时必须拒绝")
	}

	// 缺少客户端工具时要直接说清楚，而不是让 mount 抛一句难懂的错误。
	bare := storageSnapshot(t)
	bare.Tools = []string{"lsblk"}
	if _, err := PlanStorageChange(StorageRequest{
		Action: StorageMountNFS, Server: "192.168.1.5", Share: "/export", Target: "/mnt/x",
	}, bare); err == nil {
		t.Fatal("缺少 mount.nfs 时应当拒绝")
	}
}

func TestPlanUnmountRemovesFstabEntry(t *testing.T) {
	plan, err := PlanStorageChange(StorageRequest{
		Action: StorageUnmount,
		Target: "/mnt/nas",
	}, storageSnapshot(t))
	if err != nil {
		t.Fatalf("生成计划失败：%v", err)
	}

	if plan.Mount != "umount -- '/mnt/nas'" {
		t.Fatalf("卸载命令错误：%s", plan.Mount)
	}
	// 只卸载不删 fstab 条目的话，重启后它会自己冒回来，用户无从理解。
	persist := strings.Join(plan.Persist, "\n")
	if !strings.Contains(persist, "awk -v t='/mnt/nas'") {
		t.Fatalf("fstab 条目未被移除：\n%s", persist)
	}
	if plan.Expect.WantMounted || !plan.Expect.WantPersistent {
		t.Fatalf("预期状态错误：%+v", plan.Expect)
	}
}

func TestStorageExpectVerify(t *testing.T) {
	expect := &StorageExpect{Action: StorageMountLocal, Target: "/mnt/backup", WantMounted: true, WantPersistent: true}

	ok := &StorageSnapshot{Mounts: []Mount{{Target: "/mnt/backup", Persistent: true}}}
	if err := expect.Verify(ok); err != nil {
		t.Fatalf("已挂载且已持久化却判定失败：%v", err)
	}

	// 挂上了但 fstab 条目没生效 —— 这是"重启后不见了"的成因，必须暴露出来。
	notPersistent := &StorageSnapshot{Mounts: []Mount{{Target: "/mnt/backup"}}}
	if err := expect.Verify(notPersistent); err == nil {
		t.Fatal("fstab 条目缺失时应判定为失败")
	}

	missing := &StorageSnapshot{}
	if err := expect.Verify(missing); err == nil {
		t.Fatal("没有挂载时应判定为失败")
	}

	unmount := &StorageExpect{Action: StorageUnmount, Target: "/mnt/nas", WantMounted: false, WantPersistent: true}
	stillThere := &StorageSnapshot{
		Mounts: []Mount{{Target: "/mnt/nas"}},
		Fstab:  []FstabEntry{{Target: "/mnt/nas"}},
	}
	if err := unmount.Verify(stillThere); err == nil {
		t.Fatal("条目仍留在 fstab 时应判定为失败")
	}
	if err := unmount.Verify(&StorageSnapshot{}); err != nil {
		t.Fatalf("已彻底卸载却判定失败：%v", err)
	}
}
