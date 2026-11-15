package api

import (
	"encoding/json"
	"net/http"

	"panelssh/internal/auth"
)

// desktopFolder 是桌面上的一个文件夹（分组容器）。
type desktopFolder struct {
	ID   string   `json:"id"`
	Name string   `json:"name"`
	Apps []string `json:"apps"`
}

// desktopLayout 是桌面布局。
//
// 只记"被移出桌面的图标"（hidden）和文件夹结构，不记完整可见列表：
// 插件是动态的，新装的插件默认就该出现在桌面上；若存可见列表，
// 每装一个新插件都得先手工把它加进去，用户会看到"装了却不在桌面"。
type desktopLayout struct {
	Hidden  []string        `json:"hidden"`
	Folders []desktopFolder `json:"folders"`
}

// emptyDesktopLayout 返回默认布局：全部图标可见、没有文件夹。
func emptyDesktopLayout() desktopLayout {
	return desktopLayout{Hidden: []string{}, Folders: []desktopFolder{}}
}

// handleGetDesktopLayout 返回当前用户的桌面布局。
func (s *Server) handleGetDesktopLayout(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserFrom(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "未登录")
		return
	}

	raw, err := s.store.GetDesktopLayout(userID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取桌面布局失败")
		return
	}

	layout := emptyDesktopLayout()
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &layout); err != nil {
			// 库里的 JSON 坏掉时退回默认布局，而不是让用户永远打不开桌面。
			layout = emptyDesktopLayout()
		}
	}
	// 空数组必须序列化成 []，不能是 null：前端直接遍历这两个字段。
	if layout.Hidden == nil {
		layout.Hidden = []string{}
	}
	if layout.Folders == nil {
		layout.Folders = []desktopFolder{}
	}
	writeJSON(w, http.StatusOK, layout)
}

// handleSetDesktopLayout 覆盖保存当前用户的桌面布局。
func (s *Server) handleSetDesktopLayout(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserFrom(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "未登录")
		return
	}

	var layout desktopLayout
	if err := decodeJSONBody(w, r, &layout, 1<<16); err != nil {
		return
	}
	if layout.Hidden == nil {
		layout.Hidden = []string{}
	}
	if layout.Folders == nil {
		layout.Folders = []desktopFolder{}
	}

	raw, err := json.Marshal(layout)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存桌面布局失败")
		return
	}
	if err := s.store.SetDesktopLayout(userID, string(raw)); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存桌面布局失败")
		return
	}
	writeJSON(w, http.StatusOK, layout)
}
