package main

// webui.go —— WebUI 管理端实现
//
// 设计要点：
//  1. 只依赖 rsc.io/qr：二维码在服务端生成 PNG，前端因此不需要任何第三方 JS 库，
//     整个页面 go:embed 进二进制。
//  2. WebUI 不做登录鉴权（与项目原有的控制台一致，绑定/解绑/token 都开放），
//     但所有 /admin/api/* 都要求带 X-WebUI: 1 自定义头，用于挡住外部网页发起的
//     跨站请求伪造（CSRF）。外部网页的表单发不出自定义头，用 fetch 又会触发
//     跨域预检，而我们不返回任何跨域允许头，浏览器不会真正发出请求。
//  3. 扫码登录在服务端后台轮询微信状态，前端只轮询本地接口，逻辑简单且不会
//     因为前端并发轮询而打爆微信的长轮询接口。

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"rsc.io/qr"
)

//go:embed static
var staticFS embed.FS

const (
	webUIHeader = "X-WebUI"

	// 界面文案放在 static/i18n/<code>.json，新增语言只要丢一个文件进去
	i18nDir         = "static/i18n"
	i18nDefaultLang = "zh"
)

// ---------------------------------------------------------------- 路由注册

func registerWebUI() {
	http.HandleFunc("GET /{$}", adminGate(handleIndex, false))

	http.HandleFunc("GET /admin/api/bots", adminGate(handleBotsList, true))
	http.HandleFunc("DELETE /admin/api/bots/{botID}", adminGate(handleBotDelete, true))
	http.HandleFunc("POST /admin/api/bots/{botID}/test", adminGate(handleBotTest, true))

	http.HandleFunc("POST /admin/api/login/start", adminGate(handleLoginStart, true))
	http.HandleFunc("GET /admin/api/login/qr", adminGate(handleLoginQR, true))
	http.HandleFunc("GET /admin/api/login/status", adminGate(handleLoginStatus, true))

	http.HandleFunc("GET /admin/api/logs", adminGate(handleLogs, true))
	http.HandleFunc("GET /admin/api/logs/stream", adminGate(handleLogsStream, true))

	http.HandleFunc("GET /admin/api/i18n", adminGate(handleI18nList, true))
	http.HandleFunc("GET /admin/api/i18n/{lang}", adminGate(handleI18nFile, true))
}

// ---------------------------------------------------------------- 界面语言
//
// 文案以 json 文件形式放在 static/i18n/ 下，一起 embed 进二进制。
// 加一种语言 = 加一个 <code>.json，前端会自动出现在语言切换里。

// handleI18nList 返回可用语言列表（含各语言的自称与默认语言），前端据此渲染切换按钮。
func handleI18nList(w http.ResponseWriter, r *http.Request) {
	entries, err := fs.ReadDir(staticFS, i18nDir)
	if err != nil {
		sendJSON(w, http.StatusInternalServerError, map[string]interface{}{"code": 500, "error": err.Error()})
		return
	}
	langs := make([]map[string]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		code := strings.TrimSuffix(e.Name(), ".json")
		name := code
		if data, err := staticFS.ReadFile(i18nDir + "/" + e.Name()); err == nil {
			var meta struct {
				Name string `json:"_name"`
			}
			if json.Unmarshal(data, &meta) == nil && meta.Name != "" {
				name = meta.Name
			}
		}
		langs = append(langs, map[string]string{"code": code, "name": name})
	}
	sort.Slice(langs, func(i, j int) bool { return langs[i]["code"] < langs[j]["code"] })

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"code": 200,
		"data": map[string]interface{}{"default": i18nDefaultLang, "langs": langs},
	})
}

// handleI18nFile 返回某个语言的文案。语言不存在时回落到默认语言，保证页面永远渲染得出来。
func handleI18nFile(w http.ResponseWriter, r *http.Request) {
	lang := r.PathValue("lang")
	for _, c := range lang {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-'
		if !ok {
			lang = ""
			break
		}
	}
	if lang == "" {
		lang = i18nDefaultLang
	}

	data, err := staticFS.ReadFile(i18nDir + "/" + lang + ".json")
	if err != nil {
		data, err = staticFS.ReadFile(i18nDir + "/" + i18nDefaultLang + ".json")
	}
	if err != nil {
		sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"code": 500, "error": "no i18n files are embedded",
		})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "index.html not embedded", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(data)
}

