package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"panelssh/internal/env"
	"panelssh/internal/store"
)

// maxEnvPackages 是已安装包列表的返回上限。
// 全量 JSON 只在打开"系统包"页签时拉一次；十万级包的容器极其罕见，
// 截断比把几十兆 JSON 推给浏览器更诚实（搜索由前端在这 5000 条内完成）。
const maxEnvPackages = 5000

// localHostName 是本机目标的显示名，需与前端 watchedHost 的 LOCAL_HOST_NAME 保持一致。
const localHostName = "宿主机"

// envTarget 按主机 ID 取执行器；0 是本机，走本地进程，其余走 SSH。
func (s *Server) envTarget(hostID int64) (env.Executor, string, error) {
	if hostID == LocalHostID {
		// 配过提权账号就带上口令：env 层的 Probe 靠 SecretFeeder 判定
		// su/sudo 是否可用，Wrap 也据此决定命令怎么包。
		if account, secret, ok := s.localPrivilege(); ok {
			return env.NewLocalExecutorWithSecret(account, secret), localHostName, nil
		}
		return env.NewLocalExecutor(), localHostName, nil
	}
	host, err := s.store.GetHost(hostID)
	if err != nil {
		return nil, "", err
	}
	return &sshEnvExecutor{s: s, hostID: hostID}, host.Name, nil
}

// envDomainError 把环境模块的领域错误映射为 400；返回 false 表示交给
// 通用的主机错误映射（502/504/404）。
func envDomainError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, env.ErrNoPrivilege),
		errors.Is(err, env.ErrUnsupportedFamily),
		errors.Is(err, env.ErrInvalidPackage),
		errors.Is(err, env.ErrNoSpecs),
		errors.Is(err, env.ErrVersionUnavailable),
		errors.Is(err, env.ErrUnknownMirror):
		writeErr(w, http.StatusBadRequest, err.Error())
		return true
	}
	return false
}

// handleEnvOverview 返回系统画像与软件目录状态（打开环境管理窗口时的首屏数据）。
func (s *Server) handleEnvOverview(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	ex, _, err := s.envTarget(hostID)
	if err != nil {
		s.writeHostError(w, err, "连接目标主机失败")
		return
	}

	profile, err := env.Probe(r.Context(), ex)
	if err != nil {
		if !envDomainError(w, err) {
			s.writeHostError(w, err, "系统画像探测失败")
		}
		return
	}
	items, err := env.CatalogStatus(r.Context(), ex, profile)
	if err != nil {
		s.writeHostError(w, err, "读取软件目录状态失败")
		return
	}
	// 目录探测同时维护"可打开应用"的记录：装上的保留、探测不到的清掉，
	// 桌面图标因此不会停在过期状态。
	s.syncLaunchApps(hostID, items)
	// 提权方式由后端给出而不是让界面按 root / su 自己拼优先级：
	// 优先级是行为契约（见 env.Profile.PrivilegeMode），只该有一处定义。
	writeJSON(w, http.StatusOK, map[string]any{
		"profile": profile, "catalog": items, "privilegeMode": profile.PrivilegeMode(),
	})
}

// syncLaunchApps 把目录探测结果同步到可打开应用记录。
//
// 只处理声明了 Launch 的目录项：运行时、工具链、数据库这类纯环境软件
// 不产生桌面图标，也不在应用中心显示"打开"。
func (s *Server) syncLaunchApps(hostID int64, items []env.CatalogItemStatus) {
	for _, item := range items {
		recipe := env.GetRecipe(item.Slug)
		if recipe == nil || recipe.Launch == nil {
			continue
		}
		if item.Installed {
			_ = s.store.RecordLaunchApp(hostID, item.Slug)
		} else {
			_ = s.store.RemoveLaunchApp(hostID, item.Slug)
		}
	}
}

// launchAppView 是桌面图标与「打开」入口需要的一条数据。
type launchAppView struct {
	Slug        string   `json:"slug"`
	Name        string   `json:"name"`
	HostID      int64    `json:"hostId"`
	HostName    string   `json:"hostName"`
	Command     string   `json:"command"`
	ConfigPaths []string `json:"configPaths,omitempty"`
	// 应用仓库安装的包带自己的图标与版本；内置安全应用为空。
	IconURL  string `json:"iconUrl,omitempty"`
	Version  string `json:"version,omitempty"`
	Category string `json:"category,omitempty"`
}

