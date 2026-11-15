// 环境管理操作（env_ops / env_op_steps）的持久化。
//
// 一次"环境操作"对应一台主机上的一次变更事务：预检 → 快照 → 逐步骤执行 →
// 失败补偿 / 版本复核。步骤输出在本表保留，因此操作天然可追溯、可复盘。
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// 环境操作的动作类型。
const (
	EnvActionInstall  = "install"
	EnvActionUpgrade  = "upgrade"
	EnvActionRemove   = "remove"
	EnvActionMirror   = "mirror"
	EnvActionRollback = "rollback"
)

// 环境操作状态。
const (
	EnvStatusRunning     = "running"
	EnvStatusSuccess     = "success"
	EnvStatusFailed      = "failed"
	EnvStatusInterrupted = "interrupted"
)

// 单个步骤状态。
const (
	EnvStepRunning = "running"
	EnvStepSuccess = "success"
	EnvStepFailed  = "failed"
	EnvStepSkipped = "skipped"
)

// EnvOp 是一次环境变更操作。
type EnvOp struct {
	ID          int64           `json:"id"`
	HostID      int64           `json:"hostId"`
	HostName    string          `json:"hostName"`
	Action      string          `json:"action"`
	Target      string          `json:"target"`
	Plan        json.RawMessage `json:"plan"`
	Status      string          `json:"status"`
	Compensated bool            `json:"compensated"`
	Verified    bool            `json:"verified"`
	VerifyNote  string          `json:"verifyNote"`
	OriginOpID  int64           `json:"originOpId"`
	Summary     string          `json:"summary"`
	CreatedAt   time.Time       `json:"createdAt"`
	FinishedAt  time.Time       `json:"finishedAt"`
	Steps       []EnvOpStep     `json:"steps,omitempty"`
	SnapshotDir string          `json:"-"`
}

// EnvOpStep 是操作中的一个执行步骤。
//
// Stdout/Stderr 由调用方在写入前截断；这里不再做长度保护，
// 避免每个写入点重复同一段裁剪逻辑却各写各的上限。
type EnvOpStep struct {
	Seq        int    `json:"seq"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	ExitCode   int    `json:"exitCode"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
	DurationMS int64  `json:"durationMs"`
}

// CreateEnvOp 登记一次即将开始的环境操作，返回操作 ID。
func (s *Store) CreateEnvOp(hostID int64, hostName, action, target, planJSON, snapshotDir string, originOpID int64) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO env_ops(host_id, host_name, action, target, plan_json, status, snapshot_dir, origin_op_id, created_at)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		hostID, hostName, action, target, planJSON, EnvStatusRunning, snapshotDir, originOpID, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetEnvOpSnapshotDir 登记操作对应的快照目录（op 先建、快照后拍）。
func (s *Store) SetEnvOpSnapshotDir(id int64, dir string) error {
	_, err := s.db.Exec(`UPDATE env_ops SET snapshot_dir = ? WHERE id = ?`, dir, id)
	return err
}

// StartEnvOpStep 把一个步骤标记为执行中。
func (s *Store) StartEnvOpStep(opID int64, seq int, name string) error {
	_, err := s.db.Exec(`
		INSERT INTO env_op_steps(op_id, seq, name, status)
		VALUES(?,?,?,?)`,
		opID, seq, name, EnvStepRunning)
	return err
}

// FinishEnvOpStep 收尾一个步骤并写入结果。
func (s *Store) FinishEnvOpStep(opID int64, seq int, status string, exitCode int, stdout, stderr string, durationMS int64) error {
	_, err := s.db.Exec(`
		UPDATE env_op_steps
		SET status = ?, exit_code = ?, stdout = ?, stderr = ?, duration_ms = ?
		WHERE op_id = ? AND seq = ?`,
		status, exitCode, stdout, stderr, durationMS, opID, seq)
	return err
}

// EnvOpResult 是收尾一次操作所需的结果字段。
type EnvOpResult struct {
	Status      string
	Compensated bool
	Verified    bool
	VerifyNote  string
	Summary     string
}

