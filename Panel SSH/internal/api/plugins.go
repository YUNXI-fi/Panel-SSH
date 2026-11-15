package api

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"panelssh/internal/plugin"
	"panelssh/internal/store"
)

// pluginSDK 是注入到插件沙箱内的前端 SDK。
//
//go:embed assets/sdk.js
var pluginSDK embed.FS

// maxPluginArchive 限制安装包体积。
const maxPluginArchive = 32 << 20

// 常见类型显式给定，避免依赖宿主机的 mime 配置。
var pluginContentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".map":   "application/json; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".webp":  "image/webp",
	".ico":   "image/x-icon",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".txt":   "text/plain; charset=utf-8",
}

// capabilityCatalog 返回能力的界面文案与风险等级。
//
// 顺序由 plugin.CapabilityOrder 决定（风险升序），前端照单排版即可，
// 不必自己再定义一份顺序 —— 两份顺序迟早会不一致。
func capabilityCatalog() []map[string]string {
	items := make([]map[string]string, 0, len(plugin.CapabilityOrder))
	for _, name := range plugin.CapabilityOrder {
		items = append(items, map[string]string{
			"name":  name,
			"label": plugin.CapabilityLabel[name],
			"risk":  plugin.CapabilityRisk[name],
		})
	}
	return items
}

