package api

import (
	"net/http"
	"os/user"
	"strings"
)

// 宿主机提权账号。
//
// 宿主机不在主机表里（它是保留 ID 0，没有地址、也不能被删除），但"改 /etc"
// 这类操作同样需要 root。这里给它一份与应用运行用户绑定的提权口令：
//   - 账号 root      → su（口令是 root 自己的）
//   - 账号为运行用户 → sudo -S（口令是该用户的 sudo 口令）
//
// 口令与其他主机凭据一样用主密钥加密后落库，任何接口都不回填明文。

type localPrivilegeView struct {
	Account    string `json:"account"`
	Configured bool   `json:"configured"`
}

type localPrivilegeRequest struct {
	Account string `json:"account"`
	Secret  string `json:"secret"`
}

// localPrivilege 读取并解密宿主机的提权账号；没配置时 ok 为假。
//
// 没有存储时视为未配置：宿主机目标本来就不依赖主机库（见 TestExecEscalatedIntentContract
// 里直接用裸 Server 验证本机通道），这条分支不能因为读配置而 panic。
func (s *Server) localPrivilege() (account, secret string, ok bool) {
	if s.store == nil {
		return "", "", false
	}
	account, enc, err := s.store.GetLocalPrivilege()
	if err != nil || enc == "" {
		return "", "", false
	}
	pw, err := s.box.Decrypt(enc)
	if err != nil || pw == "" {
		return "", "", false
	}
	return account, pw, true
}

// handleLocalPrivilege 读取、保存与清除宿主机的提权账号。
func (s *Server) handleLocalPrivilege(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		account, _, ok := s.localPrivilege()
		writeJSON(w, http.StatusOK, localPrivilegeView{Account: account, Configured: ok})

	case http.MethodDelete:
		if err := s.store.ClearLocalPrivilege(); err != nil {
			writeErr(w, http.StatusInternalServerError, "清除提权账号失败："+err.Error())
			return
		}
		// 画像里记着"能不能提权"，不清掉会继续按旧结论判断。
		s.profiles.forget(LocalHostID)
		writeJSON(w, http.StatusOK, localPrivilegeView{})

	default:
		var req localPrivilegeRequest
		if err := decodeJSONBody(w, r, &req, 1<<16); err != nil {
			return
		}
		account := strings.TrimSpace(req.Account)
		if account == "" {
			account = "root"
		}
		// sudo 只能以执行者自己的身份提权，所以账号只有两种合法取值；
		// 放开任意账号只会让用户填完才发现提不了权。
		current, err := user.Current()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "读取当前运行用户失败："+err.Error())
			return
		}
		if account != "root" && account != current.Username {
			writeErr(w, http.StatusBadRequest,
				"提权账号只能是 root 或面板运行用户 "+current.Username)
			return
		}
		if req.Secret == "" {
			writeErr(w, http.StatusBadRequest, "提权口令不能为空")
			return
		}

		enc, err := s.box.Encrypt(req.Secret)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "加密提权口令失败："+err.Error())
			return
		}
		if err := s.store.SaveLocalPrivilege(account, enc); err != nil {
			writeErr(w, http.StatusInternalServerError, "保存提权账号失败："+err.Error())
			return
		}
		s.profiles.forget(LocalHostID)
		writeJSON(w, http.StatusOK, localPrivilegeView{Account: account, Configured: true})
	}
}
