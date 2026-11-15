package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"panelssh/internal/docker"
)

// Docker 相关请求的超时。
//
// 列表与单次动作都很轻，30 秒足够；拉镜像、docker run、compose up 会先下载
// 镜像或构建，几分钟是常态，因此单独给一个长超时（与 exec 的最大值一致）。
const (
	dockerTimeout     = 30 * time.Second
	dockerLongTimeout = 10 * time.Minute
	// dockerConfigTimeout 覆盖"写 daemon.json + 重启 Docker"。
	// 重启本身通常几秒，但容器多、磁盘慢的机器上可能拖到几十秒。
	dockerConfigTimeout = 90 * time.Second
	// dockerMaxComposeFile 限制在线编辑的 compose 文件体积。
	// 正文要经 base64 拼进一条 SSH 命令，体积会被放大两次（本地编码 + 提权包装），
	// 128 KB 已足够容纳远超正常规模的 compose 文件。
	dockerMaxComposeFile = 128 * 1024
)

// dockerExec 执行一条 docker 命令，必要时提权。
//
// 顺序是"先按登录用户跑，被拒绝再用 root 重做"，而不是无条件提权。原因是
// Docker 有两种部署形态：一种是 /var/run/docker.sock 属 docker 组，另一种是
// 用户自己的 rootless dockerd。后者只有登录用户才连得上 —— 无脑用 root 会
// 连到另一个（通常不存在的）守护进程上，把一台本来能管的机器判成"没有 Docker"。
func (s *Server) dockerExec(ctx context.Context, hostID int64, command string, timeout time.Duration) (*execResult, error) {
	res, err := s.execOnHost(ctx, hostID, command, timeout)
	if err != nil {
		return nil, err
	}
	if res.ExitCode == 0 {
		return res, nil
	}
	if isPermissionDeniedText(res.Stdout) || isPermissionDeniedText(res.Stderr) {
		return s.execEscalated(ctx, hostID, command, timeout, escalateRequired)
	}
	return res, nil
}

// dockerMessage 从执行结果里取一句能展示的原因。
func dockerMessage(res *execResult) string {
	text := strings.TrimSpace(res.Stderr)
	if text == "" {
		text = strings.TrimSpace(res.Stdout)
	}
	if text == "" {
		text = fmt.Sprintf("命令以退出码 %d 结束", res.ExitCode)
	}
	// 失败的 shell 输出可能很长（镜像构建日志），界面只需要结论。
	if len(text) > 800 {
		text = text[:800] + "…"
	}
	return text
}

// dockerDone 把执行结果翻译成响应；返回 false 表示已经写过响应。
func (s *Server) dockerDone(w http.ResponseWriter, res *execResult, err error, prefix string) bool {
	if err != nil {
		s.writeHostError(w, err, prefix)
		return false
	}
	if res.ExitCode != 0 {
		writeErr(w, http.StatusBadRequest, prefix+"："+dockerMessage(res))
		return false
	}
	return true
}

// handleDockerSummary 返回窗口轮询所需的全部状态。
func (s *Server) handleDockerSummary(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	res, err := s.dockerExec(r.Context(), hostID, docker.SummaryCommand(), dockerTimeout)
	if err != nil {
		s.writeHostError(w, err, "读取 Docker 状态失败")
		return
	}
	// 提权失败时输出里连一个探测标记都没有（最常见的是 su 的
	// "Authentication failure"）。这种情况绝不能解释成"没装 docker" ——
	// 那会把用户引去重装一个本来就装好的软件。把命令自己的话说出来。
	if !strings.Contains(res.Stdout, "PANELSSH_DOCKER=") {
		writeErr(w, http.StatusBadGateway, "读取 Docker 状态失败："+dockerMessage(res))
		return
	}
	summary := docker.ParseSummary(res.Stdout)
	// 命令整体失败（例如脚本最后一段被信号打断）时，把结论补进 probe，
	// 否则界面只会看到一份空列表。
	if res.ExitCode != 0 && summary.Probe.Error == "" {
		summary.Probe.Error = dockerMessage(res)
	}
	writeJSON(w, http.StatusOK, summary)
}