// ---------------------------------------------------------------- 访问控制
//
// 后台（网页 + /admin/api/*）统一走四道闸，顺序固定：
//
//	1. 来源闸  只放本机与内网。判据只用 TCP 对端地址，绝不看 X-Forwarded-For
//	           （那个头客户端可以随便填）；对端解析不出来一律拒绝（fail-closed）。
//	2. 锁  定  同一来源连续口令错误后指数级暂停，挡字典与暴力尝试。
//	3. 口  令  HTTP Basic，浏览器原生弹窗，因此前端不需要登录页与会话。
//	4. CSRF   要求 X-WebUI 自定义头。跨站表单发不出自定义头，跨站 fetch 会
//	           触发预检而我们不返回任何 CORS 头，两条路都过不来。
//
// 对外 API /bots/* 完全不在这些闸门后面：它有自己的 api_token 认证，而且调用方
// 可能在内网之外（VPS 上的脚本、云函数），套上来源闸会直接把它们打死。

const (
	webUIRealm        = "WeClawBot-API WebUI"
	webUIFailLimit    = 5              // 连续失败多少次开始锁
	webUILockBaseSec  = 300            // 首次锁 5 分钟
	webUILockMaxSec   = 3600           // 封顶 60 分钟
	webUIFailForget   = 24 * time.Hour // 多久没有新失败就忘掉这条记录
	webUILimitEntries = 1000           // 记账表内存上限
	webUICredPath     = "./config/webui.json"
)

// localNets 默认放行的来源：本机与私有网段（172.16/12 覆盖了 Docker 默认网桥 172.17/16）。
var localNets = []string{
	"127.0.0.0/8", "::1/128",
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	"169.254.0.0/16", "fe80::/10", "fc00::/7",
}

type webUICredFile struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type webUIAuthConfig struct {
	user        string
	password    string
	source      string
	disabled    bool
	allowPublic bool
	trusted     []*net.IPNet
}

var webAuth = &webUIAuthConfig{user: "admin"}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// readWebUICredFile 读取 config/webui.json；文件不存在或解析失败返回 false。
func readWebUICredFile() (webUICredFile, bool) {
	cred := webUICredFile{}
	data, err := os.ReadFile(webUICredPath)
	if err != nil {
		return cred, false
	}
	if err := json.Unmarshal(data, &cred); err != nil {
		log.Printf("Warning: cannot parse %s: %v", webUICredPath, err)
		return webUICredFile{}, false
	}
	return cred, true
}

// initWebUIAuth 决定后台口令的来源，优先级：环境变量 > config/webui.json > 随机生成并落盘。
func initWebUIAuth(noAuth bool, trustCIDR string, allowPublic bool) {
	webAuth.disabled = noAuth
	webAuth.allowPublic = allowPublic

	for _, part := range strings.Split(trustCIDR, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ipnet, err := net.ParseCIDR(part); err == nil {
			webAuth.trusted = append(webAuth.trusted, ipnet)
		} else {
			log.Printf("Warning: ignoring invalid -trust-cidr entry %q: %v", part, err)
		}
	}

	cred, _ := readWebUICredFile()
	webAuth.user = envOr("WEBUI_USER", envOr(cred.Username, "admin"))

	if pw := os.Getenv("WEBUI_PASSWORD"); pw != "" {
		webAuth.password = pw
		webAuth.source = "环境变量 WEBUI_PASSWORD"
		return
	}
	if cred.Password != "" {
		webAuth.password = cred.Password
		webAuth.source = webUICredPath
		return
	}
	if noAuth {
		// 已经明确关掉口令，就不要再生成一个没人用的密钥文件
		webAuth.source = "已用 -no-auth 关闭"
		return
	}

	// 两处都没有：生成一个 32 字符随机口令并落盘，之后重建容器也不会变
	webAuth.password = generateToken(24)
	webAuth.source = "随机生成，已写入 " + webUICredPath
	cred = webUICredFile{Username: webAuth.user, Password: webAuth.password}
	if data, err := json.MarshalIndent(cred, "", "  "); err == nil {
		if err := os.WriteFile(webUICredPath, data, 0600); err != nil {
			log.Printf("Warning: cannot persist WebUI credentials to %s: %v", webUICredPath, err)
		}
	}
	fmt.Println("============================================================")
	fmt.Println(" 已生成 WebUI 口令（只打印这一次，同时写入 " + webUICredPath + "）：")
	fmt.Println("   " + webAuth.password)
	fmt.Println(" 忘了它：删除该文件后重启会重新生成，或用环境变量 WEBUI_PASSWORD 指定。")
	fmt.Println("============================================================")
}

