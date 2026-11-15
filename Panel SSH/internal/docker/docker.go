// Package docker 封装目标机上 docker CLI 的命令构造与输出解析。
//
// 面板不去连 Docker 的 HTTP API：目标机上可能只开了 unix socket（权限与 root
// 绑定），而面板已经有一条能执行命令的通道。用 CLI 的 `--format '{{json .}}'`
// 拿到的是稳定的一行一个 JSON，比解析表格更适合跨版本兼容。
package docker

import (
	"encoding/json"
	"fmt"
	"net"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Quote 把单个参数安全地拼进 shell 命令。
//
// 容器名、镜像名都由用户输入，虽然下面会做字符校验，但拼进 shell 前仍要转义：
// 这一层不能依赖"上游一定校验过"。
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// 校验用的字符集。镜像名覆盖 registry:port/ns/name:tag@sha256:... 这一整类写法；
// 容器名与 compose 项目名按 docker 自己的规则收敛。
var (
	reImage   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)
	reName    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	reProject = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	reKey     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	reVersion = regexp.MustCompile(`\d+\.\d+(?:\.\d+)?`)
)

// Section 标记把一次命令的多块输出分开。
//
// 目标机上的 docker 是三次独立调用（ps / images / network ls），各自输出
// 一行一个 JSON。用标记切开比开三条 SSH 会话省得多 —— 轮询时每多一条会话
// 就多一次目标机上的进程创建。
const (
	SectionContainers = "__PANELSSH_CONTAINERS__"
	SectionImages     = "__PANELSSH_IMAGES__"
	SectionNetworks   = "__PANELSSH_NETWORKS__"
)

// Probe 是一台主机上 Docker 与 Compose 的可用性。
type Probe struct {
	// Available 只表示 docker 命令存在，不表示守护进程连得上。
	Available bool `json:"available"`
	// Version 是 docker 服务端版本；连不上守护进程时这里放的是报错原文。
	Version string `json:"version"`
	// ComposeVersion 为空表示目标机上没有 compose（v2 插件与 v1 都探过）。
	ComposeVersion string `json:"composeVersion"`
	// Error 非空表示这台机器上的 Docker 现在用不了，内容可以直接展示。
	Error string `json:"error"`
}

// Container 是 docker ps 的一行。
type Container struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Image       string   `json:"image"`
	Command     string   `json:"command"`
	CreatedAt   string   `json:"createdAt"`
	RunningFor  string   `json:"runningFor"`
	State       string   `json:"state"`
	Status      string   `json:"status"`
	Ports       string   `json:"ports"`
	Mounts      string   `json:"mounts"`
	Networks    string   `json:"networks"`
	Project     string   `json:"project"`
	WorkingDir  string   `json:"workingDir"`
	ConfigFiles []string `json:"configFiles"`
}

// Image 是 docker images 的一行。
type Image struct {
	ID         string `json:"id"`
	Repo       string `json:"repo"`
	Tag        string `json:"tag"`
	Ref        string `json:"ref"`
	Digest     string `json:"digest"`
	Size       string `json:"size"`
	Created    string `json:"created"`
	Containers string `json:"containers"`
	Dangling   bool   `json:"dangling"`
}

// Network 是 docker network ls 的一行。
type Network struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Driver    string `json:"driver"`
	Scope     string `json:"scope"`
	IPv6      bool   `json:"ipv6"`
	Internal  bool   `json:"internal"`
	CreatedAt string `json:"createdAt"`
}

// NetworkAttachment 是某个网络上的一个容器。
type NetworkAttachment struct {
	ContainerID string `json:"containerId"`
	Name        string `json:"name"`
	IPv4        string `json:"ipv4"`
	Mac         string `json:"mac"`
}

// NetworkSubnet 是网络的一段地址池。
type NetworkSubnet struct {
	Subnet  string `json:"subnet"`
	Gateway string `json:"gateway"`
}

// NetworkDetail 来自 docker network inspect。
type NetworkDetail struct {
	ID         string              `json:"id"`
	Name       string              `json:"name"`
	Driver     string              `json:"driver"`
	Scope      string              `json:"scope"`
	Internal   bool                `json:"internal"`
	IPv6       bool                `json:"ipv6"`
	Subnets    []NetworkSubnet     `json:"subnets"`
	Containers []NetworkAttachment `json:"containers"`
}

