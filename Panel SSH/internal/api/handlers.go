package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"panelssh/internal/auth"
	"panelssh/internal/secretbox"
	"panelssh/internal/sshclient"
	"panelssh/internal/store"
)

// ---------- 认证 ----------

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	user, err := s.store.GetUserByUsername(strings.TrimSpace(req.Username))
	// 用户不存在与密码错误返回同一提示，避免账号枚举。
	// 取不到用户时也要执行一次真实的哈希比对：否则两条路径的响应耗时
	// 差异本身就是一个可测量的账号枚举旁路。
	var hash string
	if err == nil {
		hash = user.PasswordHash
	}
	if !auth.CheckPasswordConstantTime(hash, req.Password) || err != nil {
		writeErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}

	s.sessions.Issue(w, r, user.ID)
	writeJSON(w, http.StatusOK, map[string]any{"id": user.ID, "username": user.Username})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.sessions.Clear(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.sessions.Current(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "未登录")
		return
	}
	user, err := s.store.GetUserByID(userID)
	if err != nil {
		s.sessions.Clear(w, r)
		writeErr(w, http.StatusUnauthorized, "会话对应的账号不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": user.ID, "username": user.Username})
}

// 新密码的最小长度。面板会凭这些凭据登录生产服务器，
// 弱口令的影响面远大于普通 Web 应用。
const minPasswordLen = 10

type passwordRequest struct {
	OldPassword string `json:"oldPassword"`
	NewPassword string `json:"newPassword"`
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserFrom(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "未登录")
		return
	}

	var req passwordRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	if len([]rune(req.NewPassword)) < minPasswordLen {
		writeErr(w, http.StatusBadRequest, "新密码至少需要 "+strconv.Itoa(minPasswordLen)+" 个字符")
		return
	}

	user, err := s.store.GetUserByID(userID)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "账号不存在")
		return
	}
	if !auth.CheckPassword(user.PasswordHash, req.OldPassword) {
		writeErr(w, http.StatusForbidden, "原密码不正确")
		return
	}

	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成密码哈希失败")
		return
	}
	if err := s.store.UpdateUserPassword(userID, hash); err != nil {
		writeErr(w, http.StatusInternalServerError, "更新密码失败")
		return
	}

	// 会话是无状态签名 Cookie，改密码本身不会让已泄露的会话失效。
	// 轮换会话签名密钥作废全部历史会话，再给当前浏览器补发一个。
	//
	// 密码哈希与新的 epoch 必须在同一个事务里落库：分成两步写时，
	// 若第二步失败就会出现"密码已改、但历史会话一个都没失效"的不一致状态。
	epoch, err := secretbox.RandomToken(24)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成会话密钥失败")
		return
	}
	if err := s.store.ChangePasswordAndSetMeta(userID, hash, metaSessionEpoch, epoch); err != nil {
		writeErr(w, http.StatusInternalServerError, "更新密码失败")
		return
	}
	s.sessions.Rotate(sessionKey(s.box, epoch))
	s.sessions.Issue(w, r, userID)

	s.notifySecurity(store.LevelWarning, "面板密码已修改",
		"密码已更新，其他已登录的会话已全部失效")

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- 主机 ----------