// handleLaunchApps 返回全部主机上已安装、且支持在面板里打开的应用。
func (s *Server) handleLaunchApps(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListLaunchApps()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询可打开应用失败")
		return
	}

	hostNames := map[int64]string{LocalHostID: localHostName}
	views := make([]launchAppView, 0, len(rows))
	for _, row := range rows {
		recipe := env.GetRecipe(row.Slug)
		if recipe == nil {
			continue
		}
		// 目录软件自带 Launch 才能打开；此外，从应用仓库安装的应用包
		// 即使不参与目录安装，也应该出现在桌面上。
		hasPackage := false
		pkgVersion := ""
		pkgConfigs := []string(nil)
		if s.appPackages != nil {
			if manifest, _, err := s.appPackages.Installed(row.Slug); err == nil {
				hasPackage = true
				pkgVersion = manifest.Version
				for _, item := range manifest.Config {
					pkgConfigs = append(pkgConfigs, item.Path)
				}
			}
		}
		if recipe.Launch == nil && !hasPackage {
			continue
		}

		name, seen := hostNames[row.HostID]
		if !seen {
			host, err := s.store.GetHost(row.HostID)
			if err != nil {
				// 主机已删除：记录本身没有意义，跳过而不是造一个指向空气的图标。
				continue
			}
			name = host.Name
			hostNames[row.HostID] = name
		}

		view := launchAppView{
			Slug: recipe.Slug, Name: recipe.Name,
			HostID: row.HostID, HostName: name,
			Category: recipe.Category,
		}
		if recipe.Launch != nil {
			view.Command = recipe.Launch.Command
			view.ConfigPaths = recipe.Launch.ConfigPaths
		}
		if hasPackage {
			view.IconURL = "/api/store/apps/" + row.Slug + "/icon"
			view.Version = pkgVersion
			view.ConfigPaths = pkgConfigs
		}
		views = append(views, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": views})
}

// privilegeCheckResult 是提权自检的结论。
type privilegeCheckResult struct {
	OK     bool   `json:"ok"`
	Mode   string `json:"mode"`
	Detail string `json:"detail"`
}

// privilegeUIDMarker 是自检输出里的标记前缀。
//
// 与脚本共用同一个常量：两处各写一遍字符串，改一处忘一处就会静默变成
// "提权总是失败" —— 那种错不会报错，只会让用户以为目标机有问题。
const privilegeUIDMarker = "PANEL_UID="

// privilegeCheckScript 自检脚本：把实际 uid 打成一行可解析的标记。
//
// 用标记而不是直接解析 id 的输出：su 路径下会话带伪终端，口令提示会混在
// 同一份输出里，只有自己打的标记才是可信的。
const privilegeCheckScript = `printf '` + privilegeUIDMarker + `%s\n' "$(id -u)"`

// handlePrivilegeCheck 实测一次提权，回答面板能不能在这台机器上拿到 root。
//
// 为什么需要它：su 这条路在探测阶段只能确认"路径存在"，口令对不对
// 只有真跑一次才知道。与其让用户在环境管理里装到一半才发现提权失败，不如给一个
// 点一下就有结论的入口。自检只跑 id -u，不改变目标机任何状态。
func (s *Server) handlePrivilegeCheck(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	// 自检是用户主动点的"现在到底行不行"，因此强制重探画像：
	// 顺手也把提权方式与「角色」列的缓存刷新成刚测出来的事实。
	profile, err := s.hostProfile(r.Context(), hostID, true)
	if err != nil {
		if !envDomainError(w, err) {
			s.writeHostError(w, err, "系统画像探测失败")
		}
		return
	}

	mode := profile.PrivilegeMode()
	if mode == env.PrivNone {
		writeJSON(w, http.StatusOK, privilegeCheckResult{Mode: string(mode),
			Detail: "没有可用的提权路径：目标机不是 root，也没有可用的 sudo 或 su" +
				"（su 需要主机上另配的提权口令，sudo 需要这个账号能免密或能凭登录口令通过密码认证）。"})
		return
	}

	ex, _, err := s.envTarget(hostID)
	if err != nil {
		s.writeHostError(w, err, "连接目标主机失败")
		return
	}
	res, err := ex.Run(r.Context(), env.Wrap(privilegeCheckScript, profile), env.ProbeTimeout)
	if err != nil {
		s.writeHostError(w, err, "提权自检执行失败")
		return
	}

	uid, parsed := parsePrivilegeUID(res.Stdout)
	switch {
	case !parsed:
		writeJSON(w, http.StatusOK, privilegeCheckResult{Mode: string(mode),
			Detail: "提权失败（" + privilegeModeLabel(mode) + "）：" + privilegeFailureText(res)})
	case uid != "0":
		writeJSON(w, http.StatusOK, privilegeCheckResult{Mode: string(mode),
			Detail: "提权命令执行了，但拿到的仍是普通用户（uid=" + uid + "），" +
				"面板将无法变更系统环境。"})
	default:
		writeJSON(w, http.StatusOK, privilegeCheckResult{OK: true, Mode: string(mode),
			Detail: "已能取得 root 权限（" + privilegeModeLabel(mode) + "）"})
	}
}

// parsePrivilegeUID 从自检输出里取出 uid。
//
// 标记不一定在行首：su 的口令提示既不回显也不换行，标记会紧跟在
// "Password: " 之后落在同一行（"Password: PANEL_UID=0"）。
func parsePrivilegeUID(out string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, privilegeUIDMarker); i >= 0 {
			return strings.TrimSpace(line[i+len(privilegeUIDMarker):]), true
		}
	}
	return "", false
}