// printAccessSummary 在启动日志里把当前生效的访问策略说清楚。
func printAccessSummary() {
	fmt.Println("WebUI access control:")
	if webAuth.disabled {
		fmt.Println("  [!] -no-auth：后台不校验口令，任何能连上端口的人都有全部权限")
	} else {
		fmt.Printf("  username : %s\n", webAuth.user)
		fmt.Printf("  password : %s\n", webAuth.source)
	}
	if webAuth.allowPublic {
		fmt.Println("  [!] -allow-public：允许任何来源访问后台，包括公网")
	} else {
		fmt.Println("  source   : 仅本机与内网（可用 -trust-cidr 追加网段，-allow-public 完全放开）")
	}
	for _, n := range webAuth.trusted {
		fmt.Printf("  extra    : 额外信任 %s\n", n.String())
	}
	fmt.Println("  note     : /bots/* 不受上述限制，仍按 api_token 认证")
}

// peerIP 只认 TCP 对端地址。X-Forwarded-For 是客户端可伪造的头，这里刻意不看。
func peerIP(r *http.Request) (net.IP, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	if ip == nil {
		return nil, false
	}
	if v4 := ip.To4(); v4 != nil {
		return v4, true
	}
	return ip, true
}

// sourceAllowed 判定来源是否可以访问后台。对端为空（解析失败）时返回 false。
func sourceAllowed(ip net.IP) bool {
	if webAuth.allowPublic {
		return true
	}
	if ip == nil {
		return false
	}
	for _, cidr := range localNets {
		_, ipnet, err := net.ParseCIDR(cidr)
		if err == nil && ipnet.Contains(ip) {
			return true
		}
	}
	for _, ipnet := range webAuth.trusted {
		if ipnet.Contains(ip) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- 失败限速

type failEntry struct {
	failures int
	lockedTo time.Time
	lastFail time.Time
}

type failureGuard struct {
	mu      sync.Mutex
	entries map[string]*failEntry
}

func newFailureGuard() *failureGuard {
	return &failureGuard{entries: make(map[string]*failEntry)}
}

var loginLimiter = newFailureGuard()

// check 只查不计数：锁没到期返回 false 和剩余秒数。
func (g *failureGuard) check(ip net.IP) (bool, int) {
	if g == nil || ip == nil {
		return true, 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.entries[ip.String()]
	if e == nil || !time.Now().Before(e.lockedTo) {
		return true, 0
	}
	return false, int(time.Until(e.lockedTo).Seconds()) + 1
}

// fail 记一次失败，返回累计失败次数与本次锁定秒数（未触发锁定为 0）。
func (g *failureGuard) fail(ip net.IP) (int, int) {
	if g == nil || ip == nil {
		return 0, 0
	}
	now := time.Now()
	key := ip.String()

	g.mu.Lock()
	defer g.mu.Unlock()

	g.forgetOldLocked(now)

	e := g.entries[key]
	if e == nil {
		e = &failEntry{}
		g.entries[key] = e
	}
	e.failures++
	e.lastFail = now

	if e.failures < webUIFailLimit {
		return e.failures, 0
	}
	// 第 5 次失败锁 5 分钟，之后每次失败翻倍，60 分钟封顶
	secs := webUILockBaseSec
	for i := 0; i < e.failures-webUIFailLimit; i++ {
		secs *= 2
		if secs >= webUILockMaxSec {
			secs = webUILockMaxSec
			break
		}
	}
	e.lockedTo = now.Add(time.Duration(secs) * time.Second)
	g.enforceCapLocked()
	return e.failures, secs
}

func (g *failureGuard) succeed(ip net.IP) {
	if g == nil || ip == nil {
		return
	}
	g.mu.Lock()
	delete(g.entries, ip.String())
	g.mu.Unlock()
}

// forgetOldLocked 丢掉太久没有新失败的记录，让失败次数重新从 0 开始算。
func (g *failureGuard) forgetOldLocked(now time.Time) {
	for k, e := range g.entries {
		if now.Sub(e.lastFail) > webUIFailForget {
			delete(g.entries, k)
		}
	}
}

// enforceCapLocked 是内存上限，不是"少锁谁"：超限就淘汰最久没失败的那批，留 75%。
func (g *failureGuard) enforceCapLocked() {
	if len(g.entries) <= webUILimitEntries {
		return
	}
	type kv struct {
		key  string
		last time.Time
	}
	all := make([]kv, 0, len(g.entries))
	for k, e := range g.entries {
		all = append(all, kv{k, e.lastFail})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].last.Before(all[j].last) })
	target := len(all) * 3 / 4
	for i := 0; i < len(all)-target; i++ {
		delete(g.entries, all[i].key)
	}
}

// ---------------------------------------------------------------- 闸门

// adminGate 是所有后台入口的统一闸门，requireCSRF 为 true 时额外要求 X-WebUI 头
// （网页本身由浏览器直接导航打开，带不了自定义头，所以那一处传 false）。
func adminGate(next http.HandlerFunc, requireCSRF bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")

		ip, parsed := peerIP(r)
		if !parsed || !sourceAllowed(ip) {
			via := "peer=" + r.RemoteAddr + " (X-Forwarded-For is deliberately ignored)"
			if !parsed {
				via = "unparsable peer address: " + r.RemoteAddr
			}
			sendJSON(w, http.StatusForbidden, map[string]interface{}{
				"code":       403,
				"error_code": "admin_local_only",
				"error":      "admin_local_only: WebUI 只允许从本机或内网访问",
				"via":        via,
			})
			return
		}

		if allowed, retry := loginLimiter.check(ip); !allowed {
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			sendJSON(w, http.StatusTooManyRequests, map[string]interface{}{
				"code":        429,
				"error_code":  "too_many_failures",
				"retry_after": retry,
				"error":       fmt.Sprintf("口令错误次数过多，已暂停 %d 秒", retry),
			})
			return
		}

		if !webAuth.disabled {
			user, pass, hasAuth := r.BasicAuth()
			userOK := subtle.ConstantTimeCompare([]byte(user), []byte(webAuth.user)) == 1
			passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(webAuth.password)) == 1
			if !hasAuth || !userOK || !passOK {
				_, locked := loginLimiter.fail(ip)
				w.Header().Set("WWW-Authenticate", fmt.Sprintf("Basic realm=%q, charset=\"UTF-8\"", webUIRealm))
				resp := map[string]interface{}{
					"code":       401,
					"error_code": "auth_required",
					"error":      "需要登录：请输入 WebUI 用户名与口令",
				}
				if locked > 0 {
					w.Header().Set("Retry-After", strconv.Itoa(locked))
					resp["error_code"] = "too_many_failures"
					resp["retry_after"] = locked
					resp["error"] = fmt.Sprintf("口令错误次数过多，已暂停 %d 秒", locked)
				}
				sendJSON(w, http.StatusUnauthorized, resp)
				return
			}
			loginLimiter.succeed(ip)
		}

		if requireCSRF && r.Header.Get(webUIHeader) != "1" {
			sendJSON(w, http.StatusForbidden, map[string]interface{}{
				"code":       403,
				"error_code": "csrf_header_missing",
				"error":      "Forbidden: 缺少 " + webUIHeader + " 请求头",
			})
			return
		}

		next(w, r)
	}
}

