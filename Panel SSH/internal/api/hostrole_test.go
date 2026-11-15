package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"panelssh/internal/config"
	"panelssh/internal/secretbox"
	"panelssh/internal/sshclient"
	"panelssh/internal/store"
	"panelssh/internal/sysinfo"
)

// 「角色」列靠真的登录目标机才得出结论，因此接口必须把两种"没有权限"分开：
//   - 探到了，但账号在这台机器上没法提权 → env.PrivNone（界面显示"普通"）；
//   - 根本没探到（离线/凭据错/超时）     → 空串（界面显示 —）。
//
// 把后者当成前者，会把一台连不上的机器安静地标成"普通账号"——用户据此
// 判断"这台机器权限不够"，而事实是根本没连上。
func TestHostRolesKeepsUnknownApart(t *testing.T) {
	dir := t.TempDir()

	st := newTestStore(t)
	// 指向一个必然连不上的端口，且凭据为空：探测必定失败。
	id, err := st.CreateHost(store.HostInput{
		Name: "连不上的机器", Addr: "127.0.0.1", Port: 1, Username: "root",
		AuthType: sshclient.AuthKey, Group: "", Remark: "",
	})
	if err != nil {
		t.Fatalf("准备主机失败：%v", err)
	}

	box, err := secretbox.Load(filepath.Join(dir, "panel.key"))
	if err != nil {
		t.Fatalf("准备密钥箱失败：%v", err)
	}
	s := NewServer(
		&config.Config{DataDir: dir, DevMode: true}, st, box, newTestSessionManager(t),
		sshclient.NewPool(filepath.Join(dir, "known_hosts")), sysinfo.NewCollector(),
	)

	r := httptest.NewRequest(http.MethodGet, "/api/hosts/roles", nil)
	w := httptest.NewRecorder()
	s.handleHostRoles(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("角色接口应以 200 返回（单台连不上不该让整列变错误页），得到 %d：%s", w.Code, w.Body.String())
	}
	var got []hostRole
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON：%v（%s）", err, w.Body.String())
	}
	if len(got) != 1 || got[0].ID != id {
		t.Fatalf("应逐台返回一条结果，得到 %+v", got)
	}
	if got[0].PrivilegeMode != "" {
		t.Fatalf("探测失败应当是未知（空串），得到 %q —— 这会把连不上的机器说成「普通账号」", got[0].PrivilegeMode)
	}
}