// builtinView 是内置插件在应用中心里的展示项。
type builtinView struct {
	Org         string `json:"org"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Version     string `json:"version"`
	URL         string `json:"url"`
	Installed   bool   `json:"installed"`
}

// pluginView 是插件记录在列表接口里的展示形态。
//
// 比 store.Plugin 多一个 hostName：同名插件装在两台机器上是两条记录，
// 界面上必须能分辨哪条是哪台，而"主机名"这个信息在插件表里没有，
// 由这里随列表一起给出，省得前端再拉一次主机列表自己去对。
type pluginView struct {
	store.Plugin
	HostName string `json:"hostName"`
}

func (s *Server) handleListPlugins(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListPlugins()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取插件列表失败")
		return
	}

	// 内置插件"是否已安装"要按主机分别判断：同一台主机上装过就算装过。
	installed := make(map[string]bool, len(items))
	views := make([]pluginView, 0, len(items))
	for _, item := range items {
		installed[item.Org+"/"+item.Name+"@"+fmt.Sprint(item.HostID)] = true
		views = append(views, pluginView{
			Plugin:   builtinEntryNormalized(item),
			HostName: s.hostLabel(item.HostID),
		})
	}

	builtins := make([]builtinView, 0, len(plugin.Builtins))
	for _, item := range plugin.Builtins {
		builtins = append(builtins, builtinView{
			Org:         item.Org,
			Name:        item.Name,
			DisplayName: item.DisplayName,
			Description: item.Description,
			Version:     item.Version,
			URL:         item.URL,
			Installed:   installed[item.ID()+"@"+fmt.Sprint(LocalHostID)],
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"plugins":      views,
		"builtins":     builtins,
		"capabilities": capabilityCatalog(),
	})
}

// builtinEntry 给出内置插件在某台主机上的入口地址。
//
// 只有 DSH 工作台需要按主机区分：它每台主机一个实例，入口必须带主机号
// （/dsh/7/）。其余内置插件仍用清单里声明的地址。
//
// 入口形态只在这里定义一次：前端因此不必知道任何插件的 org/name，
// 换个内置插件也不必去前端改一处硬编码。
func builtinEntry(builtin plugin.Builtin, hostID int64) string {
	if builtin.ID() == plugin.BuiltinDshWorkspaceID {
		return dshPrefixFor(hostID) + "/"
	}
	return builtin.URL
}

// builtinEntryNormalized 修正内置插件记录里的入口地址。
//
// 为什么读取时现算而不是只靠写入时写对：改造前存下的记录里 Entry 是 /dsh/，
// 那是一个"没有主机号"的地址。读的时候现算，这些历史记录不需要数据迁移
// ——迁移漏掉一台，用户看到的就是一个打不开的图标，而且没有任何线索。
func builtinEntryNormalized(item store.Plugin) store.Plugin {
	if item.Source != plugin.SourceEmbedded {
		return item
	}
	// 认不出的内置插件保持原样：它可能是更新版本的面板才认识的条目。
	if builtin, ok := plugin.FindBuiltin(item.Org, item.Name); ok {
		item.Entry = builtinEntry(builtin, item.HostID)
	}
	return item
}

// handleInstallBuiltin 安装一个内置插件。
//
// 内置插件不带静态文件，它的能力由面板自己实现，因此这里只写一条记录：
// 记录存在与否就是"用户是否选择启用它"的唯一依据。
func (s *Server) handleInstallBuiltin(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}

	builtin, ok := plugin.FindBuiltin(r.PathValue("org"), r.PathValue("name"))
	if !ok {
		writeErr(w, http.StatusNotFound, "没有这个内置插件")
		return
	}

	input := store.PluginInput{
		Org:         builtin.Org,
		Name:        builtin.Name,
		DisplayName: builtin.DisplayName,
		Description: builtin.Description,
		Version:     builtin.Version,
		// 内置插件的 entry 存的是面板内的入口地址，而不是包内文件路径。
		Entry:  builtinEntry(builtin, hostID),
		Source: plugin.SourceEmbedded,
		HostID: hostID,
	}
	preserveExistingCapabilities(s.store, &input)

	item, err := s.store.UpsertPlugin(input)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "写入插件记录失败")
		return
	}

	s.notifySecurity(store.LevelInfo, "安装内置插件："+item.DisplayName,
		fmt.Sprintf("%s/%s · 版本 %s · %s", item.Org, item.Name, item.Version, s.hostLabel(item.HostID)))

	writeJSON(w, http.StatusCreated, item)
}

// hostLabel 把主机 ID 转成通知里可读的目标描述。
func (s *Server) hostLabel(hostID int64) string {
	if hostID == LocalHostID {
		return "宿主机"
	}
	host, err := s.store.GetHost(hostID)
	if err != nil {
		return fmt.Sprintf("主机 #%d", hostID)
	}
	return host.Name
}

// pluginCapabilityNote 把能力清单拼成一行说明，空清单给一句明确的文案。
//
// "授权为空"与"没有这条通知"是两回事：前者是管理员主动回收了全部权限，
// 属于要留痕的操作。
func pluginCapabilityNote(granted []string) string {
	if len(granted) == 0 {
		return "当前未授予任何能力"
	}
	return "已授予：" + strings.Join(granted, "、")
}

// preserveExistingCapabilities 在重复安装内置插件时保留已有的能力与授权。
//
// 为什么需要它：UpsertPlugin 会把 capabilities / granted 两列一起覆盖。
// 内置插件不带能力清单，如果直接 upsert 一条空记录，用户此前逐项确认过的授权
// 就会因为"又点了一次安装"而静默消失。插件未申请能力时这两列本来就该保持原样。
func preserveExistingCapabilities(st *store.Store, input *store.PluginInput) {
	existing, err := st.GetPlugin(input.Org, input.Name, input.HostID)
	if err != nil {
		return // 首次安装，没有需要保留的东西
	}
	input.Capabilities = existing.Capabilities
	input.Granted = existing.Granted
}

// pathHostID 读取请求路径里的 {id}（主机 ID）。
//
// 宿主机是 0，是合法值，因此不能用 pathID 那套"必须为正整数"的校验。
func (s *Server) pathHostID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 0 {
		writeErr(w, http.StatusBadRequest, "主机 ID 非法")
		return 0, false
	}
	if id != LocalHostID {
		if _, err := s.store.GetHost(id); err != nil {
			writeErr(w, http.StatusNotFound, "主机不存在")
			return 0, false
		}
	}
	return id, true
}

// lookupPlugin 解析路径中的 {org}/{name} 与 {id} 并查出插件。
func (s *Server) lookupPlugin(w http.ResponseWriter, r *http.Request) (*store.Plugin, bool) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return nil, false
	}

	item, err := s.store.GetPlugin(r.PathValue("org"), r.PathValue("name"), hostID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "插件不存在")
		return nil, false
	}
	return item, true
}

func (s *Server) handleInstallPlugin(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxPluginArchive)

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "安装包读取失败，或超过体积上限")
		return
	}
	if len(raw) == 0 {
		writeErr(w, http.StatusBadRequest, "安装包为空")
		return
	}

	// 解压到暂存区，此时不触碰任何已安装版本。
	staged, err := plugin.Stage(s.pluginsDir, bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	manifest := staged.Manifest

	if hostID != LocalHostID {
		home, err := s.remotePluginEnv(hostID).HomeDir(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "读取目标主机家目录失败："+err.Error())
			return
		}
		log.Printf("[debug] 插件安装：目标 hostID=%d 家目录=%q entry=%q version=%q",
			hostID, home, manifest.Entry, manifest.Version)
	}

	requested := r.URL.Query().Get("capabilities")
	granted, err := parseRequestedCapabilities(requested, manifest.Capabilities)
	if err != nil {
		staged.Discard()
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	previous, prevErr := s.store.GetPlugin(manifest.Org, manifest.Name, hostID)
	if strings.TrimSpace(requested) == "" {
		if prevErr == nil {
			// 升级：只保留"此前已授予 且 新版本仍然申请"的能力。
			// 新增的权限申请必须由管理员显式确认，避免插件靠一次升级偷偷扩权。
			granted = intersectCapabilities(previous.Granted, manifest.Capabilities)
		} else {
			// 默认拒绝：没显式授权就什么都不给。
			granted = []string{}
		}
	}

	// 先写库、再落位文件。
	//
	// 两步之间的失败都要能恢复：
	// - 写库失败：暂存内容直接丢掉，已安装的版本毫发无损。
	// - 落位失败：尽量把库记录改回原样，避免出现"库里有记录、磁盘上没文件"。
	// 反过来（先落位文件再写库）在写库失败时会删掉旧版本，插件会彻底打不开。
	item, err := s.store.UpsertPlugin(store.PluginInput{
		Org:          manifest.Org,
		Name:         manifest.Name,
		DisplayName:  manifest.DisplayName,
		Description:  manifest.Description,
		Version:      manifest.Version,
		Entry:        manifest.Entry,
		Icon:         manifest.Icon,
		Capabilities: manifest.Capabilities,
		Granted:      granted,
		HostID:       hostID,
	})
	if err != nil {
		staged.Discard()
		writeErr(w, http.StatusInternalServerError, "写入插件记录失败")
		return
	}

	if err := staged.Commit(); err != nil {
		staged.Discard()
		if prevErr == nil {
			if _, restoreErr := s.store.UpsertPlugin(store.PluginInput{
				Org:          previous.Org,
				Name:         previous.Name,
				DisplayName:  previous.DisplayName,
				Description:  previous.Description,
				Version:      previous.Version,
				Entry:        previous.Entry,
				Icon:         previous.Icon,
				Source:       previous.Source,
				Capabilities: previous.Capabilities,
				Granted:      previous.Granted,
				HostID:       previous.HostID,
			}); restoreErr != nil {
				writeErr(w, http.StatusInternalServerError,
					"安装包落位失败，且原记录未能恢复："+err.Error()+"；"+restoreErr.Error())
				return
			}
		} else {
			_ = s.store.DeletePlugin(manifest.Org, manifest.Name, hostID)
		}
		writeErr(w, http.StatusInternalServerError, "安装包落位失败："+err.Error())
		return
	}

	// 远端主机还要把同一份包装过去落位：面板本地这份供 iframe 托管界面，
	// 远端那份是插件真正操作的目标机上的一份副本。
	if hostID != LocalHostID {
		if err := plugin.PushToRemote(r.Context(), s.remotePluginEnv(hostID), raw, manifest); err != nil {
			// 远端落位失败就整体回滚：本地文件与库记录都不留，避免出现
			// "面板以为装好了、目标机上什么都没有"的假成功。
			s.rollbackPluginInstall(previous, prevErr, manifest, hostID)
			s.writeHostError(w, err, "插件推送到目标主机失败")
			return
		}
	}

	// 安装第三方插件是引入外部代码的动作，用 warning 而不是 info：
	// 它比"安装内置插件"更需要被注意到。
	s.notifySecurity(store.LevelWarning, "安装第三方插件："+item.DisplayName,
		fmt.Sprintf("%s/%s · 版本 %s · %s · 申请能力 %d 项",
			item.Org, item.Name, item.Version, s.hostLabel(item.HostID), len(item.Capabilities)))

	writeJSON(w, http.StatusCreated, item)
}

// rollbackPluginInstall 在远端落位失败后把本地状态复原。
//
// 卸载（uninstall）与"本次是首次安装"两种情况共用同一份收尾：
// 没有旧记录就直接删掉刚写的那条并把本地文件清掉，有旧记录就写回去。
func (s *Server) rollbackPluginInstall(previous *store.Plugin, prevErr error, manifest *plugin.Manifest, hostID int64) {
	if prevErr == nil {
		if _, err := s.store.UpsertPlugin(store.PluginInput{
			Org:          previous.Org,
			Name:         previous.Name,
			DisplayName:  previous.DisplayName,
			Description:  previous.Description,
			Version:      previous.Version,
			Entry:        previous.Entry,
			Icon:         previous.Icon,
			Source:       previous.Source,
			Capabilities: previous.Capabilities,
			Granted:      previous.Granted,
			HostID:       previous.HostID,
		}); err == nil {
			return
		}
	}
	_ = s.store.DeletePlugin(manifest.Org, manifest.Name, hostID)
	_ = plugin.Uninstall(s.pluginsDir, manifest.Org, manifest.Name)
}

func (s *Server) handleUninstallPlugin(w http.ResponseWriter, r *http.Request) {
	item, ok := s.lookupPlugin(w, r)
	if !ok {
		return
	}

	// 远端优先：目标机上那份是这个插件被安装的理由。
	//
	// 如果先删本地再删远端，远端删不掉时用户会看到一个"列表里已经没有它、
	// 目标机上却还留着一整个目录"的中间状态，而且界面里再也没有入口去收拾它。
	// 反过来则失败是干净的：什么都没动，用户可以重试。
	if item.Source != plugin.SourceEmbedded && item.HostID != LocalHostID {
		if err := plugin.UninstallRemote(r.Context(), s.remotePluginEnv(item.HostID), item.Org, item.Name); err != nil {
			s.writeHostError(w, err, "删除目标主机上的插件失败")
			return
		}
	}

	// 先停进程、关隧道，再动文件与记录（顺序见设计文档 5.5）。
	// 反过来会留下一个还在跑、而面板已经管不到的插件进程，以及一条没人认领的隧道。
	s.pluginTunnels.closePlugin(item.Org, item.Name)
	if err := s.runtimes.Remove(item.Org, item.Name); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 内置插件没有磁盘文件，只有一条记录；卸载的真正含义是回收它占用的资源。
	if item.Source != plugin.SourceEmbedded {
		if err := plugin.Uninstall(s.pluginsDir, item.Org, item.Name); err != nil {
			writeErr(w, http.StatusInternalServerError, "删除插件文件失败："+err.Error())
			return
		}
	} else if item.Org+"/"+item.Name == plugin.BuiltinDshWorkspaceID {
		// dsh 常驻数百 MB，卸载后必须立刻停掉，否则用户以为已经关掉了它。
		// 连同这台主机上的启动命令一起清掉：留着的话，重新安装时界面看起来
		// 已经配好，实际拉起的却可能是一个已经被删掉的可执行文件。
		if err := s.dsh.Forget(item.HostID); err != nil {
			writeErr(w, http.StatusInternalServerError, "清理 dsh 启动命令失败")
			return
		}
		// 不关这台主机的常驻通道：通道由 dsh 反代、浏览器反代、SOCKS 代理共用，
		// 别处可能正在用它。没人用之后由通道自己的空闲回收收掉（见 hostTunnels）。
	}

	if err := s.store.DeletePlugin(item.Org, item.Name, item.HostID); err != nil {
		writeErr(w, http.StatusInternalServerError, "删除插件记录失败")
		return
	}

	s.notifySecurity(store.LevelWarning, "卸载插件："+item.DisplayName,
		fmt.Sprintf("%s/%s 已从 %s 移除", item.Org, item.Name, s.hostLabel(item.HostID)))

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSetPluginEnabled(w http.ResponseWriter, r *http.Request) {
	item, ok := s.lookupPlugin(w, r)
	if !ok {
		return
	}

	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeJSONBody(w, r, &req, 1<<16); err != nil {
		return
	}

	if err := s.store.SetPluginEnabled(item.ID, req.Enabled); err != nil {
		writeErr(w, http.StatusInternalServerError, "更新插件状态失败")
		return
	}

	// 停用即回收：进程和隧道都不该留着 —— 一个"已停用"的插件还连着目标机的
	// 数据库、还占着一条转发通道，与"停用"这个词自相矛盾。
	if !req.Enabled {
		s.pluginTunnels.closePlugin(item.Org, item.Name)
		s.runtimes.Stop(item.Org, item.Name)
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": req.Enabled})
}

func (s *Server) handleSetPluginCapabilities(w http.ResponseWriter, r *http.Request) {
	item, ok := s.lookupPlugin(w, r)
	if !ok {
		return
	}

	var req struct {
		Granted []string `json:"granted"`
	}
	if err := decodeJSONBody(w, r, &req, 1<<16); err != nil {
		return
	}

	for _, name := range req.Granted {
		if !plugin.IsKnownCapability(name) {
			writeErr(w, http.StatusBadRequest, "未知能力："+name)
			return
		}
		if !contains(item.Capabilities, name) {
			// 只能授予插件清单里申请过的能力，不能凭空加权限。
			writeErr(w, http.StatusBadRequest, "插件未申请该能力，无法授予："+name)
			return
		}
	}

	if err := s.store.SetPluginGranted(item.ID, req.Granted); err != nil {
		writeErr(w, http.StatusInternalServerError, "更新授权失败")
		return
	}

	updated, err := s.store.GetPlugin(item.Org, item.Name, item.HostID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取插件失败")
		return
	}

	// 权限授予是这套插件体系里最需要留痕的动作：插件能做什么，
	// 完全由这里决定。单独提升到 warning，让它不被其他管理通知淹没。
	s.notifySecurity(store.LevelWarning, "变更插件授权："+updated.DisplayName,
		fmt.Sprintf("%s/%s · %s", updated.Org, updated.Name, pluginCapabilityNote(updated.Granted)))

	writeJSON(w, http.StatusOK, updated)
}

// handlePluginSDK 提供沙箱内使用的前端 SDK。
//
// 该资源必须免鉴权：插件运行在 opaque origin 的沙箱里，
// 其子资源请求不携带会话 Cookie。SDK 本身不含任何用户数据。
func (s *Server) handlePluginSDK(w http.ResponseWriter, r *http.Request) {
	content, err := pluginSDK.ReadFile("assets/sdk.js")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "SDK 不可用")
		return
	}

	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(content)
}

// handlePluginAsset 托管插件的静态资源。
//
// 同样免鉴权，原因与 SDK 一致（沙箱内子资源不带 Cookie）。
// 插件前端代码本身不含用户数据；所有敏感操作都要经过能力桥，
// 由主应用带会话身份代为执行。
//
// 这里读的一律是**面板本地的那份镜像**，与插件装在哪台主机上无关：
// 一个插件同时装在多台主机上时，各台机器上的那份是逐字节相同的同一个包，
// 版本也由同一条记录跟着，因此没必要（也没法）按主机分路径 —— 这个接口
// 免鉴权，路径里再带主机维度只会多出一处需要校验的可控输入。
//
// 与此对应，"启停"的判定必须覆盖该插件的全部安装：只要还有一台主机上
// 它是启用的，本地镜像就要继续提供资源，否则停用一台主机会让另一台上的
// 插件窗口白屏。
func (s *Server) handlePluginAsset(w http.ResponseWriter, r *http.Request) {
	org, name := r.PathValue("org"), r.PathValue("name")

	items, err := s.store.ListPluginsByName(org, name)
	if err != nil || len(items) == 0 {
		writeErr(w, http.StatusNotFound, "插件不存在")
		return
	}

	// 各主机上的版本可能不同（在一台上升级过、另一台还没升）。
	// 本地镜像只留最新落位的那一份，因此这里也取最靠后的那条记录来定位目录，
	// 否则会出现"库里版本是 2.0.0、磁盘上却是 1.0.0"的错配。
	latest := items[0]
	enabled := false
	for _, item := range items {
		if item.Enabled {
			enabled = true
		}
		if item.InstalledAt.After(latest.InstalledAt) {
			latest = item
		}
	}
	if !enabled {
		writeErr(w, http.StatusForbidden, "插件已停用")
		return
	}

	root := filepath.Join(s.pluginsDir, latest.Org, latest.Name, latest.Version)
	target, err := plugin.ResolveAsset(root, r.PathValue("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	file, err := os.Open(target)
	if err != nil {
		writeErr(w, http.StatusNotFound, "资源不存在")
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || info.IsDir() {
		writeErr(w, http.StatusNotFound, "资源不存在")
		return
	}

	if contentType, ok := pluginContentTypes[strings.ToLower(filepath.Ext(target))]; ok {
		w.Header().Set("Content-Type", contentType)
	}
	// 插件是第三方代码，禁止浏览器去嗅探类型。
	w.Header().Set("X-Content-Type-Options", "nosniff")

	// 这里必须用 ServeContent 而不是 ServeFile：后者会把任何以 "/index.html"
	// 结尾的请求 307 重定向到所在目录，而插件入口默认就叫 index.html，
	// 结果就是默认入口的插件永远打不开。路径安全已由 ResolveAsset 负责，
	// ServeFile 内建的 ".." 检查在这里是多余且有害的。
	http.ServeContent(w, r, filepath.Base(target), info.ModTime(), file)
}

// parseRequestedCapabilities 解析调用方显式给出的授权列表。
// 只能授予清单里声明过的能力，不允许凭空扩权。
func parseRequestedCapabilities(raw string, declared []string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return []string{}, nil
	}

	granted := make([]string, 0)
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !plugin.IsKnownCapability(name) {
			return nil, errors.New("未知能力：" + name)
		}
		if !contains(declared, name) {
			return nil, errors.New("插件未申请该能力：" + name)
		}
		granted = append(granted, name)
	}
	return granted, nil
}

// intersectCapabilities 求两个集合的交集，保持 order 的顺序。
func intersectCapabilities(previous, order []string) []string {
	out := make([]string, 0, len(order))
	for _, name := range order {
		if contains(previous, name) {
			out = append(out, name)
		}
	}
	return out
}

func contains(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}