// Project 是 compose 项目。它由容器上的 com.docker.compose.* 标签聚合而来，
// 因此不依赖 compose CLI 是否存在，也不依赖 `docker compose ls` 的版本差异。
type Project struct {
	Name        string   `json:"name"`
	ConfigFiles []string `json:"configFiles"`
	WorkingDir  string   `json:"workingDir"`
	Total       int      `json:"total"`
	Running     int      `json:"running"`
	Containers  []string `json:"containers"`
}

// SearchHit 是 docker search 的一行。
type SearchHit struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Stars       int    `json:"stars"`
	Official    bool   `json:"official"`
	Automated   bool   `json:"automated"`
}

// Summary 是窗口一次轮询要的全部数据。
type Summary struct {
	Probe      Probe             `json:"probe"`
	Containers []Container       `json:"containers"`
	Images     []Image           `json:"images"`
	Networks   []Network         `json:"networks"`
	Projects   []Project         `json:"projects"`
	Errors     map[string]string `json:"errors,omitempty"`
}

// SummaryCommand 一次取齐状态、容器、镜像与网络。
//
// 先用 `command -v docker` 判存在：没有 docker 时后面的命令都会输出
// "command not found"，那些噪音会盖住真正要告诉用户的一句话。
func SummaryCommand() string {
	return `if ! command -v docker >/dev/null 2>&1; then
  echo 'PANELSSH_DOCKER=missing'
  exit 0
fi
echo 'PANELSSH_DOCKER=ok'
echo "PANELSSH_SERVER=$(docker version --format '{{.Server.Version}}' 2>&1 | tail -n 1)"
echo "PANELSSH_COMPOSE=$( (docker compose version --short 2>/dev/null || docker-compose version --short 2>/dev/null) | tail -n 1)"
echo '` + SectionContainers + `'
docker ps -a --format '{{json .}}' 2>&1
echo '` + SectionImages + `'
docker images --format '{{json .}}' 2>&1
echo '` + SectionNetworks + `'
docker network ls --format '{{json .}}' 2>&1`
}

// ParseProbe 解析探测结果。
func ParseProbe(raw string) Probe {
	var p Probe
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "PANELSSH_DOCKER=missing":
			// 保持 Available=false，错误在函数末尾统一给出。
		case strings.HasPrefix(line, "PANELSSH_DOCKER=ok"):
			p.Available = true
		case strings.HasPrefix(line, "PANELSSH_SERVER="):
			p.Version = strings.TrimSpace(strings.TrimPrefix(line, "PANELSSH_SERVER="))
		case strings.HasPrefix(line, "PANELSSH_COMPOSE="):
			p.ComposeVersion = strings.TrimSpace(strings.TrimPrefix(line, "PANELSSH_COMPOSE="))
		}
	}
	if !p.Available {
		p.Error = "目标机上没有找到 docker 命令"
		return p
	}
	// docker 的版本号是 20.10.7 这样的形状。输出里找不到版本号，说明 --format
	// 拿到的其实是守护进程的报错（最常见的是连不上 socket、权限不足）。
	// 用"从任意位置提取"而不是"整行必须匹配"：docker 会先打一行配置文件的警告，
	// 那一行不影响服务端版本，却会把整句判成错误。
	if version := reVersion.FindString(p.Version); version != "" {
		p.Version = version
	} else {
		p.Error = p.Version
		if p.Error == "" {
			p.Error = "无法连接到 Docker 守护进程"
		}
	}
	return p
}

// ParseSummary 解析 SummaryCommand 的输出。
func ParseSummary(raw string) Summary {
	sections := map[string][]string{}
	current := "probe"
	for _, line := range strings.Split(raw, "\n") {
		switch line {
		case SectionContainers:
			current = "containers"
			continue
		case SectionImages:
			current = "images"
			continue
		case SectionNetworks:
			current = "networks"
			continue
		}
		sections[current] = append(sections[current], line)
	}

	summary := Summary{Errors: map[string]string{}}
	summary.Probe = ParseProbe(strings.Join(sections["probe"], "\n"))
	summary.Containers, summary.Errors["containers"] = ParseContainers(strings.Join(sections["containers"], "\n"))
	summary.Images, summary.Errors["images"] = ParseImages(strings.Join(sections["images"], "\n"))
	summary.Networks, summary.Errors["networks"] = ParseNetworks(strings.Join(sections["networks"], "\n"))
	summary.Projects = Projects(summary.Containers)
	for key, value := range summary.Errors {
		if value == "" {
			delete(summary.Errors, key)
		}
	}
	if len(summary.Errors) == 0 {
		summary.Errors = nil
	}
	return summary
}

