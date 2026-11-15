package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"panelssh/internal/config"
	"panelssh/internal/sysinfo"
)

// 卸载 OVS 开机自恢复是一次有副作用的写操作：它会删掉目标机上的 systemd
// 单元与状态文件。这里走真实路由表把三件事一起钉住 —— 路径注册对不对、
// 鉴权在不在、宿主机被不被挡。只调 handler 是测不出路径写错这类问题的。
func TestOvsUnpersistRouteContract(t *testing.T) {
	newServer := func(t *testing.T) *Server {
		t.Helper()
		return &Server{cfg: &config.Config{DevMode: true}, sessions: newTestSessionManager(t)}
	}

	t.Run("未登录被挡下", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/hosts/0/network/ovs/unpersist", nil)
		req.Header.Set(csrfHeader, "1")

		w := httptest.NewRecorder()
		newServer(t).Router().ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("未登录应被挡下（401），得到 %d：%s", w.Code, w.Body.String())
		}
	})

	t.Run("路径已注册且宿主机被拒", func(t *testing.T) {
		s := newServer(t)
		issued := httptest.NewRecorder()
		s.sessions.Issue(issued, httptest.NewRequest(http.MethodPost, "/api/auth/login", nil), 1)

		req := httptest.NewRequest(http.MethodPost, "/api/hosts/0/network/ovs/unpersist", nil)
		req.Header.Set(csrfHeader, "1")
		for _, c := range issued.Result().Cookies() {
			req.AddCookie(c)
		}

		w := httptest.NewRecorder()
		s.Router().ServeHTTP(w, req)

		// 404 说明路径压根没注册；这里要看到的是"到了 handler，并且被挡在
		// 宿主机这道门上" —— 宿主机没有 SSH，删不了远端的东西。
		if w.Code != http.StatusBadRequest {
			t.Fatalf("宿主机应得到 400，得到 %d：%s", w.Code, w.Body.String())
		}
		var body struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("响应不是合法 JSON：%v（%s）", err, w.Body.String())
		}
		if !strings.Contains(body.Error, "宿主机") {
			t.Fatalf("拒绝的理由应当指向宿主机，实际：%q", body.Error)
		}
	})
}

// TestIsOvsChange 钉住"哪类变更用哪套保护"这条分界。
//
// 判错的后果是不对称的：把 NetworkManager 类变更误判成 OVS，它会既拿不到
// NM 检查点、也不会建立自建保护，变成一次"看起来有保护、实际没有"的改动 ——
// 而那恰恰是这个功能最不能出的错。因此两个方向都要测。
func TestIsOvsChange(t *testing.T) {
	for _, action := range []string{
		sysinfo.NetChangeOvsAttach,
		sysinfo.NetChangeOvsDetach,
		sysinfo.NetChangeOvsBridgeCreate,
		sysinfo.NetChangeOvsBridgeDelete,
	} {
		if !isOvsChange(action) {
			t.Fatalf("%s 应当走 OVS 自建保护", action)
		}
	}

	for _, action := range []string{
		sysinfo.NetChangeAddress,
		sysinfo.NetChangeMTU,
		sysinfo.NetChangeLinkState,
		sysinfo.NetChangeBridgeCreate,
		sysinfo.NetChangeBridgeDelete,
		sysinfo.NetChangeBridgeAttach,
		sysinfo.NetChangeBridgeDetach,
	} {
		if isOvsChange(action) {
			t.Fatalf("%s 必须走 NetworkManager 检查点", action)
		}
	}
}

func TestChangeScriptStructure(t *testing.T) {
	script := changeScript("abc123", []string{
		"nmcli con mod 'Wired connection 1' master 'br0'",
		"nmcli con up 'br0'",
	})

	// 生成的是"写脚本 + 启动脚本"两件事，逐项确认关键结构都在。
	checks := []string{
		"cat > /tmp/panelssh-net-abc123.sh <<'PANELSSH_SCRIPT_EOF'",
		`rm -f "$0"`,
		"exec >>/tmp/panelssh-net-abc123.log 2>&1",
		"set -x",
		"sleep 3",
		"nmcli con mod 'Wired connection 1' master 'br0'",
		"nmcli con up 'br0'",
		"chmod +x /tmp/panelssh-net-abc123.sh",
		"setsid /tmp/panelssh-net-abc123.sh",
	}
	for _, want := range checks {
		if !strings.Contains(script, want) {
			t.Fatalf("脚本缺少 %q：\n%s", want, script)
		}
	}

	// 结束标记必须落在所有命令之后。写在命令中间会把后面的内容当成脚本文本，
	// 结果是"启动了一个什么都没干的脚本"。
	end := strings.Index(script, "\nPANELSSH_SCRIPT_EOF\n")
	if end < 0 {
		t.Fatalf("脚本没有正确的结束标记：\n%s", script)
	}
	if up := strings.Index(script, "nmcli con up 'br0'"); up > end {
		t.Fatalf("有命令落在 heredoc 结束标记之后：\n%s", script)
	}

	// setsid 必须在 heredoc 之外，否则它只会被写进脚本文本而不被执行。
	if strings.Index(script, "setsid") < end {
		t.Fatalf("setsid 落在 heredoc 内部：\n%s", script)
	}
}

func TestNetChangeStoreExpiry(t *testing.T) {
	store := newNetChangeStore()
	now := time.Now()

	store.put(&networkChange{HostID: 1, ExpiresAt: now.Add(2 * time.Minute)})
	if store.active(1, now) == nil {
		t.Fatal("窗口内的变更应当被视为待确认")
	}

	// 窗口一过，NM 会自己回滚，面板这边不能再把它当"待确认"，
	// 否则界面会被一条永远不会生效的提示一直占着。
	store.put(&networkChange{HostID: 1, ExpiresAt: now.Add(-time.Second)})
	if store.active(1, now) != nil {
		t.Fatal("已过窗口的变更不应再被视为待确认")
	}
	// 但记录要留着：用户仍可能手动回滚，或查看它的执行日志。
	if store.get(1) == nil {
		t.Fatal("过期不等于删除，记录应当保留")
	}

	store.delete(1)
	if store.get(1) != nil {
		t.Fatal("删除后不应再查到")
	}
	if store.active(2, now) != nil {
		t.Fatal("没有记录的主机应当返回空")
	}
}

func TestNewChangeIDIsSafe(t *testing.T) {
	// 这个 ID 会进入远端文件名与脚本内容，必须只含十六进制字符。
	for i := 0; i < 16; i++ {
		id, err := newChangeID()
		if err != nil {
			t.Fatalf("生成失败：%v", err)
		}
		if len(id) != 16 {
			t.Fatalf("长度不是 16：%q", id)
		}
		for _, c := range id {
			if !strings.ContainsRune("0123456789abcdef", c) {
				t.Fatalf("含有非十六进制字符：%q", id)
			}
		}
	}
}
