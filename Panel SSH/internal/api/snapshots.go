package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"panelssh/internal/env"
	"panelssh/internal/store"
)

// 本文件是「操作快照」模块的后端：把环境变更留下的快照汇总成跨主机的视图。
//
// 快照本身不进任何数据表 —— 它只是 env Runner 落在数据目录下的一组文件
// （packages.tsv / sources.tgz），由 env_ops.snapshot_dir 指过去。因此这里
// 只做读取与统计，不新增存储：列表读 env_ops，内容读快照目录。
//
// 两个刻意的取舍：
//   - EnvOp.SnapshotDir 带 json:"-"（它是服务端磁盘路径，不该下发），快照的
//     存在性/体积/包数一律经 snapshotView 这几个派生字段暴露。
//   - 解析 packages.tsv 必须借目标机的包管理器：apt 的行是
//     `Status-Abbrev\tName\tVersion\tArch`、rpm 是三列、apk 是整行文本，
//     列格式互不兼容。拿不到系统画像时降级为原文展示，保证历史永远读得到，
//     而不是因为"这台机器连不上了"就看不到当时装了什么。

// maxSnapshotList 是跨主机操作列表一次返回的最大条数。
// 与任务记录同理：面板是单机工具，最近两百次变更足够回看。
const maxSnapshotList = 200

// maxSnapshotPackages 限制单次返回的包条数。
//
// 大包清单（几万个包）整份回给前端要几 MB JSON，还会渲染出几万个 DOM 节点，
// 而用户翻这份清单的目的通常只是"确认某个包在不在"。差异计算仍走全量、
// 不受这个上限影响 —— 差异才是这个窗口的主要信息。
const maxSnapshotPackages = 2000

// snapshotView 是列表里的一条操作记录，附带快照落盘情况。
type snapshotView struct {
	store.EnvOp

	// HasSnapshot 为假而 Pruned 为真表示快照采集过、后来被保留策略清理了。
	// 这件事必须在界面上说出来，否则用户会以为面板弄丢了数据。
	HasSnapshot   bool  `json:"hasSnapshot"`
	Pruned        bool  `json:"pruned"`
	SnapshotBytes int64 `json:"snapshotBytes"`
	PackageCount  int   `json:"packageCount"`
}

// snapshotDetailView 是一条操作的快照内容。
type snapshotDetailView struct {
	Op        *store.EnvOp  `json:"op"`
	Available bool          `json:"available"`
	Reason    string        `json:"reason,omitempty"`
	Note      string        `json:"note,omitempty"`
	Parsed    bool          `json:"parsed"`
	Family    env.Family    `json:"family,omitempty"`
	Packages  []env.Package `json:"packages"`
	Total     int           `json:"packageTotal"`
	Truncated bool          `json:"truncated"`
	Raw       string        `json:"raw,omitempty"`
	Sources   int64         `json:"sourcesBytes"`
	Diff      *snapshotDiff `json:"diff,omitempty"`
}

// snapshotDiff 是"与同主机上一次快照相比"的包差异。
type snapshotDiff struct {
	BaseOpID int64           `json:"baseOpId"`
	BaseAt   time.Time       `json:"baseAt"`
	Added    []env.Package   `json:"added"`
	Removed  []env.Package   `json:"removed"`
	Changed  []packageChange `json:"changed"`
}

// packageChange 是同名包版本变化的记录。
type packageChange struct {
	Name string `json:"name"`
	From string `json:"from"`
	To   string `json:"to"`
}