// ---------------------------------------------------------------- 账号列表

func botViewOf(botID string, u *UserConfig) map[string]interface{} {
	runtimeLock.Lock()
	rt := runtimes[botID]
	monitoring := rt != nil && rt.running
	var recv int64
	if rt != nil {
		recv = atomic.LoadInt64(&rt.recv)
	}
	runtimeLock.Unlock()

	return map[string]interface{}{
		"bot_id":        botID,
		"api_token":     u.APIToken,
		"ilink_user_id": u.IlinkUserID,
		"has_bot_token": u.BotToken != "",
		"activated":     u.IlinkUserID != "" && u.ContextToken != "",
		"monitoring":    monitoring,
		"recv_count":    recv,
	}
}

func handleBotsList(w http.ResponseWriter, r *http.Request) {
	configLock.Lock()
	list := make([]map[string]interface{}, 0, len(cfg.Bots))
	for botID, u := range cfg.Bots {
		list = append(list, botViewOf(botID, u))
	}
	configLock.Unlock()

	sort.Slice(list, func(i, j int) bool {
		return fmt.Sprint(list[i]["bot_id"]) < fmt.Sprint(list[j]["bot_id"])
	})

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"code": 200,
		"data": map[string]interface{}{
			"bots":  list,
			"count": len(list),
		},
	})
}