// hostPayload 是前端提交的主机表单。
type hostPayload struct {
	Name     string `json:"name"`
	Addr     string `json:"addr"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	AuthType string `json:"authType"`
	Secret   string `json:"secret"`
	// EscalateSecret 是提权口令（root 口令）；留空在新增时不设置，
	// 在编辑时表示保持原值（与 Secret 的语义一致）。
	EscalateSecret string `json:"escalateSecret"`
	Group          string `json:"group"`
	Remark         string `json:"remark"`
}

func (s *Server) handleListHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.store.ListHosts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询主机失败")
		return
	}
	writeJSON(w, http.StatusOK, hosts)
}

func (s *Server) handleCreateHost(w http.ResponseWriter, r *http.Request) {
	req, ok := s.decodeHostPayload(w, r)
	if !ok {
		return
	}
	// 凭据是可选的：允许先把主机登记进来（地址、端口、用户名），凭据以后再补。
	// 这类主机在列表里标记为"未配置凭据"，连接类操作会被明确挡下。
	// 空串经 Encrypt 原样返回空串，因此 secret_enc 为空正是"没有凭据"的可靠标记。
	secretEnc, err := s.box.Encrypt(req.Secret)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "加密凭据失败")
		return
	}
	escalateEnc, err := encryptOptional(s.box, req.EscalateSecret)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "加密提权口令失败")
		return
	}

	id, err := s.store.CreateHost(store.HostInput{
		Name:        req.Name,
		Addr:        req.Addr,
		Port:        req.Port,
		Username:    req.Username,
		AuthType:    req.AuthType,
		SecretEnc:   secretEnc,
		EscalateEnc: escalateEnc,
		Group:       req.Group,
		Remark:      req.Remark,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "写入主机失败")
		return
	}

	host, err := s.store.GetHost(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取新建主机失败")
		return
	}

	s.notifySecurity(store.LevelInfo, fmt.Sprintf("新增主机：%s", host.Name),
		fmt.Sprintf("%s:%d · 登录用户 %s", host.Addr, host.Port, host.Username))

	writeJSON(w, http.StatusCreated, host)
}

func (s *Server) handleUpdateHost(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	req, ok := s.decodeHostPayload(w, r)
	if !ok {
		return
	}

	// 两份口令都按"未填即保持不变"处理，由 store 保留原密文。
	secretEnc, err := encryptOptional(s.box, req.Secret)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "加密凭据失败")
		return
	}
	escalateEnc, err := encryptOptional(s.box, req.EscalateSecret)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "加密提权口令失败")
		return
	}

	err = s.store.UpdateHost(id, store.HostInput{
		Name:        req.Name,
		Addr:        req.Addr,
		Port:        req.Port,
		Username:    req.Username,
		AuthType:    req.AuthType,
		SecretEnc:   secretEnc,
		EscalateEnc: escalateEnc,
		Group:       req.Group,
		Remark:      req.Remark,
	})
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "主机不存在")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "更新主机失败")
		return
	}

	host, err := s.store.GetHost(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取主机失败")
		return
	}
	// 地址或凭据可能已变，丢弃旧连接与采样基准，下一次请求按新配置重建。
	s.pool.Invalidate(id)
	s.collector.Forget(id)
	s.metrics.forget(id)
	// 指标流必须一起停：它是按旧地址、旧凭据建立的长驻 SSH 会话，
	// 留着它既连不上新地址，还会继续把旧机器的数据当成现状推给界面。
	s.metricsStreams.forget(id)
	s.history.forget(id)
	s.procs.forget(id)
	s.forgetNetwork(id)
	s.netCollector.Forget(id)
	s.netChanges.delete(id)
	s.storage.forget(id)
	s.status.forget(id)
	// 账号角色也要重探：换了地址或凭据之后，旧账号的分类已经不属于这台机器。
	s.profiles.forget(id)
	// 判定基准也要丢：地址变了之后，按旧地址得出的在线状态已经没有意义，
	// 留着会让新地址的首次探测被当成"状态变化"。
	s.hostWatch.forget(id)
	writeJSON(w, http.StatusOK, host)
}

func (s *Server) handleDeleteHost(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}

	// 先取名字：删除之后就查不到了，而通知里要写明删掉的是哪台机器。
	// 取不到也不影响删除（可能是并发删除），此时退回用 ID 标识。
	name := ""
	if host, err := s.store.GetHost(id); err == nil {
		name = host.Name
	}

	err := s.store.DeleteHost(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "主机不存在")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "删除主机失败")
		return
	}
	s.pool.Invalidate(id)
	s.collector.Forget(id)
	s.metrics.forget(id)
	// 指标流必须一起停：主机都没了，那条长驻会话只会一直对着一个不存在的目标重连。
	s.metricsStreams.forget(id)
	s.history.forget(id)
	s.procs.forget(id)
	s.forgetNetwork(id)
	s.netCollector.Forget(id)
	s.netChanges.delete(id)
	s.storage.forget(id)
	s.status.forget(id)
	s.profiles.forget(id)
	// 判定状态必须一起丢：主键会被 SQLite 复用，残留的旧状态会让新建的主机
	// 带着上一台机器的在线基准开始，凭空造出一次状态变化。
	s.hostWatch.forget(id)

	label := name
	if label == "" {
		label = fmt.Sprintf("#%d", id)
	}
	s.notifySecurity(store.LevelWarning, fmt.Sprintf("删除主机：%s", label),
		"该主机的连接配置与凭据已从面板移除")

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// hostTestResult 是连接测试的统一响应体。
//
// 失败时 LatencyMs 为 0：连不上时用户要看的是原因，不是耗时。
// 这里刻意不给它加 omitempty —— 握手恰好 0 毫秒（本机回环）是合法结果，
// 省略字段会让前端分不清"没有耗时"和"耗时为零"。
//
// 带小数的毫秒：本机与同网段机器握手常在 1 毫秒以内，取整只能得到 0。
type hostTestResult struct {
	OK        bool    `json:"ok"`
	Message   string  `json:"message"`
	LatencyMs float64 `json:"latencyMs"`
	// NeedSecret 表示这次只验证了网络可达，主机尚未配置凭据。
	// 前端据此跳过"账号角色"那一步 —— 没有凭据必然登录失败。
	NeedSecret bool `json:"needSecret"`
}

// probeHost 按给定配置连一次 SSH 并立刻断开，返回握手耗时。
//
// 计时覆盖 ssh.Dial 的全部三段：TCP 连接、密钥交换、身份认证。
// 这三段合起来才是用户感知的"这台机器连得快不快"。
func (s *Server) probeHost(t sshclient.Target) (time.Duration, error) {
	start := time.Now()
	client, err := sshclient.Dial(t, s.knownHosts, 10*time.Second)
	if err != nil {
		return 0, err
	}
	_ = client.Close()
	return time.Since(start), nil
}

func (s *Server) handleTestHost(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	host, err := s.store.GetHost(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "主机不存在")
		return
	}
	secret, err := s.hostSecret(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 没配凭据的主机只能验证网络可达性 —— 但 none / localkey 是另一回事：
	// 它们本来就不需要用户提供凭据（前者服务端放行，后者用本机密钥），
	// 必须真的连一次才能回答"能不能登进去"。
	// NeedSecret 让前端知道"别再问账号角色了"：凭据缺失时必然登录失败，
	// 再发一次探测只会多等一个超时，还会把"网络是通的"这件事盖成一条错误。
	if secret == "" && host.AuthType != sshclient.AuthNone && host.AuthType != sshclient.AuthLocalKey {
		status := probeHostPort(id, host.Addr, host.Port)
		if !status.Online {
			writeJSON(w, http.StatusOK, hostTestResult{
				OK:      false,
				Message: fmt.Sprintf("无法连接 %s:%d，请检查地址、端口与网络", host.Addr, host.Port),
			})
			return
		}
		writeJSON(w, http.StatusOK, hostTestResult{
			OK:         true,
			Message:    "网络可达（未配置凭据，只验证了端口连通，未登录）",
			LatencyMs:  status.LatencyMs,
			NeedSecret: true,
		})
		return
	}

	elapsed, err := s.probeHost(sshclient.Target{
		Addr:     host.Addr,
		Port:     host.Port,
		Username: host.Username,
		AuthType: host.AuthType,
		Secret:   secret,
	})
	if err != nil {
		writeJSON(w, http.StatusOK, hostTestResult{OK: false, Message: err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, hostTestResult{
		OK:        true,
		Message:   "连接成功",
		LatencyMs: latencyMs(elapsed),
	})
}

// handleTestHostDraft 用表单里尚未保存的配置试连一次，不落库。
//
// 与 handleTestHost 的区别：那个测的是已保存的主机，这个测的是用户还没保存的草稿，
// 目的是在关闭弹窗之前就知道这套地址与凭据能不能连上。
//
// ID 是可选的，也是必要的：编辑已有主机时表单不回填凭据，"留空"表示沿用原凭据。
// 没有 ID 就取不回那份凭据，用户在编辑态每测一次都得把密码重输一遍。
//
// 这里不复用 decodeHostPayload：它强制要求 name，而连通性跟主机名叫什么无关，
// 拿"主机名称不能为空"去回应一次连接测试只会让人困惑。
func (s *Server) handleTestHostDraft(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       int64  `json:"id"`
		Addr     string `json:"addr"`
		Port     int    `json:"port"`
		Username string `json:"username"`
		AuthType string `json:"authType"`
		Secret   string `json:"secret"`
	}
	if err := decodeJSONBody(w, r, &req, 1<<20); err != nil {
		return
	}

	addr := strings.TrimSpace(req.Addr)
	username := strings.TrimSpace(req.Username)
	authType := req.AuthType
	if authType == "" {
		authType = sshclient.AuthPassword
	}
	port := req.Port
	if port <= 0 || port > 65535 {
		port = 22
	}

	// 凭据留空 = 沿用已保存主机的那一份。此时地址、端口、用户名、认证方式必须
	// 一并从库里取，不能继续用请求体里的值 —— 它们与凭据同属"一台主机"的属性，
	// 允许分开传等于允许拿 A 主机的凭据去连任意地址：既是凭据搬用，
	// 也让面板变成对内网任意地址的探测跳板。
	secret := req.Secret
	if secret == "" && req.ID > 0 {
		stored, err := s.store.GetHost(req.ID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeErr(w, http.StatusNotFound, "主机不存在")
				return
			}
			writeErr(w, http.StatusInternalServerError, "读取主机失败")
			return
		}
		addr, port, username, authType = stored.Addr, stored.Port, stored.Username, stored.AuthType
		if secret, err = s.hostSecret(req.ID); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	// 校验放在取值之后：编辑态留空凭据时，地址与用户名本来就来自库，
	// 拿请求体里的空值去报"请先填写主机地址"属于误报。
	switch {
	case addr == "":
		writeErr(w, http.StatusBadRequest, "请先填写主机地址")
		return
	case username == "":
		writeErr(w, http.StatusBadRequest, "请先填写登录用户名")
		return
	case authType != sshclient.AuthPassword && authType != sshclient.AuthKey &&
		authType != sshclient.AuthNone && authType != sshclient.AuthLocalKey:
		writeErr(w, http.StatusBadRequest, "认证方式仅支持 password、key、none 或 localkey")
		return
	}

	// 地址里混进用户名是最容易踩的一脚：从终端复制 `ssh -p 2222 user@host`
	// 之后，整段粘进「地址」是很自然的动作，而现象是 DNS 报 no such host ——
	// 用户会去查域名，而不是查自己填错了字段。这里直接说破。
	if strings.ContainsAny(addr, "@ ") {
		writeErr(w, http.StatusBadRequest, "「地址」只能填主机名或 IP，不能包含用户名或空格；请把 user@ 里的用户名填到「用户名」字段")
		return
	}

	// 不需要用户填凭据的两种认证方式，必须真的连一次才能给出结论。
	// 这里不能退化成 TCP 探测 —— 那样永远测不出"到底能不能登进去"，
	// 而用户要的恰恰是后者。
	if authType == sshclient.AuthNone || authType == sshclient.AuthLocalKey {
		elapsed, err := s.probeHost(sshclient.Target{
			Addr:     addr,
			Port:     port,
			Username: username,
			AuthType: authType,
		})
		if err != nil {
			writeJSON(w, http.StatusOK, hostTestResult{OK: false, Message: err.Error()})
			return
		}
		success := "连接成功（该主机允许无凭据登录）"
		if authType == sshclient.AuthLocalKey {
			success = "连接成功（使用面板所在机器的 SSH 密钥）"
		}
		writeJSON(w, http.StatusOK, hostTestResult{
			OK:        true,
			Message:   success,
			LatencyMs: latencyMs(elapsed),
		})
		return
	}

	// 没有凭据时只验证网络可达性。
	//
	// SSH 登录必然需要凭据，这是协议要求，不是面板的限制；但"这台机器通不通"
	// 不需要凭据就能回答。允许先填地址、端口、用户名并把机器登记进来，
	// 正是为了让用户能在拿到凭据之前先把地址填对、把网络打通。
	// 结论必须明确说出"没有登录"，否则会被当成"这台机器能连"。
	if secret == "" {
		status := probeHostPort(0, addr, port)
		if !status.Online {
			writeJSON(w, http.StatusOK, hostTestResult{
				OK:      false,
				Message: fmt.Sprintf("无法连接 %s:%d，请检查地址、端口与网络", addr, port),
			})
			return
		}
		writeJSON(w, http.StatusOK, hostTestResult{
			OK:        true,
			Message:   "网络可达（未配置凭据，只验证了端口连通，未登录）",
			LatencyMs: status.LatencyMs,
		})
		return
	}

	elapsed, err := s.probeHost(sshclient.Target{
		Addr:     addr,
		Port:     port,
		Username: username,
		AuthType: authType,
		Secret:   secret,
	})
	if err != nil {
		// 连不上是测试的结论，不是接口错误，因此照常返回 200。
		writeJSON(w, http.StatusOK, hostTestResult{OK: false, Message: err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, hostTestResult{
		OK:        true,
		Message:   "连接成功",
		LatencyMs: latencyMs(elapsed),
	})
}

// ---------- 辅助 ----------

// latencyMs 把一段耗时换算成带小数的毫秒。
//
// 不用 Duration.Milliseconds()：它会把不足 1 毫秒的部分整个丢掉，而本机回环
// 与同网段主机的建连恰好就落在那个区间里 —— 丢掉的正是唯一的有效数字，
// 界面上只能显示成 "0 ms"，看着像没测到数据。
func latencyMs(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

func (s *Server) decodeHostPayload(w http.ResponseWriter, r *http.Request) (hostPayload, bool) {
	var req hostPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return req, false
	}

	req.Name = strings.TrimSpace(req.Name)
	req.Addr = strings.TrimSpace(req.Addr)
	req.Username = strings.TrimSpace(req.Username)
	req.Group = strings.TrimSpace(req.Group)
	req.Remark = strings.TrimSpace(req.Remark)

	if req.Port <= 0 || req.Port > 65535 {
		req.Port = 22
	}
	if req.AuthType == "" {
		req.AuthType = sshclient.AuthPassword
	}

	switch {
	case req.Name == "":
		writeErr(w, http.StatusBadRequest, "主机名称不能为空")
	case req.Addr == "":
		writeErr(w, http.StatusBadRequest, "主机地址不能为空")
	case strings.ContainsAny(req.Addr, "@ "):
		// 从终端复制 `ssh -p 2222 user@host` 后整段粘进地址是很自然的动作，
		// 而现象是 DNS 报 no such host —— 用户会去查域名，而不是查自己填错了字段。
		writeErr(w, http.StatusBadRequest, "「地址」只能填主机名或 IP，不能包含用户名或空格；请把 user@ 里的用户名填到「用户名」字段")
	case req.Username == "":
		writeErr(w, http.StatusBadRequest, "登录用户名不能为空")
	case req.AuthType != sshclient.AuthPassword && req.AuthType != sshclient.AuthKey &&
		req.AuthType != sshclient.AuthNone && req.AuthType != sshclient.AuthLocalKey:
		writeErr(w, http.StatusBadRequest, "认证方式仅支持 password、key、none 或 localkey")
	default:
		return req, true
	}
	return req, false
}

// pathID 解析路径里的主机 ID。
//
// 0 是合法的：它代表宿主机（面板自己所在的那台机器），只读采集接口靠它本地直采。
// 需要 SSH 会话的接口由 leaseHost 挡下宿主机，不必在这里区分。
func (s *Server) pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 0 {
		writeErr(w, http.StatusBadRequest, "主机 ID 无效")
		return 0, false
	}
	return id, true
}

// hostSecret 读取并解密主机凭据。
func (s *Server) hostSecret(id int64) (string, error) {
	loginEnc, _, err := s.store.GetHostSecrets(id)
	if err != nil {
		return "", errors.New("读取凭据失败")
	}
	secret, err := s.box.Decrypt(loginEnc)
	if err != nil {
		return "", errors.New("解密凭据失败，主密钥可能已更换")
	}
	return secret, nil
}

// hostEscalationSecret 返回面板能拿来提权的口令、它的来源，以及有没有。
//
// 优先用主机上单独配置的「提权口令」（root 口令），dedicated 为真 —— 它只能
// 用来回答 su。没配时退回登录凭据，dedicated 为假 —— 它是执行者自己的口令，
// 正好是 sudo 要的那一份。两者都没有时 ok 为假，调用方据此判定"提不了权"。
//
// 退回登录凭据只在密码认证下成立：密钥认证时 secret_enc 存的是私钥正文，
// 把它当口令喂给 sudo 不只是白跑一次，还会把私钥写进目标机的认证日志。
func (s *Server) hostEscalationSecret(id int64) (secret string, dedicated bool, ok bool) {
	loginEnc, escalateEnc, err := s.store.GetHostSecrets(id)
	if err != nil {
		return "", false, false
	}
	if escalateEnc != "" {
		if pw, err := s.box.Decrypt(escalateEnc); err == nil && pw != "" {
			return pw, true, true
		}
	}

	host, err := s.store.GetHost(id)
	if err != nil || host.AuthType != sshclient.AuthPassword {
		return "", false, false
	}
	pw, err := s.box.Decrypt(loginEnc)
	if err != nil || pw == "" {
		return "", false, false
	}
	return pw, false, true
}

// encryptOptional 加密一份可选凭据；空串表示"不设置或保持原值"，原样返回空串。
func encryptOptional(box *secretbox.Box, plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	return box.Encrypt(plain)
}