// handleSnapshotList 返回全部主机的操作记录及各自的快照状态。
func (s *Server) handleSnapshotList(w http.ResponseWriter, r *http.Request) {
	// hostID 传 -1：ListEnvOps 对负值不加主机过滤，这正是"跨机总览"要的。
	ops, err := s.store.ListEnvOps(-1, maxSnapshotList)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询操作快照失败")
		return
	}

	views := make([]snapshotView, 0, len(ops))
	for i := range ops {
		views = append(views, newSnapshotView(&ops[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ops": views})
}

// handleSnapshotDetail 返回一条操作的快照内容与包差异。
func (s *Server) handleSnapshotDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("opId"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "操作 ID 无效")
		return
	}

	op, err := s.store.GetEnvOp(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "操作不存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, "查询操作失败")
		return
	}

	view := snapshotDetailView{Op: op, Packages: []env.Package{}}
	if !dirExists(op.SnapshotDir) {
		view.Reason = fmt.Sprintf(
			"这次操作的快照已不在磁盘上：每台主机只保留最近 %d 次变更的快照，更早的会被自动清理。操作记录与步骤日志本身仍然完整。",
			env.SnapshotKeep)
		writeJSON(w, http.StatusOK, view)
		return
	}
	view.Available = true

	if fi, err := os.Stat(filepath.Join(op.SnapshotDir, "sources.tgz")); err == nil && !fi.IsDir() {
		view.Sources = fi.Size()
	}

	mgr, family := s.snapshotManager(r.Context(), op.HostID)
	if mgr == nil {
		fillRawSnapshot(&view, op.SnapshotDir,
			"读不到这台主机的系统画像（主机可能已删除或暂时连不上），下面直接给出快照里的包清单原文。")
		writeJSON(w, http.StatusOK, view)
		return
	}

	snap, lerr := env.LoadSnapshot(op.SnapshotDir, mgr)
	if lerr != nil {
		fillRawSnapshot(&view, op.SnapshotDir, "包清单解析失败，已降级为原文展示："+lerr.Error())
		writeJSON(w, http.StatusOK, view)
		return
	}

	view.Parsed = true
	view.Family = family
	view.Total = len(snap.Packages)
	if len(snap.Packages) > maxSnapshotPackages {
		view.Packages = snap.Packages[:maxSnapshotPackages]
		view.Truncated = true
	} else {
		view.Packages = snap.Packages
	}
	view.Diff = s.snapshotDiff(op, mgr)

	writeJSON(w, http.StatusOK, view)
}

// fillRawSnapshot 在解析不了包清单时退回原文，让历史仍然可读。
func fillRawSnapshot(view *snapshotDetailView, dir, note string) {
	raw, err := os.ReadFile(filepath.Join(dir, "packages.tsv"))
	if err != nil {
		view.Note = "快照里的包清单读取失败：" + err.Error()
		return
	}
	view.Raw = string(raw)
	view.Total = countLines(view.Raw)
	view.Note = note
}

// snapshotManager 取目标机的包管理器，用于解析快照里的包清单。
//
// 走 hostProfile 的缓存（与提权决策共用同一份画像），因此打开快照详情
// 通常不会额外连一次目标机；确实过期才会重探，代价可接受。
func (s *Server) snapshotManager(ctx context.Context, hostID int64) (env.PackageManager, env.Family) {
	profile, err := s.hostProfile(ctx, hostID, false)
	if err != nil {
		return nil, ""
	}
	mgr := env.NewManager(profile)
	if mgr == nil {
		return nil, ""
	}
	return mgr, profile.Family
}

// snapshotDiff 找同主机上一条快照并算出包差异。
//
// 基准取"id 更小、快照还在"的最近一条：被清理掉的快照没法当基准，
// 跳过它是唯一正确的做法（否则差异会退化成"整机都变了"）。
func (s *Server) snapshotDiff(op *store.EnvOp, mgr env.PackageManager) *snapshotDiff {
	ops, err := s.store.ListEnvOps(op.HostID, 100)
	if err != nil {
		return nil
	}

	var base *store.EnvOp
	for i := range ops {
		// ListEnvOps 按 id 倒序，因此第一个满足条件的就是"上一次"。
		if ops[i].ID < op.ID && dirExists(ops[i].SnapshotDir) {
			base = &ops[i]
			break
		}
	}
	if base == nil {
		return nil
	}

	cur, err := env.LoadSnapshot(op.SnapshotDir, mgr)
	if err != nil {
		return nil
	}
	prev, err := env.LoadSnapshot(base.SnapshotDir, mgr)
	if err != nil {
		return nil
	}
	return diffPackages(base, prev.Packages, cur.Packages)
}

// diffPackages 比对两份包清单。与 store / 磁盘无关，便于单测。
func diffPackages(base *store.EnvOp, before, after []env.Package) *snapshotDiff {
	prev := make(map[string]string, len(before))
	for _, p := range before {
		prev[p.Name] = p.Version
	}
	cur := make(map[string]string, len(after))
	for _, p := range after {
		cur[p.Name] = p.Version
	}

	diff := &snapshotDiff{
		BaseOpID: base.ID,
		BaseAt:   base.CreatedAt,
		Added:    []env.Package{},
		Removed:  []env.Package{},
		Changed:  []packageChange{},
	}
	for _, p := range after {
		old, ok := prev[p.Name]
		switch {
		case !ok:
			diff.Added = append(diff.Added, p)
		case old != p.Version:
			diff.Changed = append(diff.Changed, packageChange{Name: p.Name, From: old, To: p.Version})
		}
	}
	for _, p := range before {
		if _, ok := cur[p.Name]; !ok {
			diff.Removed = append(diff.Removed, p)
		}
	}

	// 包管理器给的是自己的输出顺序，这里统一按包名排序，界面才能稳定展示。
	sort.Slice(diff.Added, func(i, j int) bool { return diff.Added[i].Name < diff.Added[j].Name })
	sort.Slice(diff.Removed, func(i, j int) bool { return diff.Removed[i].Name < diff.Removed[j].Name })
	sort.Slice(diff.Changed, func(i, j int) bool { return diff.Changed[i].Name < diff.Changed[j].Name })
	return diff
}

// newSnapshotView 统计一条操作对应的快照落盘情况。
func newSnapshotView(op *store.EnvOp) snapshotView {
	view := snapshotView{EnvOp: *op}

	stat, ok := readSnapshotDir(op.SnapshotDir)
	if ok {
		view.HasSnapshot = true
		view.SnapshotBytes = stat.bytes
		view.PackageCount = stat.packages
		return view
	}

	// 目录不在。只有"登记过路径、后来被清理"才算 pruned；快照采集就失败的操作
	// snapshot_dir 为空，那是操作自己的失败，不该说成"快照被清理"。
	view.Pruned = op.SnapshotDir != "" && op.Status != store.EnvStatusRunning
	return view
}

type snapshotStat struct {
	bytes    int64
	packages int
}

// readSnapshotDir 汇总快照目录的大小与包数；目录或两份材料都不在时返回 false。
func readSnapshotDir(dir string) (snapshotStat, bool) {
	if dir == "" {
		return snapshotStat{}, false
	}

	stat := snapshotStat{}
	found := false
	for _, name := range []string{"packages.tsv", "sources.tgz"} {
		if fi, err := os.Stat(filepath.Join(dir, name)); err == nil && !fi.IsDir() {
			stat.bytes += fi.Size()
			found = true
		}
	}
	if !found {
		return snapshotStat{}, false
	}

	if raw, err := os.ReadFile(filepath.Join(dir, "packages.tsv")); err == nil {
		stat.packages = countLines(string(raw))
	}
	return stat, true
}

func dirExists(dir string) bool {
	if dir == "" {
		return false
	}
	fi, err := os.Stat(dir)
	return err == nil && fi.IsDir()
}

// countLines 数非空行。包清单末尾的空行不算一个包。
func countLines(raw string) int {
	count := 0
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}