// privilegeFailureText 取失败原因。
//
// 提权命令的报错落在不同流里：su 带着伪终端，sshd 会把 stderr 并进 stdout；
// 万一走了不带伪终端的通道，它又会规规矩矩写 stderr。两边都看一眼才能拿到那句话。
func privilegeFailureText(res *env.ExecResult) string {
	raw := env.TailLines(res.Stderr, 3)
	if raw == "" {
		raw = env.TailLines(res.Stdout, 3)
	}
	if raw == "" {
		return "命令没有返回任何信息，请查看目标机的认证日志"
	}
	return stripPasswordPrompt(raw)
}

// stripPasswordPrompt 去掉面板自己造成的口令提示前缀。
//
// su 从伪终端读口令，它的 "Password: " 提示既不回显也不换行，会和随后的报错
// 挤在同一行（"Password: su: Authentication failure"）。这个提示是面板自己
// 触发的，不该出现在给用户看的失败原因里 —— 用户会以为那是目标机的问题。
func stripPasswordPrompt(text string) string {
	if rest, ok := strings.CutPrefix(text, "Password: "); ok {
		return rest
	}
	return text
}

// handleEnvPackages 返回已安装包或可升级包列表。
func (s *Server) handleEnvPackages(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	ex, _, err := s.envTarget(hostID)
	if err != nil {
		s.writeHostError(w, err, "连接目标主机失败")
		return
	}

	profile, err := env.Probe(r.Context(), ex)
	if err != nil {
		if !envDomainError(w, err) {
			s.writeHostError(w, err, "系统画像探测失败")
		}
		return
	}
	mgr := env.NewManager(profile)
	if mgr == nil {
		writeErr(w, http.StatusBadRequest, "目标系统的包管理器不受支持")
		return
	}

	switch r.URL.Query().Get("filter") {
	case "upgrades":
		ups, err := mgr.Upgrades(r.Context(), ex)
		if err != nil {
			s.writeHostError(w, err, "查询可升级包失败")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"upgrades": ups, "family": profile.Family})
	default:
		pkgs, err := mgr.Installed(r.Context(), ex)
		if err != nil {
			s.writeHostError(w, err, "查询已安装包失败")
			return
		}
		truncated := false
		if len(pkgs) > maxEnvPackages {
			pkgs = pkgs[:maxEnvPackages]
			truncated = true
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"packages": pkgs, "family": profile.Family, "truncated": truncated,
		})
	}
}

// handleEnvCandidates 返回某个包的可安装版本。
func (s *Server) handleEnvCandidates(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	pkg := r.URL.Query().Get("pkg")
	if pkg == "" {
		writeErr(w, http.StatusBadRequest, "缺少 pkg 参数")
		return
	}
	ex, _, err := s.envTarget(hostID)
	if err != nil {
		s.writeHostError(w, err, "连接目标主机失败")
		return
	}
	profile, err := env.Probe(r.Context(), ex)
	if err != nil {
		if !envDomainError(w, err) {
			s.writeHostError(w, err, "系统画像探测失败")
		}
		return
	}
	mgr := env.NewManager(profile)
	cands, err := mgr.Candidates(r.Context(), ex, pkg)
	if err != nil {
		if !envDomainError(w, err) {
			s.writeHostError(w, err, "查询版本候选失败")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"candidates": cands, "family": profile.Family})
}

// handleEnvPlan 执行只读预检，返回变更计划（含求解器冲突清单）。
func (s *Server) handleEnvPlan(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req env.RunRequest
	if err := decodeJSONBody(w, r, &req, 1<<16); err != nil {
		return
	}
	ex, _, err := s.envTarget(hostID)
	if err != nil {
		s.writeHostError(w, err, "连接目标主机失败")
		return
	}
	plan, err := s.envRunner.Plan(r.Context(), ex, hostID, req)
	if err != nil {
		if !envDomainError(w, err) {
			s.writeHostError(w, err, "环境变更预检失败")
		}
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// handleEnvSources 读取目标机当前的安装源配置。
func (s *Server) handleEnvSources(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	ex, _, err := s.envTarget(hostID)
	if err != nil {
		s.writeHostError(w, err, "连接目标主机失败")
		return
	}
	profile, err := env.Probe(r.Context(), ex)
	if err != nil {
		if !envDomainError(w, err) {
			s.writeHostError(w, err, "系统画像探测失败")
		}
		return
	}
	files, err := env.ReadSources(r.Context(), ex, profile)
	if err != nil {
		s.writeHostError(w, err, "读取安装源配置失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"files": files, "family": profile.Family,
		"mirrors": env.ListMirrors(),
	})
}

// handleListEnvOps 返回环境操作记录。
func (s *Server) handleListEnvOps(w http.ResponseWriter, r *http.Request) {
	hostID := int64(-1)
	if raw := r.URL.Query().Get("hostId"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id < 0 {
			writeErr(w, http.StatusBadRequest, "hostId 参数无效")
			return
		}
		hostID = id
	}
	ops, err := s.store.ListEnvOps(hostID, 100)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询环境操作记录失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ops": ops})
}

// handleGetEnvOp 返回单次环境操作详情（含步骤日志）。
func (s *Server) handleGetEnvOp(w http.ResponseWriter, r *http.Request) {
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
		writeErr(w, http.StatusInternalServerError, "查询环境操作失败")
		return
	}
	writeJSON(w, http.StatusOK, op)
}