func handleBotDelete(w http.ResponseWriter, r *http.Request) {
	botID := r.PathValue("botID")
	if !removeBot(botID) {
		sendJSON(w, http.StatusNotFound, map[string]interface{}{
			"code": 404, "error_code": "bot_not_found", "error": "Bot not found",
		})
		return
	}
	// Action 用稳定 code，界面按当前语言渲染；不再往 Text 里塞中文说明
	apiLogs.add(&logEntry{
		Source: "webui", Method: "DELETE", Path: "/admin/api/bots/" + botID,
		BotID: botID, Action: "unbind", Status: 200, OK: true, IP: clientIP(r),
	})
	sendJSON(w, http.StatusOK, map[string]interface{}{"code": 200, "message": "OK"})
}

func handleBotTest(w http.ResponseWriter, r *http.Request) {
	botID := r.PathValue("botID")

	text := ""
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if len(raw) > 0 {
		var body map[string]interface{}
		if json.Unmarshal(raw, &body) == nil {
			if v, ok := body["text"]; ok && v != nil {
				text = fmt.Sprint(v)
			}
		} else if p, err := url.ParseQuery(string(raw)); err == nil {
			text = p.Get("text")
		}
	}
	if strings.TrimSpace(text) == "" {
		text = fmt.Sprintf("✅ WeClawBot 测试消息\n%s", time.Now().Format("2006-01-02 15:04:05"))
	}

	configLock.Lock()
	user, exists := cfg.Bots[botID]
	configLock.Unlock()
	if !exists {
		sendJSON(w, http.StatusNotFound, map[string]interface{}{
			"code": 404, "error_code": "bot_not_found", "error": "Bot not found",
		})
		return
	}

	entry := &logEntry{
		Source: "webui", Method: "POST", Path: "/admin/api/bots/" + botID + "/test",
		BotID: botID, Action: "test_push", Text: truncate(text, 200), Status: 200, OK: true,
		IP: clientIP(r),
	}

	if user.IlinkUserID == "" || user.ContextToken == "" {
		msg := "该账号尚未激活：请先在微信里给 ClawBot 发一条消息，服务器收到后才有回复上下文"
		entry.OK, entry.Status, entry.ErrorCode, entry.Error = false, 400, "bot_not_activated", msg
		apiLogs.add(entry)
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{
			"code": 400, "error_code": "bot_not_activated", "error": msg,
		})
		return
	}

	start := time.Now()
	err := sendMessage(user, user.IlinkUserID, text, user.ContextToken)
	entry.Duration = time.Since(start).Milliseconds()
	if err != nil {
		entry.OK, entry.Status, entry.ErrorCode, entry.Error = false, 500, "send_failed", err.Error()
		apiLogs.add(entry)
		sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"code": 500, "error_code": "send_failed", "error": err.Error(), "duration_ms": entry.Duration,
		})
		return
	}
	apiLogs.add(entry)
	sendJSON(w, http.StatusOK, map[string]interface{}{
		"code": 200, "message": "OK", "duration_ms": entry.Duration,
	})
}

// ---------------------------------------------------------------- 扫码登录

type loginCreds struct {
	BotToken    string
	BotID       string
	IlinkUserID string
}

type loginSession struct {
	ID        string
	QRToken   string // 微信侧二维码 token，用于轮询状态
	QRText    string // 待编码内容，交给 rsc.io/qr 生成图片
	Version   int    // 二维码刷新次数，前端靠它判断要不要换图
	Status    string // wait / scaned / confirmed / failed
	BotID     string
	Err       string // 原始错误文本（兜底）
	ErrCode   string // 已知原因的稳定 code，前端按当前语言渲染
	Refreshed int
	IP        string // 发起扫码的浏览器 IP，用于日志
	Created   time.Time
	cancel    context.CancelFunc
}

var (
	loginMu       sync.Mutex
	loginSessions = make(map[string]*loginSession)
)

func (s *loginSession) snapshot() map[string]interface{} {
	loginMu.Lock()
	defer loginMu.Unlock()
	return map[string]interface{}{
		"id":         s.ID,
		"status":     s.Status,
		"bot_id":     s.BotID,
		"error":      s.Err,
		"error_code": s.ErrCode,
		"version":    s.Version,
		"refreshed":  s.Refreshed,
	}
}

func (s *loginSession) setStatus(status, botID, errCode, errDetail string) {
	loginMu.Lock()
	defer loginMu.Unlock()
	s.Status = status
	if botID != "" {
		s.BotID = botID
	}
	if errCode != "" {
		s.ErrCode = errCode
	}
	if errDetail != "" {
		s.Err = errDetail
	}
}