// jsonLines 把"一行一个 JSON"的输出拆开。
//
// 第二个返回值收集所有不是 JSON 的行：docker 的警告与报错都混在同一个流里，
// 丢掉它们会让"列表是空的"变成一句没有原因的结论。
func jsonLines[T any](raw string) ([]T, string) {
	var out []T
	var problems []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "{") {
			problems = append(problems, line)
			continue
		}
		var item T
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			problems = append(problems, line)
			continue
		}
		out = append(out, item)
	}
	return out, strings.Join(problems, "\n")
}

type containerLine struct {
	ID         string `json:"ID"`
	Names      string `json:"Names"`
	Image      string `json:"Image"`
	Command    string `json:"Command"`
	CreatedAt  string `json:"CreatedAt"`
	RunningFor string `json:"RunningFor"`
	State      string `json:"State"`
	Status     string `json:"Status"`
	Ports      string `json:"Ports"`
	Mounts     string `json:"Mounts"`
	Networks   string `json:"Networks"`
	Labels     string `json:"Labels"`
}

// ParseContainers 解析 docker ps --format '{{json .}}'。
func ParseContainers(raw string) ([]Container, string) {
	lines, problem := jsonLines[containerLine](raw)
	out := make([]Container, 0, len(lines))
	for _, item := range lines {
		labels := parseLabels(item.Labels)
		container := Container{
			ID:         item.ID,
			Name:       item.Names,
			Image:      item.Image,
			Command:    item.Command,
			CreatedAt:  item.CreatedAt,
			RunningFor: item.RunningFor,
			State:      item.State,
			Status:     item.Status,
			Ports:      item.Ports,
			Mounts:     item.Mounts,
			Networks:   item.Networks,
			Project:    labels["com.docker.compose.project"],
			WorkingDir: labels["com.docker.compose.project.working_dir"],
		}
		if files := labels["com.docker.compose.project.config_files"]; files != "" {
			for _, part := range strings.Split(files, ",") {
				if trimmed := strings.TrimSpace(part); trimmed != "" {
					container.ConfigFiles = append(container.ConfigFiles, trimmed)
				}
			}
		}
		out = append(out, container)
	}
	return out, problem
}

// parseLabels 把 docker 的 "k=v,k=v" 标签串拆成 map。
//
// 标签值里理论上可以出现逗号（路径很少见），这里按 docker 自己的拼法处理：
// 拿不到就是拿不到，不做更复杂的转义猜测 —— compose 的路径由它写进标签，
// 正常不会带逗号。
func parseLabels(raw string) map[string]string {
	labels := map[string]string{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		labels[strings.TrimSpace(key)] = value
	}
	return labels
}

type imageLine struct {
	ID           string `json:"ID"`
	Repository   string `json:"Repository"`
	Tag          string `json:"Tag"`
	Digest       string `json:"Digest"`
	Size         string `json:"Size"`
	CreatedAt    string `json:"CreatedAt"`
	CreatedSince string `json:"CreatedSince"`
	Containers   string `json:"Containers"`
}

// ParseImages 解析 docker images --format '{{json .}}'。
func ParseImages(raw string) ([]Image, string) {
	lines, problem := jsonLines[imageLine](raw)
	out := make([]Image, 0, len(lines))
	for _, item := range lines {
		repo := item.Repository
		tag := item.Tag
		// 可操作的引用：仓库与标签都有效时用 repo:tag，否则退回镜像 ID。
		// 悬空镜像的 repo/tag 是 <none>，拿它去 rmi 只会得到一句 not found。
		ref := item.ID
		if repo != "" && repo != "<none>" && tag != "" && tag != "<none>" {
			ref = repo + ":" + tag
		}
		created := item.CreatedSince
		if created == "" {
			created = item.CreatedAt
		}
		out = append(out, Image{
			ID:         item.ID,
			Repo:       repo,
			Tag:        tag,
			Ref:        ref,
			Digest:     item.Digest,
			Size:       item.Size,
			Created:    created,
			Containers: item.Containers,
			Dangling:   (repo == "" || repo == "<none>") && (tag == "" || tag == "<none>"),
		})
	}
	return out, problem
}

// flexBool 兼容 docker 在 format 里把布尔值写成 true 或 "true" 两种形态。
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	raw := strings.Trim(strings.TrimSpace(string(data)), `"`)
	*b = flexBool(raw == "true")
	return nil
}

