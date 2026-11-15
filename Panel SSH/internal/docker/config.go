package docker

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// 本文件负责目标机的镜像加速与代理配置。
//
// 这两项都不是"面板自己的设置"：docker pull 由目标机的 dockerd 发起，
// 面板进程里的 HTTP_PROXY 传不过去。唯一生效的位置是 dockerd 的配置，
// 所以这里直接改 /etc/docker/daemon.json，并在改完后重启 Docker。

// DaemonConfigPath 是系统级 Docker 的守护进程配置。
//
// 只支持系统级：rootless Docker 的配置在用户目录下，重启方式也不同，
// 面板的 Docker 模块目前没有区分这两种部署形态。
const DaemonConfigPath = "/etc/docker/daemon.json"

// 输出标记：把"文件不存在"与"文件为空"分开，否则无法判断该不该新建。
const (
	daemonConfigMarker      = "__PANELSSH_DAEMON_CONFIG__"
	daemonConfigMissing     = "__PANELSSH_DAEMON_CONFIG_MISSING__"
	daemonRestartedMarker   = "__PANELSSH_DOCKER_RESTARTED__"
	daemonRolledBackMarker  = "__PANELSSH_DOCKER_ROLLED_BACK__"
	daemonMaxMirrors        = 10
	daemonMaxEndpointLength = 512
	daemonProxyMinMajor     = 23
)

// DaemonConfig 是 daemon.json 里与镜像拉取相关的部分。
type DaemonConfig struct {
	Mirrors []string `json:"mirrors"`
	// Proxy 同时用于 HTTP 与 HTTPS；为空表示没有配置代理。
	Proxy   string `json:"proxy"`
	NoProxy string `json:"noProxy"`
	// HasFile 为 false 表示目标机上还没有 daemon.json。
	HasFile bool   `json:"hasFile"`
	Path    string `json:"path"`
}

// DaemonConfigUpdate 是界面提交的新值。
type DaemonConfigUpdate struct {
	Mirrors []string `json:"mirrors"`
	Proxy   string   `json:"proxy"`
	NoProxy string   `json:"noProxy"`
}

// DaemonConfigReadCommand 读取 daemon.json。
func DaemonConfigReadCommand() string {
	return fmt.Sprintf(
		"if [ -f %s ]; then echo %s; cat %s; else echo %s; fi",
		Quote(DaemonConfigPath),
		Quote(daemonConfigMarker),
		Quote(DaemonConfigPath),
		Quote(daemonConfigMissing),
	)
}

// ParseDaemonConfig 解析读取命令的输出。
//
// 返回原始 map 是为了保存时能保留用户自己写的其他字段（log-driver、data-root 等），
// 只覆盖本界面管理的三项。
func ParseDaemonConfig(out string) (DaemonConfig, map[string]any, error) {
	cfg := DaemonConfig{Path: DaemonConfigPath, Mirrors: []string{}}
	text := strings.TrimSpace(out)

	if strings.HasPrefix(text, daemonConfigMissing) {
		return cfg, map[string]any{}, nil
	}
	text = strings.TrimSpace(strings.TrimPrefix(text, daemonConfigMarker))
	cfg.HasFile = true
	if text == "" {
		return cfg, map[string]any{}, nil
	}

	var raw map[string]any
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return cfg, nil, fmt.Errorf("%s 不是合法 JSON，已拒绝覆盖：%w", DaemonConfigPath, err)
	}
	if raw == nil {
		raw = map[string]any{}
	}

	if list, ok := raw["registry-mirrors"].([]any); ok {
		for _, item := range list {
			if mirror, ok := item.(string); ok {
				cfg.Mirrors = append(cfg.Mirrors, mirror)
			}
		}
	}

	if proxies, ok := raw["proxies"].(map[string]any); ok {
		cfg.Proxy = firstString(proxies["https-proxy"], proxies["http-proxy"])
		cfg.NoProxy, _ = proxies["no-proxy"].(string)
	}
	return cfg, raw, nil
}

func firstString(values ...any) string {
	for _, value := range values {
		if text, ok := value.(string); ok && text != "" {
			return text
		}
	}
	return ""
}