func (s *loginSession) qrText() string {
	loginMu.Lock()
	defer loginMu.Unlock()
	return s.QRText
}

func findLoginSession(id string) *loginSession {
	if id == "" {
		return nil
	}
	loginMu.Lock()
	defer loginMu.Unlock()
	return loginSessions[id]
}

// resetLoginSessions 取消旧的登录会话，避免重复扫码时留下仍在轮询的协程。
// 刚确认成功的会话保留一分钟，让前端还能读到结果。
func resetLoginSessions() {
	loginMu.Lock()
	defer loginMu.Unlock()
	for id, s := range loginSessions {
		if s.Status == "confirmed" && time.Since(s.Created) < time.Minute {
			continue
		}
		if s.cancel != nil {
			s.cancel()
		}
		delete(loginSessions, id)
	}
}

func handleLoginStart(w http.ResponseWriter, r *http.Request) {
	resetLoginSessions()

	ctx, cancel := context.WithCancel(context.Background())
	sess := &loginSession{
		ID:      generateToken(9),
		Status:  "wait",
		Created: time.Now(),
		IP:      clientIP(r),
		cancel:  cancel,
	}
	if err := sess.fetchQRCode(ctx); err != nil {
		cancel()
		sendJSON(w, http.StatusBadGateway, map[string]interface{}{
			"code": 502, "error_code": "qrcode_fetch_failed",
			"error": "获取二维码失败: " + err.Error(), "detail": err.Error(),
		})
		return
	}

	loginMu.Lock()
	loginSessions[sess.ID] = sess
	loginMu.Unlock()

	go sess.poll(ctx)
	sendJSON(w, http.StatusOK, map[string]interface{}{"code": 200, "data": sess.snapshot()})
}

func handleLoginStatus(w http.ResponseWriter, r *http.Request) {
	sess := findLoginSession(r.URL.Query().Get("id"))
	if sess == nil {
		sendJSON(w, http.StatusNotFound, map[string]interface{}{
			"code": 404, "error_code": "login_session_missing",
			"error": "登录会话不存在或已过期，请重新获取二维码",
		})
		return
	}
	sendJSON(w, http.StatusOK, map[string]interface{}{"code": 200, "data": sess.snapshot()})
}

func handleLoginQR(w http.ResponseWriter, r *http.Request) {
	sess := findLoginSession(r.URL.Query().Get("id"))
	if sess == nil {
		http.Error(w, "login session not found", http.StatusNotFound)
		return
	}
	text := sess.qrText()
	if text == "" {
		http.Error(w, "qrcode not ready", http.StatusConflict)
		return
	}
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		http.Error(w, "encode qrcode failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(code.PNG())
}

func (s *loginSession) fetchQRCode(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", DefaultBaseURL+"/ilink/bot/get_bot_qrcode?bot_type=3", nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var qrRes struct {
		QRcode           string `json:"qrcode"`
		QRcodeImgContent string `json:"qrcode_img_content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&qrRes); err != nil {
		return err
	}
	if qrRes.QRcode == "" || qrRes.QRcodeImgContent == "" {
		return fmt.Errorf("empty qrcode payload from the WeChat API")
	}

	loginMu.Lock()
	s.QRToken = qrRes.QRcode
	s.QRText = qrRes.QRcodeImgContent
	s.Version++
	s.Status = "wait"
	loginMu.Unlock()
	return nil
}

func (s *loginSession) queryStatus(ctx context.Context, client *http.Client) (string, loginCreds, error) {
	var creds loginCreds
	loginMu.Lock()
	token := s.QRToken
	loginMu.Unlock()
	if token == "" {
		return "", creds, fmt.Errorf("no qrcode token")
	}

	req, err := http.NewRequestWithContext(ctx, "GET",
		DefaultBaseURL+"/ilink/bot/get_qrcode_status?qrcode="+url.QueryEscape(token), nil)
	if err != nil {
		return "", creds, err
	}
	commonHeaders(req, false, "")
	req.Header.Set("iLink-App-ClientVersion", "1")

	resp, err := client.Do(req)
	if err != nil {
		return "", creds, err
	}
	defer resp.Body.Close()

	var sRes struct {
		Status      string `json:"status"`
		BotToken    string `json:"bot_token"`
		IlinkBotID  string `json:"ilink_bot_id"`
		IlinkUserID string `json:"ilink_user_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sRes); err != nil {
		return "", creds, err
	}
	creds = loginCreds{BotToken: sRes.BotToken, BotID: sRes.IlinkBotID, IlinkUserID: sRes.IlinkUserID}
	return sRes.Status, creds, nil
}