// flexInt 兼容数字与字符串数字（docker search 的 StarCount 在不同版本里两种都见过）。
type flexInt int

func (i *flexInt) UnmarshalJSON(data []byte) error {
	raw := strings.Trim(strings.TrimSpace(string(data)), `"`)
	if raw == "" || raw == "<nil>" {
		*i = 0
		return nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		*i = 0
		return nil
	}
	*i = flexInt(value)
	return nil
}

type networkLine struct {
	ID        string   `json:"ID"`
	Name      string   `json:"Name"`
	Driver    string   `json:"Driver"`
	Scope     string   `json:"Scope"`
	CreatedAt string   `json:"CreatedAt"`
	IPv6      flexBool `json:"IPv6"`
	Internal  flexBool `json:"Internal"`
}

// ParseNetworks 解析 docker network ls --format '{{json .}}'。
func ParseNetworks(raw string) ([]Network, string) {
	lines, problem := jsonLines[networkLine](raw)
	out := make([]Network, 0, len(lines))
	for _, item := range lines {
		out = append(out, Network{
			ID:        item.ID,
			Name:      item.Name,
			Driver:    item.Driver,
			Scope:     item.Scope,
			IPv6:      bool(item.IPv6),
			Internal:  bool(item.Internal),
			CreatedAt: item.CreatedAt,
		})
	}
	return out, problem
}