// FinishEnvOp 给一次操作收尾。
func (s *Store) FinishEnvOp(id int64, r EnvOpResult) error {
	_, err := s.db.Exec(`
		UPDATE env_ops
		SET status = ?, compensated = ?, verified = ?, verify_note = ?, summary = ?, finished_at = ?
		WHERE id = ?`,
		r.Status, boolToInt(r.Compensated), boolToInt(r.Verified), r.VerifyNote, r.Summary, time.Now().Unix(), id)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// interruptRunningEnvOps 把上次进程退出时残留的 running 操作标记为 interrupted。
//
// 与批量任务同理：环境操作只活在发起它的 WebSocket 连接里，进程重启后不可能
// 还有操作在跑。注意目标机上的命令可能确实执行了一半 —— 是否需要补偿由用户
// 看到 interrupted 状态后手动发起回滚决定，启动时不擅自连回主机做动作。
func (s *Store) interruptRunningEnvOps() error {
	_, err := s.db.Exec(`UPDATE env_ops SET status = ?, finished_at = ? WHERE status = ?`,
		EnvStatusInterrupted, time.Now().Unix(), EnvStatusRunning)
	return err
}

// ListEnvOps 按时间倒序返回环境操作。
// hostID 小于 0 时不限主机。
func (s *Store) ListEnvOps(hostID int64, limit int) ([]EnvOp, error) {
	var rows *sql.Rows
	var err error
	if hostID >= 0 {
		rows, err = s.db.Query(`
			SELECT id, host_id, host_name, action, target, plan_json, status, compensated,
			       verified, verify_note, snapshot_dir, origin_op_id, summary, created_at, finished_at
			FROM env_ops WHERE host_id = ? ORDER BY id DESC LIMIT ?`, hostID, limit)
	} else {
		rows, err = s.db.Query(`
			SELECT id, host_id, host_name, action, target, plan_json, status, compensated,
			       verified, verify_note, snapshot_dir, origin_op_id, summary, created_at, finished_at
			FROM env_ops ORDER BY id DESC LIMIT ?`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEnvOps(rows)
}

// GetEnvOp 返回单个操作及其全部步骤。
func (s *Store) GetEnvOp(id int64) (*EnvOp, error) {
	op, err := scanEnvOp(s.db.QueryRow(`
		SELECT id, host_id, host_name, action, target, plan_json, status, compensated,
		       verified, verify_note, snapshot_dir, origin_op_id, summary, created_at, finished_at
		FROM env_ops WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}

	rows, err := s.db.Query(`
		SELECT seq, name, status, exit_code, stdout, stderr, duration_ms
		FROM env_op_steps WHERE op_id = ? ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	op.Steps = make([]EnvOpStep, 0)
	for rows.Next() {
		var step EnvOpStep
		if err := rows.Scan(&step.Seq, &step.Name, &step.Status, &step.ExitCode,
			&step.Stdout, &step.Stderr, &step.DurationMS); err != nil {
			return nil, err
		}
		op.Steps = append(op.Steps, step)
	}
	return op, rows.Err()
}

type envRowScanner interface {
	Scan(dest ...any) error
}

func scanEnvOp(row envRowScanner) (*EnvOp, error) {
	var (
		op          EnvOp
		planJSON    string
		compensated int
		verified    int
		created     int64
		finished    int64
	)
	if err := row.Scan(&op.ID, &op.HostID, &op.HostName, &op.Action, &op.Target, &planJSON,
		&op.Status, &compensated, &verified, &op.VerifyNote, &op.SnapshotDir,
		&op.OriginOpID, &op.Summary, &created, &finished); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	op.Plan = json.RawMessage(planJSON)
	if len(planJSON) == 0 {
		op.Plan = json.RawMessage("null")
	}
	op.Compensated = compensated != 0
	op.Verified = verified != 0
	op.CreatedAt = time.Unix(created, 0)
	if finished > 0 {
		op.FinishedAt = time.Unix(finished, 0)
	}
	return &op, nil
}

func scanEnvOps(rows *sql.Rows) ([]EnvOp, error) {
	ops := make([]EnvOp, 0)
	for rows.Next() {
		op, err := scanEnvOp(rows)
		if err != nil {
			return nil, err
		}
		ops = append(ops, *op)
	}
	return ops, rows.Err()
}

// LaunchApp 是一条"已安装且可以打开"的目录软件记录。
type LaunchApp struct {
	HostID      int64
	Slug        string
	InstalledAt time.Time
}

// RecordLaunchApp 登记某台主机上的可启动应用；重复登记只刷新时间。
func (s *Store) RecordLaunchApp(hostID int64, slug string) error {
	_, err := s.db.Exec(`
		INSERT INTO launch_apps(host_id, slug, installed_at) VALUES(?,?,?)
		ON CONFLICT(host_id, slug) DO UPDATE SET installed_at = excluded.installed_at`,
		hostID, slug, time.Now().Unix())
	return err
}

// RemoveLaunchApp 删除某台主机上的可启动应用记录（卸载或探测不到时调用）。
func (s *Store) RemoveLaunchApp(hostID int64, slug string) error {
	_, err := s.db.Exec(`DELETE FROM launch_apps WHERE host_id = ? AND slug = ?`, hostID, slug)
	return err
}

// RemoveLaunchAppBySlug 删除所有主机上的同一款应用入口。
// 应用包是面板级的一份，卸载后各处图标都应消失。
func (s *Store) RemoveLaunchAppBySlug(slug string) error {
	_, err := s.db.Exec(`DELETE FROM launch_apps WHERE slug = ?`, slug)
	return err
}

// ListLaunchApps 返回全部主机上的可启动应用记录，按主机与安装时间排序。
func (s *Store) ListLaunchApps() ([]LaunchApp, error) {
	rows, err := s.db.Query(`
		SELECT host_id, slug, installed_at FROM launch_apps
		ORDER BY host_id, installed_at DESC, slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	apps := make([]LaunchApp, 0)
	for rows.Next() {
		var item LaunchApp
		var installed int64
		if err := rows.Scan(&item.HostID, &item.Slug, &installed); err != nil {
			return nil, err
		}
		item.InstalledAt = time.Unix(installed, 0)
		apps = append(apps, item)
	}
	return apps, rows.Err()
}