// poll 在后台轮询微信侧的扫码状态，前端只轮询本地的 /admin/api/login/status。
func (s *loginSession) poll(ctx context.Context) {
	client := &http.Client{Timeout: 40 * time.Second}

	for {
		if ctx.Err() != nil {
			return
		}

		status, creds, err := s.queryStatus(ctx, client)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(2 * time.Second) // 网络抖动，等一会儿重试，与原控制台逻辑一致
			continue
		}

		switch status {
		case "wait":
			// 保持等待
		case "scaned":
			s.setStatus("scaned", "", "", "")
		case "confirmed":
			if creds.BotToken == "" || creds.BotID == "" {
				s.setStatus("failed", "", "login_no_credentials", "")
				return
			}
			registerBot(creds.BotToken, creds.BotID, creds.IlinkUserID)
			s.setStatus("confirmed", creds.BotID, "", "")
			apiLogs.add(&logEntry{
				Source: "webui", Method: "POST", Path: "/admin/api/login/start",
				BotID: creds.BotID, Action: "bind", Status: 200, OK: true,
				IP: s.IP,
			})
			return
		case "expired":
			loginMu.Lock()
			s.Refreshed++
			refreshed := s.Refreshed
			loginMu.Unlock()
			if refreshed > 3 {
				s.setStatus("failed", "", "qrcode_expired", "")
				return
			}
			if err := s.fetchQRCode(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				s.setStatus("failed", "", "qrcode_refresh_failed", err.Error())
				return
			}
		default:
			// 未知状态，继续轮询
		}

		time.Sleep(time.Second)
	}
}

// ---------------------------------------------------------------- 调用日志

type logEntry struct {
	Seq     uint64 `json:"seq"`
	Time    string `json:"time"`
	Source  string `json:"source"` // api = 第三方调用；webui = 页面上的操作
	Method  string `json:"method"`
	Path    string `json:"path"`
	BotID   string `json:"bot_id"`
	Action  string `json:"action"`
	AuthVia string `json:"auth_via"`
	Text    string `json:"text"`
	IP      string `json:"ip"`
	Status  int    `json:"status"`
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	// ErrorCode 是已知原因的稳定标识（前端按当前语言渲染），Error 留作原始文本/兜底
	ErrorCode string `json:"error_code,omitempty"`
	Duration  int64  `json:"duration_ms"`
}

type logStore struct {
	mu      sync.Mutex
	fileMu  sync.Mutex
	entries []*logEntry
	max     int
	seq     uint64
	subs    map[chan *logEntry]struct{}
	file    *os.File
	path    string
}

var apiLogs = newLogStore(1000)

func newLogStore(max int) *logStore {
	if max <= 0 {
		max = 1000
	}
	return &logStore{max: max, subs: make(map[chan *logEntry]struct{})}
}

// enableFile 打开可选的日志落盘（JSONL），失败时返回错误由调用方决定是否致命。
func (s *logStore) enableFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.file = f
	s.path = path
	s.mu.Unlock()
	return nil
}

// filePath 返回落盘路径，未开启落盘时为空字符串。
func (s *logStore) filePath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}

func (s *logStore) add(e *logEntry) {
	if e.Time == "" {
		e.Time = time.Now().Format("2006-01-02 15:04:05.000")
	}

	s.mu.Lock()
	s.seq++
	e.Seq = s.seq
	s.entries = append(s.entries, e)
	if len(s.entries) > s.max {
		trimmed := make([]*logEntry, s.max)
		copy(trimmed, s.entries[len(s.entries)-s.max:])
		s.entries = trimmed
	}
	f := s.file
	subs := make([]chan *logEntry, 0, len(s.subs))
	for ch := range s.subs {
		subs = append(subs, ch)
	}
	s.mu.Unlock()

	if f != nil {
		if b, err := json.Marshal(e); err == nil {
			s.fileMu.Lock()
			f.Write(append(b, '\n'))
			s.fileMu.Unlock()
		}
	}

	for _, ch := range subs {
		select {
		case ch <- e:
		default: // 订阅者读得慢就丢，绝不阻塞请求
		}
	}
}