// MergeDaemonConfig 把界面提交的值合并进原有配置。
//
// 只动 registry-mirrors 与 proxies 里的三个键，其余字段原样保留 ——
// daemon.json 里可能有 data-root 这类一改就影响所有镜像位置的配置，
// 面板不能因为用户来设个加速源就把它抹掉。
func MergeDaemonConfig(raw map[string]any, update DaemonConfigUpdate) ([]byte, error) {
	if raw == nil {
		raw = map[string]any{}
	}

	mirrors, err := normalizeMirrors(update.Mirrors)
	if err != nil {
		return nil, err
	}
	proxy, err := normalizeProxy(update.Proxy)
	if err != nil {
		return nil, err
	}
	noProxy := strings.TrimSpace(update.NoProxy)
	if strings.ContainsAny(noProxy, "\r\n") {
		return nil, fmt.Errorf("NO_PROXY 不能包含换行")
	}

	if len(mirrors) > 0 {
		raw["registry-mirrors"] = mirrors
	} else {
		delete(raw, "registry-mirrors")
	}

	proxies, _ := raw["proxies"].(map[string]any)
	if proxies == nil {
		proxies = map[string]any{}
	}
	if proxy != "" {
		proxies["http-proxy"] = proxy
		proxies["https-proxy"] = proxy
		if noProxy != "" {
			proxies["no-proxy"] = noProxy
		} else {
			delete(proxies, "no-proxy")
		}
	} else {
		delete(proxies, "http-proxy")
		delete(proxies, "https-proxy")
		delete(proxies, "no-proxy")
	}
	if len(proxies) == 0 {
		delete(raw, "proxies")
	} else {
		raw["proxies"] = proxies
	}

	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func normalizeMirrors(items []string) ([]string, error) {
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		mirror := strings.TrimSpace(item)
		if mirror == "" {
			continue
		}
		if len(mirror) > daemonMaxEndpointLength {
			return nil, fmt.Errorf("加速源地址过长：%s", mirror)
		}
		parsed, err := url.Parse(mirror)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return nil, fmt.Errorf("加速源必须是 http(s) 地址：%s", mirror)
		}
		if seen[mirror] {
			continue
		}
		seen[mirror] = true
		out = append(out, mirror)
		if len(out) > daemonMaxMirrors {
			return nil, fmt.Errorf("加速源最多 %d 个", daemonMaxMirrors)
		}
	}
	return out, nil
}

func normalizeProxy(value string) (string, error) {
	proxy := strings.TrimSpace(value)
	if proxy == "" {
		return "", nil
	}
	if len(proxy) > daemonMaxEndpointLength {
		return "", fmt.Errorf("代理地址过长")
	}
	parsed, err := url.Parse(proxy)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("代理地址不合法：%s", proxy)
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", fmt.Errorf("代理只支持 http / https / socks5：%s", proxy)
	}
	return proxy, nil
}

// ProxySupported 判断 dockerd 是否支持 daemon.json 里的 proxies 字段。
//
// Docker 23.0 之前写进去会被当成未知字段，dockerd 直接起不来。
// 版本解析不出来时按不支持处理：宁可让用户手动配 systemd 代理，
// 也不要拿一台机器的容器去赌。
func ProxySupported(version string) bool {
	match := reVersion.FindString(version)
	if match == "" {
		return false
	}
	majorText, _, _ := strings.Cut(match, ".")
	major, err := strconv.Atoi(majorText)
	if err != nil {
		return false
	}
	return major >= daemonProxyMinMajor
}

// DaemonConfigApplyCommand 生成"写入配置 + 重启 Docker + 失败回滚"的脚本。
//
// 回滚是必须的：daemon.json 写坏会让 dockerd 起不来，而重启失败时容器已经
// 全被停掉了 —— 如果不把文件恢复，目标机会一直卡在没有 Docker 的状态。
func DaemonConfigApplyCommand(payload []byte) string {
	encoded := base64.StdEncoding.EncodeToString(payload)
	backup := DaemonConfigPath + ".panelssh.bak"
	tmp := DaemonConfigPath + ".panelssh.tmp"

	// 注意 printf 的 %s 要写成 %%s：这段字符串本身会被 Sprintf 再格式化一次。
	script := `set -u
CONFIG=%s
BACKUP=%s
TMP=%s
if [ -f "$CONFIG" ]; then cp -a "$CONFIG" "$BACKUP" || exit 1; fi
printf '%%s' %s | base64 -d > "$TMP" || exit 1
chmod 644 "$TMP" || exit 1
mv "$TMP" "$CONFIG" || exit 1
restart_docker() {
  if command -v systemctl >/dev/null 2>&1; then
    systemctl restart docker 2>&1
  else
    service docker restart 2>&1
  fi
}
if restart_docker; then
  echo %s
  exit 0
fi
if [ -f "$BACKUP" ]; then
  cp -a "$BACKUP" "$CONFIG"
else
  rm -f "$CONFIG"
fi
restart_docker >/dev/null 2>&1
echo %s
exit 1
`
	return fmt.Sprintf(script,
		Quote(DaemonConfigPath),
		Quote(backup),
		Quote(tmp),
		Quote(encoded),
		Quote(daemonRestartedMarker),
		Quote(daemonRolledBackMarker),
	)
}