// handleDockerCreate 新建并启动一个容器。
func (s *Server) handleDockerCreate(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var spec docker.RunSpec
	if err := decodeJSONBody(w, r, &spec, 1<<20); err != nil {
		return
	}
	command, err := spec.Build()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.dockerExec(r.Context(), hostID, command, dockerLongTimeout)
	if !s.dockerDone(w, res, err, "创建容器失败") {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"output": strings.TrimSpace(res.Stdout),
	})
}

// dockerContainerActionRequest 是容器动作的入参。
type dockerContainerActionRequest struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	Force  bool   `json:"force"`
}

func (s *Server) handleDockerContainerAction(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req dockerContainerActionRequest
	if err := decodeJSONBody(w, r, &req, 1<<20); err != nil {
		return
	}
	command, err := docker.ContainerActionCommand(req.ID, req.Action, req.Force)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.dockerExec(r.Context(), hostID, command, dockerTimeout)
	if !s.dockerDone(w, res, err, "容器操作失败") {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "output": strings.TrimSpace(res.Stdout)})
}

// handleDockerContainerLogs 读取容器日志尾部。
func (s *Server) handleDockerContainerLogs(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	command, err := docker.LogsCommand(r.URL.Query().Get("id"), tail)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.dockerExec(r.Context(), hostID, command, dockerTimeout)
	if !s.dockerDone(w, res, err, "读取容器日志失败") {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"output": res.Stdout, "tail": tail})
}

// handleDockerContainerFiles 列出容器内一个目录的条目。
func (s *Server) handleDockerContainerFiles(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	container := r.URL.Query().Get("container")
	dir, err := docker.NormalizeContainerPath(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	command, err := docker.ListContainerFilesCommand(container, dir)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.dockerExec(r.Context(), hostID, command, dockerTimeout)
	if err != nil {
		s.writeHostError(w, err, "读取容器目录失败")
		return
	}
	// 目录不存在与"docker 自己报错"分开说：前者是用户点错了路径，
	// 后者要去查容器状态或权限。
	if res.ExitCode != 0 && strings.Contains(res.Stderr, "__PANELSSH_NO_DIR__") {
		writeErr(w, http.StatusNotFound, "目录不存在："+dir)
		return
	}
	if !s.dockerDone(w, res, err, "读取容器目录失败") {
		return
	}
	writeJSON(w, http.StatusOK, docker.ParseContainerFiles(res.Stdout, dir))
}

// containerFileText 是容器内一个文本文件的读取结果。
//
// binary 为真时 content 为空 —— 二进制内容既没法在界面上读，也不该原样塞进 JSON。
type containerFileText struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
	Binary    bool   `json:"binary"`
}

// handleDockerContainerFile 读取容器内的一个文本文件。
func (s *Server) handleDockerContainerFile(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	file, err := docker.NormalizeContainerPath(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	command, err := docker.ReadContainerFileCommand(r.URL.Query().Get("container"), file)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.dockerExec(r.Context(), hostID, command, dockerTimeout)
	if err != nil {
		s.writeHostError(w, err, "读取容器文件失败")
		return
	}
	if res.ExitCode != 0 && strings.Contains(res.Stderr, "__PANELSSH_NOT_FILE__") {
		writeErr(w, http.StatusBadRequest, "这不是一个普通文件："+file)
		return
	}
	if !s.dockerDone(w, res, err, "读取容器文件失败") {
		return
	}
	body := containerFileText{
		Path:    file,
		Content: res.Stdout,
		Truncated: res.StdoutClipped || strings.Contains(res.Stderr, "__PANELSSH_TRUNCATED__") ||
			strings.Contains(res.Stdout, "[输出超过上限，已截断]"),
	}
	if strings.IndexByte(body.Content, 0) >= 0 {
		body.Binary = true
		body.Content = ""
	}
	writeJSON(w, http.StatusOK, body)
}

// handleDockerImagePull 拉取镜像。
func (s *Server) handleDockerImagePull(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		Reference string `json:"reference"`
	}
	if err := decodeJSONBody(w, r, &req, 1<<20); err != nil {
		return
	}
	command, err := docker.PullCommand(strings.TrimSpace(req.Reference))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.dockerExec(r.Context(), hostID, command, dockerLongTimeout)
	if !s.dockerDone(w, res, err, "拉取镜像失败") {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "output": strings.TrimSpace(res.Stdout)})
}