type inspectNetwork struct {
	ID         string `json:"Id"`
	Name       string `json:"Name"`
	Driver     string `json:"Driver"`
	Scope      string `json:"Scope"`
	Internal   bool   `json:"Internal"`
	EnableIPv6 bool   `json:"EnableIPv6"`
	IPAM       struct {
		Config []struct {
			Subnet  string `json:"Subnet"`
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
	Containers map[string]struct {
		Name        string `json:"Name"`
		IPv4Address string `json:"IPv4Address"`
		MacAddress  string `json:"MacAddress"`
	} `json:"Containers"`
}

// ParseNetworkDetail 解析 docker network inspect 的 JSON 数组。
func ParseNetworkDetail(raw string) (NetworkDetail, string) {
	var items []inspectNetwork
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &items); err != nil {
		return NetworkDetail{}, err.Error()
	}
	if len(items) == 0 {
		return NetworkDetail{}, "目标机上没有这个网络"
	}
	item := items[0]
	detail := NetworkDetail{
		ID:       item.ID,
		Name:     item.Name,
		Driver:   item.Driver,
		Scope:    item.Scope,
		Internal: item.Internal,
		IPv6:     item.EnableIPv6,
	}
	for _, cfg := range item.IPAM.Config {
		detail.Subnets = append(detail.Subnets, NetworkSubnet{Subnet: cfg.Subnet, Gateway: cfg.Gateway})
	}
	for id, attached := range item.Containers {
		// docker 给的是带掩码的 172.20.0.2/16，界面上只展示地址本身。
		addr, _, _ := strings.Cut(attached.IPv4Address, "/")
		detail.Containers = append(detail.Containers, NetworkAttachment{
			ContainerID: id,
			Name:        attached.Name,
			IPv4:        addr,
			Mac:         attached.MacAddress,
		})
	}
	sort.Slice(detail.Containers, func(i, j int) bool {
		return detail.Containers[i].Name < detail.Containers[j].Name
	})
	return detail, ""
}

// Projects 把容器按 compose 项目聚合。
func Projects(containers []Container) []Project {
	byName := map[string]*Project{}
	for _, container := range containers {
		if container.Project == "" {
			continue
		}
		item := byName[container.Project]
		if item == nil {
			item = &Project{
				Name:        container.Project,
				ConfigFiles: container.ConfigFiles,
				WorkingDir:  container.WorkingDir,
			}
			byName[container.Project] = item
		}
		// 同一项目的容器标签一致；取第一个非空的，避免某个容器缺标签时丢掉路径。
		if len(item.ConfigFiles) == 0 && len(container.ConfigFiles) > 0 {
			item.ConfigFiles = container.ConfigFiles
		}
		if item.WorkingDir == "" {
			item.WorkingDir = container.WorkingDir
		}
		item.Total++
		if strings.EqualFold(container.State, "running") {
			item.Running++
		}
		item.Containers = append(item.Containers, container.Name)
	}
	out := make([]Project, 0, len(byName))
	for _, item := range byName {
		sort.Strings(item.Containers)
		out = append(out, *item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RunSpec 是"新建并启动容器"的表单。
type RunSpec struct {
	Image   string   `json:"image"`
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Ports   []string `json:"ports"`
	Env     []string `json:"env"`
	Volumes []string `json:"volumes"`
	Restart string   `json:"restart"`
	Network string   `json:"network"`
}

// Build 生成 docker run 命令。
func (s RunSpec) Build() (string, error) {
	if !reImage.MatchString(s.Image) {
		return "", fmt.Errorf("镜像名不合法：%s", s.Image)
	}
	args := []string{"docker", "run", "-d"}
	if s.Name != "" {
		if !reName.MatchString(s.Name) {
			return "", fmt.Errorf("容器名只能包含字母、数字、下划线、点和短横线")
		}
		args = append(args, "--name", Quote(s.Name))
	}
	if s.Restart != "" {
		if !validRestart(s.Restart) {
			return "", fmt.Errorf("重启策略不合法：%s", s.Restart)
		}
		args = append(args, "--restart", Quote(s.Restart))
	}
	if s.Network != "" {
		if !reName.MatchString(s.Network) {
			return "", fmt.Errorf("网络名不合法：%s", s.Network)
		}
		args = append(args, "--network", Quote(s.Network))
	}
	for _, port := range s.Ports {
		port = strings.TrimSpace(port)
		if port == "" {
			continue
		}
		if !validPort(port) {
			return "", fmt.Errorf("端口映射不合法：%s（应形如 8080:80）", port)
		}
		args = append(args, "-p", Quote(port))
	}
	for _, env := range s.Env {
		env = strings.TrimSpace(env)
		if env == "" {
			continue
		}
		key, _, ok := strings.Cut(env, "=")
		if !ok || !reKey.MatchString(strings.TrimSpace(key)) {
			return "", fmt.Errorf("环境变量不合法：%s（应形如 KEY=VALUE）", env)
		}
		args = append(args, "-e", Quote(env))
	}
	for _, volume := range s.Volumes {
		volume = strings.TrimSpace(volume)
		if volume == "" {
			continue
		}
		if !validVolume(volume) {
			return "", fmt.Errorf("卷挂载不合法：%s（应形如 /host:/container）", volume)
		}
		args = append(args, "-v", Quote(volume))
	}
	args = append(args, Quote(s.Image))
	// 启动命令按原样交给 shell：它可能自带引号与管道（sh -c "..."），
	// 强行拆分只会把一条合法命令拆坏。执行者本身就是有 root 权限的管理员，
	// 这里不做二次解释。
	if command := strings.TrimSpace(s.Command); command != "" {
		args = append(args, command)
	}
	return strings.Join(args, " "), nil
}

func validRestart(value string) bool {
	switch value {
	case "no", "always", "unless-stopped":
		return true
	}
	if strings.HasPrefix(value, "on-failure") {
		rest := strings.TrimPrefix(value, "on-failure")
		if rest == "" {
			return true
		}
		if !strings.HasPrefix(rest, ":") {
			return false
		}
		if _, err := strconv.Atoi(strings.TrimPrefix(rest, ":")); err != nil {
			return false
		}
		return true
	}
	return false
}

func validPort(spec string) bool {
	if proto := strings.LastIndex(spec, "/"); proto >= 0 {
		switch spec[proto+1:] {
		case "tcp", "udp", "sctp":
		default:
			return false
		}
		spec = spec[:proto]
	}
	parts := strings.Split(spec, ":")
	if len(parts) > 3 || len(parts) == 0 {
		return false
	}
	// 最后一段一定是容器端口，必须有；前面的主机端口可选。
	container := parts[len(parts)-1]
	if !validPortNumber(container) {
		return false
	}
	if len(parts) >= 2 {
		host := parts[len(parts)-2]
		// 形如 127.0.0.1:8080:80 时，主机端口是倒数第二段。
		if host != "" && !validPortNumber(host) {
			return false
		}
	}
	return true
}

func validPortNumber(value string) bool {
	number, err := strconv.Atoi(value)
	return err == nil && number >= 1 && number <= 65535
}

func validVolume(spec string) bool {
	parts := strings.Split(spec, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return false
	}
	host, container := parts[0], parts[1]
	if !strings.HasPrefix(container, "/") {
		return false
	}
	// 宿主机侧要么是绝对路径，要么是命名卷（不含斜杠）。
	if host == "" {
		return false
	}
	if strings.HasPrefix(host, "/") {
		// 绝对路径已由 Quote 保证不会成为单独的命令；这里只做形态检查。
	} else if strings.ContainsAny(host, " /") {
		return false
	}
	if len(parts) == 3 {
		// -v 的第三个字段是逗号分隔的挂载选项：ro/rw、SELinux 的 z/Z，
		// 以及部分存储驱动支持的缓存策略。逐项白名单，避免把任意字符串
		// 原样交给 docker。
		for _, option := range strings.Split(parts[2], ",") {
			switch option {
			case "ro", "rw", "z", "Z", "nocopy", "cached", "delegated", "consistent":
			default:
				return false
			}
		}
	}
	return true
}

// ContainerActionCommand 生成容器动作命令。action 取值见调用方白名单。
func ContainerActionCommand(id, action string, force bool) (string, error) {
	if !reName.MatchString(id) {
		return "", fmt.Errorf("容器标识不合法：%s", id)
	}
	switch action {
	case "start", "stop", "restart", "pause", "unpause":
		return "docker " + action + " " + Quote(id), nil
	case "remove":
		command := "docker rm"
		if force {
			command += " -f"
		}
		return command + " " + Quote(id), nil
	default:
		return "", fmt.Errorf("不支持的容器操作：%s", action)
	}
}

// LogsCommand 生成读取容器日志的命令。2>&1 是为了保住 stdout/stderr 的先后顺序。
func LogsCommand(id string, tail int) (string, error) {
	if !reName.MatchString(id) {
		return "", fmt.Errorf("容器标识不合法：%s", id)
	}
	if tail <= 0 {
		tail = 200
	}
	if tail > 2000 {
		tail = 2000
	}
	return fmt.Sprintf("docker logs --tail %d --timestamps %s 2>&1", tail, Quote(id)), nil
}

// PullCommand 拉取镜像。
func PullCommand(ref string) (string, error) {
	if !reImage.MatchString(ref) {
		return "", fmt.Errorf("镜像名不合法：%s", ref)
	}
	return "docker pull " + Quote(ref), nil
}

// RemoveImagesCommand 删除镜像。
func RemoveImagesCommand(refs []string, force bool) (string, error) {
	if len(refs) == 0 {
		return "", fmt.Errorf("请先选择要删除的镜像")
	}
	args := make([]string, 0, len(refs))
	for _, ref := range refs {
		if !reImage.MatchString(ref) {
			return "", fmt.Errorf("镜像名不合法：%s", ref)
		}
		args = append(args, Quote(ref))
	}
	command := "docker rmi"
	if force {
		command += " -f"
	}
	return command + " " + strings.Join(args, " "), nil
}

// SearchCommand 搜索镜像仓库。
func SearchCommand(term string) (string, error) {
	if !reImage.MatchString(term) {
		return "", fmt.Errorf("搜索词只能包含字母、数字、点、斜杠、冒号、@ 与短横线")
	}
	return "docker search --limit 25 --format '{{json .}}' " + Quote(term), nil
}

type searchLine struct {
	Name        string   `json:"Name"`
	Description string   `json:"Description"`
	Stars       flexInt  `json:"StarCount"`
	Official    flexBool `json:"IsOfficial"`
	Automated   flexBool `json:"IsAutomated"`
}

// ParseSearch 解析 docker search 输出。
func ParseSearch(raw string) ([]SearchHit, string) {
	lines, problem := jsonLines[searchLine](raw)
	out := make([]SearchHit, 0, len(lines))
	for _, item := range lines {
		out = append(out, SearchHit{
			Name:        item.Name,
			Description: item.Description,
			Stars:       int(item.Stars),
			Official:    bool(item.Official),
			Automated:   bool(item.Automated),
		})
	}
	return out, problem
}

// NetworkCreateCommand 创建网络。
func NetworkCreateCommand(name, driver, subnet, gateway string, internal bool) (string, error) {
	if !reName.MatchString(name) {
		return "", fmt.Errorf("网络名只能包含字母、数字、下划线、点和短横线")
	}
	args := []string{"docker", "network", "create"}
	if driver != "" {
		if !reName.MatchString(driver) {
			return "", fmt.Errorf("驱动名不合法：%s", driver)
		}
		args = append(args, "--driver", Quote(driver))
	}
	if subnet != "" {
		if _, _, err := net.ParseCIDR(subnet); err != nil {
			return "", fmt.Errorf("子网格式不合法：%s（应形如 172.20.0.0/16）", subnet)
		}
		args = append(args, "--subnet", Quote(subnet))
	}
	if gateway != "" {
		if net.ParseIP(gateway) == nil {
			return "", fmt.Errorf("网关格式不合法：%s", gateway)
		}
		args = append(args, "--gateway", Quote(gateway))
	}
	if internal {
		args = append(args, "--internal")
	}
	args = append(args, Quote(name))
	return strings.Join(args, " "), nil
}

// NetworkRemoveCommand 删除网络。
func NetworkRemoveCommand(name string) (string, error) {
	if !reName.MatchString(name) {
		return "", fmt.Errorf("网络名不合法：%s", name)
	}
	return "docker network rm " + Quote(name), nil
}

// NetworkInspectCommand 读取一个网络的详情。
func NetworkInspectCommand(name string) (string, error) {
	if !reName.MatchString(name) {
		return "", fmt.Errorf("网络名不合法：%s", name)
	}
	return "docker network inspect " + Quote(name), nil
}

// NetworkConnectCommand 把容器接入或断开某个网络。
func NetworkConnectCommand(name, container string, disconnect bool) (string, error) {
	if !reName.MatchString(name) {
		return "", fmt.Errorf("网络名不合法：%s", name)
	}
	if !reName.MatchString(container) {
		return "", fmt.Errorf("容器标识不合法：%s", container)
	}
	sub := "connect"
	if disconnect {
		sub = "disconnect"
	}
	return "docker network " + sub + " " + Quote(name) + " " + Quote(container), nil
}

// composeCLI 选一条可用的 compose 命令。
//
// v2 是 `docker compose` 插件，v1 是独立的 `docker-compose`。两者的子命令基本
// 一致，因此只在入口处选一次，下面的命令体不必分叉。
func composeCLI() string {
	return `if docker compose version >/dev/null 2>&1; then C="docker compose"; ` +
		`elif command -v docker-compose >/dev/null 2>&1; then C="docker-compose"; ` +
		`else echo '目标机上没有 docker compose' >&2; exit 127; fi; `
}

func composeFileArgs(files []string) (string, error) {
	if len(files) == 0 {
		return "", fmt.Errorf("这个项目没有记录 compose 文件路径，无法执行操作")
	}
	args := make([]string, 0, len(files)*2)
	for _, file := range files {
		if !strings.HasPrefix(file, "/") {
			return "", fmt.Errorf("compose 文件必须是绝对路径：%s", file)
		}
		args = append(args, "-f", Quote(file))
	}
	return strings.Join(args, " "), nil
}

func composeCommand(project string, files []string, tail string) (string, error) {
	fileArgs, err := composeFileArgs(files)
	if err != nil {
		return "", err
	}
	command := composeCLI() + "$C " + fileArgs
	if project != "" {
		if !reProject.MatchString(project) {
			return "", fmt.Errorf("项目名只能包含小写字母、数字、下划线和短横线")
		}
		command += " -p " + Quote(project)
	}
	return command + " " + tail, nil
}

// ComposeActionCommand 生成项目级操作命令。
func ComposeActionCommand(project string, files []string, action string) (string, error) {
	switch action {
	case "up":
		return composeCommand(project, files, "up -d")
	case "down":
		return composeCommand(project, files, "down")
	case "stop", "start", "restart", "pull":
		return composeCommand(project, files, action)
	case "logs":
		return composeCommand(project, files, "logs --tail 200 --no-color")
	default:
		return "", fmt.Errorf("不支持的 compose 操作：%s", action)
	}
}

// ComposeUpCommand 是"保存 compose 文件后立即部署"用的命令。
func ComposeUpCommand(project, file string) (string, error) {
	return composeCommand(project, []string{file}, "up -d")
}

// ContainerFile 是容器内目录里的一个条目。
type ContainerFile struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Dir  bool   `json:"dir"`
	Link bool   `json:"link"`
	Size int64  `json:"size"`
}

// ContainerFiles 是一次容器内目录列表的结果。
type ContainerFiles struct {
	Path      string          `json:"path"`
	Parent    string          `json:"parent"`
	Entries   []ContainerFile `json:"entries"`
	Truncated bool            `json:"truncated"`
}

// 容器内单次列出的条目上限。
//
// 提权路径的单步输出上限是 64 KiB：条目再多也传不回来，还会把上层的截断标记
// 一起冲掉。800 条对浏览目录已经足够，超出时明确告诉用户被截断了。
const maxContainerEntries = 800

// NormalizeContainerPath 校验并规范化容器内路径。
//
// 容器里的路径永远是 Unix 风格，不能套用 filepath（在别的平台上语义不同）。
func NormalizeContainerPath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "/", nil
	}
	if !strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("容器内路径必须是绝对路径")
	}
	if strings.ContainsRune(raw, 0) {
		return "", fmt.Errorf("路径包含非法字符")
	}
	return path.Clean(raw), nil
}

