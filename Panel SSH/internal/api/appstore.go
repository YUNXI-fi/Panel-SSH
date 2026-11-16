package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"panelssh/internal/appstore"
)

// 本文件是"应用中心 → 应用仓库"的接口层。
//
// 应用包来自 GitHub（或 PANELSSH_APP_SOURCE_DIR 指定的本地镜像），
// 由 appstore 负责下载、校验与落盘；这里只做鉴权、主机归属与安装记录。

type storeAppView struct {
	appstore.Entry
	Installed        bool   `json:"installed"`
	InstalledVersion string `json:"installedVersion,omitempty"`
	UpdateAvailable  bool   `json:"updateAvailable"`
	OnHost           bool   `json:"onHost"`
}

// storeSourceMetaKey 是仓库源在 meta 表里的键。
//
// 存在库里而不是配置文件：管理员在应用中心改完应立即生效，
// 且换浏览器后仍然一致；没有这个键时用内置的官方仓库。
const storeSourceMetaKey = "appstore.index_url"

// handleStoreSourceGet 返回当前仓库源与默认值，供设置弹窗回填。
func (s *Server) handleStoreSourceGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"source":  s.appPackages.IndexURL(),
		"default": appstore.DefaultIndexURL,
	})
}

// handleStoreSourceSave 切换仓库源。
//
// 先校验再试拉一次索引：源写错时立刻报错，而不是让用户回到列表
// 才发现"一个应用都没有"。
func (s *Server) handleStoreSourceSave(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Source string `json:"source"`
	}
	if err := decodeJSONBody(w, r, &body, 1<<14); err != nil {
		return
	}
	source := strings.TrimSpace(body.Source)
	if source == "" {
		writeErr(w, http.StatusBadRequest, "仓库源不能为空")
		return
	}
	if err := appstore.ValidateIndexURL(source); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	previous := s.appPackages.IndexURL()
	s.appPackages.SetIndexURL(source)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	idx, _, err := s.appPackages.Index(ctx, true)
	if err != nil {
		// 试拉失败就退回原地址，避免把面板留在一个连不上的源上。
		if appstore.ValidateIndexURL(previous) == nil {
			s.appPackages.SetIndexURL(previous)
		}
		writeErr(w, http.StatusBadGateway, "仓库源不可用："+err.Error())
		return
	}
	if err := s.store.SetMeta(storeSourceMetaKey, source); err != nil {
		writeErr(w, http.StatusInternalServerError, "仓库源已生效，但保存失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "source": source, "apps": len(idx.Apps),
	})
}

// handleStoreApps 返回应用仓库清单。
//
// refresh=1 强制重新拉取索引；拉取失败但有缓存时返回缓存，并在
// cached 标记里说明"这是离线可用的一份"。
func (s *Server) handleStoreApps(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	force := r.URL.Query().Get("refresh") == "1"
	idx, fromCache, err := s.appPackages.Index(ctx, force)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}

	hostID := LocalHostID
	if raw := r.URL.Query().Get("hostId"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "hostId 不合法")
			return
		}
		hostID = id
	}
	onHost := map[string]bool{}
	if rows, err := s.store.ListLaunchApps(); err == nil {
		for _, row := range rows {
			if row.HostID == hostID {
				onHost[row.Slug] = true
			}
		}
	}

	views := make([]storeAppView, 0, len(idx.Apps))
	for _, entry := range idx.Apps {
		installed := s.appPackages.InstalledVersion(entry.Slug)
		views = append(views, storeAppView{
			Entry:            entry,
			Installed:        installed != "",
			InstalledVersion: installed,
			UpdateAvailable:  installed != "" && appstore.VersionGreater(entry.Version, installed),
			OnHost:           onHost[entry.Slug],
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"source":    s.appPackages.IndexURL(),
		"fetchedAt": idx.GeneratedAt,
		"cached":    fromCache,
		"apps":      views,
	})
}

// handleStoreInstall 安装或更新一个应用包，并把入口放到指定主机的桌面。
func (s *Server) handleStoreInstall(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	var body struct {
		HostID int64 `json:"hostId"`
	}
	if err := decodeJSONBody(w, r, &body, 1<<16); err != nil {
		return
	}
	if body.HostID != LocalHostID {
		if _, err := s.store.GetHost(body.HostID); err != nil {
			writeErr(w, http.StatusNotFound, "主机不存在或已被删除")
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	manifest, _, err := s.appPackages.Install(ctx, slug)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "安装应用失败："+err.Error())
		return
	}
	if err := s.store.RecordLaunchApp(body.HostID, manifest.Slug); err != nil {
		writeErr(w, http.StatusInternalServerError, "应用已安装，但桌面入口保存失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "slug": manifest.Slug, "version": manifest.Version, "hostId": body.HostID,
	})
}

// handleStoreUninstall 卸载应用包，并移除所有主机上的桌面入口。
func (s *Server) handleStoreUninstall(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if err := s.appPackages.Uninstall(slug); err != nil {
		writeErr(w, http.StatusInternalServerError, "卸载应用失败："+err.Error())
		return
	}
	_ = s.store.RemoveLaunchAppBySlug(slug)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "slug": slug})
}

// handleStoreIcon 返回应用图标。已安装读包内文件，未安装按索引拉取并缓存。
func (s *Server) handleStoreIcon(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.PathValue("slug"))
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	raw, contentType, err := s.appPackages.Icon(ctx, slug)
	if err != nil {
		writeErr(w, http.StatusNotFound, "读取应用图标失败："+err.Error())
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}