// handleDockerImageRemove 删除镜像。
func (s *Server) handleDockerImageRemove(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		References []string `json:"references"`
		Force      bool     `json:"force"`
	}
	if err := decodeJSONBody(w, r, &req, 1<<20); err != nil {
		return
	}
	command, err := docker.RemoveImagesCommand(req.References, req.Force)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.dockerExec(r.Context(), hostID, command, dockerTimeout)
	if !s.dockerDone(w, res, err, "删除镜像失败") {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "output": strings.TrimSpace(res.Stdout)})
}

// handleDockerImageSearch 搜索镜像仓库。
func (s *Server) handleDockerImageSearch(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	term := strings.TrimSpace(r.URL.Query().Get("q"))
	command, err := docker.SearchCommand(term)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.dockerExec(r.Context(), hostID, command, dockerTimeout)
	if !s.dockerDone(w, res, err, "搜索镜像失败") {
		return
	}
	hits, problem := docker.ParseSearch(res.Stdout)
	writeJSON(w, http.StatusOK, map[string]any{"hits": hits, "note": problem})
}

// handleDockerConfigRead 读取目标机的镜像加速与代理配置。
func (s *Server) handleDockerConfigRead(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	res, err := s.execEscalated(
		r.Context(), hostID, docker.DaemonConfigReadCommand(), dockerTimeout, escalatePreferred,
	)
	if err != nil {
		s.writeHostError(w, err, "读取 Docker 配置失败")
		return
	}
	if res.ExitCode != 0 {
		writeErr(w, http.StatusBadRequest, "读取 Docker 配置失败："+dockerMessage(res))
		return
	}
	cfg, _, parseErr := docker.ParseDaemonConfig(res.Stdout)
	if parseErr != nil {
		writeErr(w, http.StatusBadRequest, parseErr.Error())
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// handleDockerConfigSave 写入镜像加速与代理配置，并重启 Docker 让它们生效。
//
// 先读后写：daemon.json 里可能还有 data-root 这类字段，覆盖前必须原样保留。
// 任何一步失败都返回明确原因，写入成功但重启失败时由目标机脚本自行回滚。
func (s *Server) handleDockerConfigSave(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req docker.DaemonConfigUpdate
	if err := decodeJSONBody(w, r, &req, 1<<20); err != nil {
		return
	}

	res, err := s.execEscalated(
		r.Context(), hostID, docker.DaemonConfigReadCommand(), dockerTimeout, escalatePreferred,
	)
	if err != nil {
		s.writeHostError(w, err, "读取 Docker 配置失败")
		return
	}
	if res.ExitCode != 0 {
		writeErr(w, http.StatusBadRequest, "读取 Docker 配置失败："+dockerMessage(res))
		return
	}
	_, raw, parseErr := docker.ParseDaemonConfig(res.Stdout)
	if parseErr != nil {
		writeErr(w, http.StatusBadRequest, parseErr.Error())
		return
	}

	// proxies 是 Docker 23.0 才认的字段；更老的 dockerd 会把它当未知字段拒绝启动。
	// 因此在写入前先确认版本，不拿目标机上的容器去赌。
	if strings.TrimSpace(req.Proxy) != "" {
		verRes, verErr := s.dockerExec(r.Context(), hostID, "docker version --format '{{.Server.Version}}'", dockerTimeout)
		if verErr != nil {
			s.writeHostError(w, verErr, "读取 Docker 版本失败")
			return
		}
		if verRes.ExitCode != 0 {
			writeErr(w, http.StatusBadRequest, "无法确认 Docker 版本，未写入代理配置："+dockerMessage(verRes))
			return
		}
		if !docker.ProxySupported(strings.TrimSpace(verRes.Stdout)) {
			writeErr(w, http.StatusBadRequest,
				"目标机 Docker 版本不支持 daemon.json 的 proxies 字段（需要 23.0 及以上），请改用 systemd 代理配置")
			return
		}
	}

	payload, mergeErr := docker.MergeDaemonConfig(raw, req)
	if mergeErr != nil {
		writeErr(w, http.StatusBadRequest, mergeErr.Error())
		return
	}

	applyRes, applyErr := s.execEscalated(
		r.Context(), hostID,
		docker.DaemonConfigApplyCommand(payload),
		dockerConfigTimeout, escalateRequired,
	)
	if applyErr != nil {
		s.writeHostError(w, applyErr, "应用 Docker 配置失败")
		return
	}
	if applyRes.ExitCode != 0 {
		writeErr(w, http.StatusBadRequest, "应用 Docker 配置失败（已回滚）："+dockerMessage(applyRes))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "output": strings.TrimSpace(applyRes.Stdout)})
}

// dockerNetworkCreateRequest 是创建网络的入参。
type dockerNetworkCreateRequest struct {
	Name     string `json:"name"`
	Driver   string `json:"driver"`
	Subnet   string `json:"subnet"`
	Gateway  string `json:"gateway"`
	Internal bool   `json:"internal"`
}

func (s *Server) handleDockerNetworkCreate(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req dockerNetworkCreateRequest
	if err := decodeJSONBody(w, r, &req, 1<<20); err != nil {
		return
	}
	command, err := docker.NetworkCreateCommand(
		strings.TrimSpace(req.Name), strings.TrimSpace(req.Driver),
		strings.TrimSpace(req.Subnet), strings.TrimSpace(req.Gateway), req.Internal,
	)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.dockerExec(r.Context(), hostID, command, dockerTimeout)
	if !s.dockerDone(w, res, err, "创建网络失败") {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": strings.TrimSpace(res.Stdout)})
}

func (s *Server) handleDockerNetworkRemove(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSONBody(w, r, &req, 1<<20); err != nil {
		return
	}
	command, err := docker.NetworkRemoveCommand(req.Name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.dockerExec(r.Context(), hostID, command, dockerTimeout)
	if !s.dockerDone(w, res, err, "删除网络失败") {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "output": strings.TrimSpace(res.Stdout)})
}

// handleDockerNetworkDetail 返回一个网络的详情（含接入的容器）。
func (s *Server) handleDockerNetworkDetail(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	command, err := docker.NetworkInspectCommand(r.URL.Query().Get("name"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.dockerExec(r.Context(), hostID, command, dockerTimeout)
	if !s.dockerDone(w, res, err, "读取网络详情失败") {
		return
	}
	detail, problem := docker.ParseNetworkDetail(res.Stdout)
	if problem != "" {
		writeErr(w, http.StatusBadGateway, "解析网络详情失败："+problem)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// handleDockerNetworkConnect 把容器接入或断开网络。
func (s *Server) handleDockerNetworkConnect(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		Name       string `json:"name"`
		Container  string `json:"container"`
		Disconnect bool   `json:"disconnect"`
	}
	if err := decodeJSONBody(w, r, &req, 1<<20); err != nil {
		return
	}
	command, err := docker.NetworkConnectCommand(req.Name, req.Container, req.Disconnect)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	verb := "接入网络失败"
	if req.Disconnect {
		verb = "断开网络失败"
	}
	res, err := s.dockerExec(r.Context(), hostID, command, dockerTimeout)
	if !s.dockerDone(w, res, err, verb) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// normalizeComposePath 校验并规范化 compose 文件路径。
//
// 面板会以 root 写入这个路径，因此只接受绝对路径，并先用 Clean 把 ".." 与重复
// 斜杠消掉 —— 校验与最终拼进命令的必须是同一个字符串。
func normalizeComposePath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("请填写 compose 文件路径")
	}
	if !filepath.IsAbs(raw) {
		return "", errors.New("compose 文件路径必须是绝对路径")
	}
	clean := filepath.Clean(raw)
	if clean == "/" || filepath.Dir(clean) == clean {
		return "", errors.New("compose 文件路径不能是根目录")
	}
	return clean, nil
}

// handleDockerComposeRead 读取 compose 文件内容。
func (s *Server) handleDockerComposeRead(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	file, err := normalizeComposePath(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	command := "cat -- " + shellQuotePath(file)
	res, err := s.execOnHost(r.Context(), hostID, command, dockerTimeout)
	if err != nil {
		s.writeHostError(w, err, "读取 compose 文件失败")
		return
	}
	if res.ExitCode == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"path": file, "content": res.Stdout})
		return
	}

	// 文件在 root 独占的目录里时，登录用户读不到 —— 用 root 再读一次。
	if isPermissionDeniedText(res.Stdout) || isPermissionDeniedText(res.Stderr) {
		output, readErr := s.runEscalated(r.Context(), hostID, command, dockerTimeout, escalateRequired)
		if readErr != nil {
			s.writeHostError(w, readErr, "读取 compose 文件失败")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": file, "content": output})
		return
	}
	if strings.Contains(strings.ToLower(res.Stderr), "no such file") {
		writeErr(w, http.StatusNotFound, "文件不存在："+file)
		return
	}
	writeErr(w, http.StatusBadRequest, "读取 compose 文件失败："+dockerMessage(res))
}

// dockerComposeSaveRequest 是在线编辑 compose 文件的入参。
type dockerComposeSaveRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Project string `json:"project"`
	// Deploy 为真表示保存后立刻 compose up -d。
	Deploy bool `json:"deploy"`
}

// handleDockerComposeSave 写入 compose 文件，可选地立即部署。
func (s *Server) handleDockerComposeSave(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req dockerComposeSaveRequest
	if err := decodeJSONBody(w, r, &req, 1<<20); err != nil {
		return
	}
	file, err := normalizeComposePath(req.Path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		writeErr(w, http.StatusBadRequest, "compose 内容不能为空")
		return
	}
	if len(req.Content) > dockerMaxComposeFile {
		writeErr(w, http.StatusBadRequest, "compose 文件过大（上限 128 KB）")
		return
	}

	encoded := base64.StdEncoding.EncodeToString([]byte(req.Content))
	script := "mkdir -p " + shellQuotePath(filepath.Dir(file)) + "\n" +
		"base64 -d > " + shellQuotePath(file) + " <<'PANELSSH_FILE_EOF'\n" +
		encoded + "\nPANELSSH_FILE_EOF\n"
	// runFileStep 先按登录用户写，只有被权限拒绝才用 root 重做：
	// 无条件提权会让用户自己目录里的 compose 文件变成 root 属主。
	if err := s.runFileStep(r.Context(), hostID, script, "写入 compose 文件"); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	if !req.Deploy {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": file, "output": ""})
		return
	}

	command, err := docker.ComposeUpCommand(strings.TrimSpace(req.Project), file)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "文件已保存，但部署失败："+err.Error())
		return
	}
	res, execErr := s.dockerExec(r.Context(), hostID, command, dockerLongTimeout)
	if execErr != nil {
		s.writeHostError(w, execErr, "文件已保存，但部署失败")
		return
	}
	if res.ExitCode != 0 {
		writeErr(w, http.StatusBadRequest, "文件已保存，但部署失败："+dockerMessage(res))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"path":   file,
		"output": strings.TrimSpace(res.Stdout),
	})
}

// dockerComposeActionRequest 是项目级操作的入参。
type dockerComposeActionRequest struct {
	Project string   `json:"project"`
	Files   []string `json:"files"`
	Action  string   `json:"action"`
}

// handleDockerComposeAction 执行 compose 项目操作（up/stop/restart/down/logs/pull）。
func (s *Server) handleDockerComposeAction(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req dockerComposeActionRequest
	if err := decodeJSONBody(w, r, &req, 1<<20); err != nil {
		return
	}
	command, err := docker.ComposeActionCommand(strings.TrimSpace(req.Project), req.Files, req.Action)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	timeout := dockerTimeout
	switch req.Action {
	case "up", "down", "pull":
		timeout = dockerLongTimeout
	}
	res, err := s.dockerExec(r.Context(), hostID, command, timeout)
	if !s.dockerDone(w, res, err, "compose 操作失败") {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "output": res.Stdout})
}