// ListContainerFilesCommand 列出容器内一个目录的条目。
//
// 用容器自己的 sh 生成"类型/大小/名字"三列，而不是解析 ls 输出：
// ls 的列宽、日期格式、颜色控制符在不同发行版与 locale 下都不一样。
// 以 root（-u 0）执行，避免容器默认用户读不到系统目录。
func ListContainerFilesCommand(containerID, dir string) (string, error) {
	if !reName.MatchString(containerID) {
		return "", fmt.Errorf("容器标识不合法：%s", containerID)
	}
	clean, err := NormalizeContainerPath(dir)
	if err != nil {
		return "", err
	}
	script := `dir=$1
[ -d "$dir" ] || { echo '__PANELSSH_NO_DIR__' >&2; exit 3; }
i=0
for f in "$dir"/* "$dir"/.[!.]* "$dir"/..?*; do
  [ -e "$f" ] || [ -L "$f" ] || continue
  if [ "$i" -ge ` + strconv.Itoa(maxContainerEntries) + ` ]; then echo '__PANELSSH_TRUNCATED__'; break; fi
  if [ -L "$f" ]; then
    if [ -d "$f" ]; then t=ld; else t=l; fi
  elif [ -d "$f" ]; then t=d; else t=f; fi
  size=0
  if [ -f "$f" ]; then size=$(wc -c < "$f" 2>/dev/null || echo 0); fi
  printf '%s\t%s\t%s\n' "$t" "$size" "${f##*/}"
  i=$((i+1))
done
exit 0`
	return "docker exec -u 0 " + Quote(containerID) + " sh -c " + Quote(script) + " sh " + Quote(clean), nil
}

