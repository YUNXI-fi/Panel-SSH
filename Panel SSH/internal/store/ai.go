package store

import (
	"database/sql"
	"errors"
	"time"
)

// AISession 是助手的一个会话：一段彼此独立的对话。
//
// 会话本身就是隔离边界 —— 历史、工具调用结果、上下文压缩后的摘要都挂在
// 它下面，不同会话之间不共享任何一份。
type AISession struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	// CreatedAt / UpdatedAt 都是 Unix 秒；UpdatedAt 用于会话列表的排序，
	// 最近聊过的排在最前面。
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// AIConversation 是某个会话的对话存档。
type AIConversation struct {
	// HistoryJSON 是 []ai.Message 的 JSON，也就是模型实际看到的上下文。
	HistoryJSON string
	// ToolStatusJSON 是 tool_call_id → 是否成功，形如 {"call_abc":true}。
	// 模型历史里没有这个信息，它存在的唯一目的是让界面回看历史时能分清
	// 一次命令执行是成功还是失败。
	ToolStatusJSON string
	// UpdatedAt 是最后一次写入的时间（Unix 秒），0 表示这个会话还没有过对话。
	UpdatedAt int64
}

const aiSessionColumns = `id, title, created_at, updated_at`

func scanAISession(scanner rowScanner) (*AISession, error) {
	var (
		session   AISession
		createdAt int64
		updatedAt int64
	)
	if err := scanner.Scan(&session.ID, &session.Title, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	session.CreatedAt = time.Unix(createdAt, 0)
	session.UpdatedAt = time.Unix(updatedAt, 0)
	return &session, nil
}

// ListAISessions 按最近活动时间倒序返回全部会话。
func (s *Store) ListAISessions() ([]AISession, error) {
	rows, err := s.db.Query(
		`SELECT ` + aiSessionColumns + ` FROM ai_sessions ORDER BY updated_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sessions := make([]AISession, 0)
	for rows.Next() {
		session, err := scanAISession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, *session)
	}
	return sessions, rows.Err()
}

// CreateAISession 新建一个空会话。
//
// 标题留空是正常的：它要等用户真正问了第一句话才由 SaveAIConversation
// 补上。界面对空标题显示"新对话"。
func (s *Store) CreateAISession(title string) (*AISession, error) {
	now := time.Now().Unix()
	res, err := s.db.Exec(
		`INSERT INTO ai_sessions(title, created_at, updated_at) VALUES(?, ?, ?)`,
		title, now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &AISession{
		ID:        id,
		Title:     title,
		CreatedAt: time.Unix(now, 0),
		UpdatedAt: time.Unix(now, 0),
	}, nil
}

// GetAISession 按主键查询会话，不存在时返回 ErrNotFound。
func (s *Store) GetAISession(id int64) (*AISession, error) {
	return scanAISession(s.db.QueryRow(
		`SELECT `+aiSessionColumns+` FROM ai_sessions WHERE id = ?`, id))
}

// DeleteAISession 删除会话及其对话记录。
//
// 对话行由外键的 ON DELETE CASCADE 一并删除：两者必须同时消失，
// 否则残留的历史会在主键被复用时跑到别的会话里。
func (s *Store) DeleteAISession(id int64) error {
	res, err := s.db.Exec(`DELETE FROM ai_sessions WHERE id = ?`, id)
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

// GetAIConversation 读取某个会话的对话记录。
//
// 会话存在但还没聊过时返回零值而不是 ErrNotFound："还没聊过"是正常状态；
// 而会话本身不存在时返回 ErrNotFound —— 这两件事必须区分，否则界面会把
// "会话被删了"显示成一屏空白，用户接着提问才发现不对。
func (s *Store) GetAIConversation(sessionID int64) (*AIConversation, error) {
	row := s.db.QueryRow(
		`SELECT history_json, tool_status_json, updated_at FROM ai_conversation WHERE session_id = ?`,
		sessionID,
	)
	var conv AIConversation
	if err := row.Scan(&conv.HistoryJSON, &conv.ToolStatusJSON, &conv.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if _, err := s.GetAISession(sessionID); err != nil {
				return nil, err
			}
			return &AIConversation{ToolStatusJSON: "{}"}, nil
		}
		return nil, err
	}
	return &conv, nil
}

// SaveAIConversation 整份覆盖写入某个会话的对话记录，并顺带维护会话标题。
//
// 标题只在为空时补写：它取的是用户第一句话，后续再聊也不必每次重算；
// 用户手动清空会话后标题会被重置，于是下一次提问会重新生成。
//
// 会话不存在时返回 ErrNotFound，而不是悄悄插入一条孤立的历史：
// 那种数据没有任何入口能再读到，等于凭空占了磁盘。
func (s *Store) SaveAIConversation(sessionID int64, historyJSON, toolStatusJSON, title string) error {
	now := time.Now().Unix()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(
		`UPDATE ai_sessions SET
		   title = CASE WHEN title = '' AND ? <> '' THEN ? ELSE title END,
		   updated_at = ?
		 WHERE id = ?`,
		title, title, now, sessionID)
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
		`INSERT INTO ai_conversation(session_id, history_json, tool_status_json, updated_at)
		 VALUES(?, ?, ?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET
		   history_json = excluded.history_json,
		   tool_status_json = excluded.tool_status_json,
		   updated_at = excluded.updated_at`,
		sessionID, historyJSON, toolStatusJSON, now,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearAIConversation 清空某个会话的对话记录，即界面上的"清空对话"。
//
// 会话本身保留：用户要的是"这个话题重新开始"，不是"把这个会话从列表里删掉"。
// 标题一并重置，否则列表里会留着一个与空会话对不上的旧标题。
func (s *Store) ClearAIConversation(sessionID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`UPDATE ai_sessions SET title = '' WHERE id = ?`, sessionID)
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

	if _, err := tx.Exec(`DELETE FROM ai_conversation WHERE session_id = ?`, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}