// list 返回最新的日志，按时间倒序（新的在前）；since 用于只取增量。
func (s *logStore) list(limit int, since uint64) []*logEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*logEntry, 0, 64)
	for i := len(s.entries) - 1; i >= 0 && len(out) < limit; i-- {
		e := s.entries[i]
		if e.Seq <= since {
			break
		}
		out = append(out, e)
	}
	return out
}

func (s *logStore) latest() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

func (s *logStore) subscribe() chan *logEntry {
	ch := make(chan *logEntry, 128)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch
}

func (s *logStore) unsubscribe(ch chan *logEntry) {
	s.mu.Lock()
	delete(s.subs, ch)
	s.mu.Unlock()
}

func handleLogs(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 5000 {
			limit = n
		}
	}
	var since uint64
	if v := r.URL.Query().Get("since"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			since = n
		}
	}
	sendJSON(w, http.StatusOK, map[string]interface{}{
		"code": 200,
		"data": map[string]interface{}{
			"entries": apiLogs.list(limit, since),
			"latest":  apiLogs.latest(),
			"size":    apiLogs.max,
			"file":    apiLogs.filePath(),
		},
	})
}

// handleLogsStream 用 SSE 推送日志。浏览器原生的 EventSource 不支持自定义请求头，
// 所以前端用 fetch + ReadableStream 手动消费，这样 CSRF 守卫对读接口也能一视同仁。
func handleLogsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Nginx 反代时禁用缓冲
	w.WriteHeader(http.StatusOK)

	ch := apiLogs.subscribe()
	defer apiLogs.unsubscribe(ch)

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			b, err := json.Marshal(e)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// ---------------------------------------------------------------- 请求日志中间件

type loggingWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (l *loggingWriter) WriteHeader(code int) {
	l.status = code
	l.ResponseWriter.WriteHeader(code)
}

func (l *loggingWriter) Write(b []byte) (int, error) {
	if l.status == 0 {
		l.status = http.StatusOK
	}
	if room := 4096 - l.body.Len(); room > 0 {
		if len(b) < room {
			room = len(b)
		}
		l.body.Write(b[:room])
	}
	return l.ResponseWriter.Write(b)
}

// apiLogMiddleware 记录第三方对 /bots/* 的调用。请求体读出来后会原样塞回去，
// 否则后面的处理器会解析到空 body。
func apiLogMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		var bodyBytes []byte
		if r.Body != nil {
			bodyBytes, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}

		lw := &loggingWriter{ResponseWriter: w}
		next(lw, r)

		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/bots/"), "/")
		botID, action := "", ""
		if len(parts) > 0 {
			botID = parts[0]
		}
		if len(parts) > 1 {
			action = parts[1]
		}

		params := requestParams(r, bodyBytes)
		entry := &logEntry{
			Source:   "api",
			Method:   r.Method,
			Path:     r.URL.Path,
			BotID:    botID,
			Action:   action,
			AuthVia:  authVia(r, params),
			Text:     truncate(params.Get("text"), 200),
			IP:       clientIP(r),
			Status:   lw.status,
			Duration: time.Since(start).Milliseconds(),
			OK:       lw.status == http.StatusOK,
		}
		if !entry.OK {
			var e struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(bytes.TrimSpace(lw.body.Bytes()), &e) == nil {
				entry.Error = e.Error
			}
		}
		apiLogs.add(entry)
	}
}

// requestParams 尽力还原调用参数：查询串 + JSON / 表单请求体。API Token 不记录。
func requestParams(r *http.Request, body []byte) url.Values {
	vals := url.Values{}
	for k, vs := range r.URL.Query() {
		vals[k] = vs
	}
	if len(body) == 0 {
		return vals
	}
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.Contains(ct, "application/json"):
		var m map[string]interface{}
		if json.Unmarshal(body, &m) == nil {
			for k, v := range m {
				if v == nil {
					continue
				}
				vals.Set(k, fmt.Sprint(v))
			}
		}
	case strings.Contains(ct, "application/x-www-form-urlencoded"):
		if p, err := url.ParseQuery(string(body)); err == nil {
			for k, vs := range p {
				vals[k] = vs
			}
		}
	}
	return vals
}

func authVia(r *http.Request, params url.Values) string {
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return "header"
	}
	if params.Get("token") != "" {
		if r.URL.Query().Get("token") != "" {
			return "query"
		}
		return "body"
	}
	return "none"
}

// clientIP 兼容 NAS 上常见的反向代理。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xr := r.Header.Get("X-Real-IP"); xr != "" {
		return strings.TrimSpace(xr)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", ""))
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}