// ReadContainerFileCommand 读取容器内的一个文件（最多 256 KiB）。
//
// 在容器内就截断，而不是把整个文件经 SSH 拉回来再由面板丢弃：日志、数据库转储
// 这类文件可能有几个 GB，先传完再截断会把一条会话占死。
func ReadContainerFileCommand(containerID, file string) (string, error) {
	if !reName.MatchString(containerID) {
		return "", fmt.Errorf("容器标识不合法：%s", containerID)
	}
	clean, err := NormalizeContainerPath(file)
	if err != nil {
		return "", err
	}
	script := `[ -f "$1" ] || { echo '__PANELSSH_NOT_FILE__' >&2; exit 3; }
size=$(wc -c < "$1" 2>/dev/null || echo 0)
head -c 262144 "$1"
if [ "$size" -gt 262144 ]; then echo '__PANELSSH_TRUNCATED__' >&2; fi
exit 0`
	return "docker exec -u 0 " + Quote(containerID) + " sh -c " + Quote(script) + " sh " + Quote(clean), nil
}

// ParseContainerFiles 解析 ListContainerFilesCommand 的输出。
func ParseContainerFiles(raw, dir string) ContainerFiles {
	clean := path.Clean(dir)
	out := ContainerFiles{Path: clean, Entries: []ContainerFile{}}
	if clean != "/" {
		out.Parent = path.Dir(clean)
	}
	if strings.Contains(raw, "[输出超过上限，已截断]") {
		out.Truncated = true
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSuffix(line, "\r")
		switch line {
		case "":
			continue
		case "__PANELSSH_TRUNCATED__":
			out.Truncated = true
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 || parts[2] == "" {
			continue
		}
		size, _ := strconv.ParseInt(parts[1], 10, 64)
		out.Entries = append(out.Entries, ContainerFile{
			Name: parts[2],
			Path: path.Join(clean, parts[2]),
			// ld 是"指向目录的符号链接"：界面上要能点进去。
			Dir:  parts[0] == "d" || parts[0] == "ld",
			Link: parts[0] == "l" || parts[0] == "ld",
			Size: size,
		})
	}
	sort.Slice(out.Entries, func(i, j int) bool {
		if out.Entries[i].Dir != out.Entries[j].Dir {
			return out.Entries[i].Dir
		}
		return strings.ToLower(out.Entries[i].Name) < strings.ToLower(out.Entries[j].Name)
	})
	return out
}
