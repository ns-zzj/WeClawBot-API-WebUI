package main

import (
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// withAuth 临时改全局访问策略，返回还原函数。
func withAuth(public bool, trusted ...string) func() {
	savedPublic, savedTrusted, savedDisabled := webAuth.allowPublic, webAuth.trusted, webAuth.disabled
	webAuth.allowPublic = public
	webAuth.trusted = nil
	for _, c := range trusted {
		_, ipnet, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		webAuth.trusted = append(webAuth.trusted, ipnet)
	}
	return func() {
		webAuth.allowPublic = savedPublic
		webAuth.trusted = savedTrusted
		webAuth.disabled = savedDisabled
	}
}

func TestSourceAllowedDefaults(t *testing.T) {
	defer withAuth(false)()
	cases := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"192.168.8.8", true},
		{"10.1.2.3", true},
		{"172.17.0.1", true},  // Docker 默认网桥网关
		{"172.15.0.1", false}, // 172.16/12 之外
		{"172.32.0.1", false},
		{"169.254.1.1", true},
		{"fd00::1", true},
		{"fe80::1", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"100.64.1.2", false}, // Tailscale/CMGNAT 网段默认不放行，要靠 -trust-cidr
	}
	for _, c := range cases {
		if got := sourceAllowed(net.ParseIP(c.ip)); got != c.want {
			t.Errorf("sourceAllowed(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

func TestSourceAllowedFailClosed(t *testing.T) {
	defer withAuth(false)()
	if sourceAllowed(nil) {
		t.Error("对端地址解析不出来时必须拒绝（fail-closed）")
	}
}

func TestSourceAllowedTrustedCIDR(t *testing.T) {
	defer withAuth(false, "100.64.0.0/10")()
	if !sourceAllowed(net.ParseIP("100.64.1.2")) {
		t.Error("-trust-cidr 里的网段应当放行")
	}
	if sourceAllowed(net.ParseIP("100.128.1.2")) {
		t.Error("-trust-cidr 之外的地址不应放行")
	}
}

func TestSourceAllowedPublic(t *testing.T) {
	defer withAuth(true)()
	if !sourceAllowed(net.ParseIP("8.8.8.8")) {
		t.Error("-allow-public 时任何来源都应放行")
	}
}

func TestPeerIP(t *testing.T) {
	cases := []struct {
		remote string
		want   string
		ok     bool
	}{
		{"192.168.1.5:54321", "192.168.1.5", true},
		{"[fd00::1]:443", "fd00::1", true},
		{"192.168.1.5", "192.168.1.5", true}, // 没有端口
		{"not-an-ip", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		ip, ok := peerIP(&http.Request{RemoteAddr: c.remote})
		if ok != c.ok {
			t.Errorf("peerIP(%q) ok = %v, want %v", c.remote, ok, c.ok)
			continue
		}
		if ok && ip.String() != c.want {
			t.Errorf("peerIP(%q) = %s, want %s", c.remote, ip, c.want)
		}
	}
}

func TestFailureGuardEscalation(t *testing.T) {
	g := newFailureGuard()
	ip := net.ParseIP("192.168.1.10")
	// 前 4 次不锁；第 5 次起 5 → 10 → 20 → 40 → 60 分钟封顶
	want := []int{0, 0, 0, 0, 300, 600, 1200, 2400, 3600, 3600, 3600}
	for i, w := range want {
		fails, locked := g.fail(ip)
		if fails != i+1 {
			t.Fatalf("第 %d 次失败累计数 = %d", i+1, fails)
		}
		if locked != w {
			t.Errorf("第 %d 次失败锁定 %d 秒，期望 %d 秒", i+1, locked, w)
		}
	}
}

func TestFailureGuardCheckDoesNotCount(t *testing.T) {
	g := newFailureGuard()
	ip := net.ParseIP("10.0.0.5")
	for i := 0; i < 3; i++ {
		g.fail(ip)
	}
	if ok, _ := g.check(ip); !ok {
		t.Error("未到阈值不该锁定")
	}
	for i := 0; i < 10; i++ {
		g.check(ip) // check 只查不计数
	}
	if fails, _ := g.fail(ip); fails != 4 {
		t.Errorf("check 不应累加失败次数，得到 %d，期望 4", fails)
	}
}

func TestFailureGuardLockedThenSucceedResets(t *testing.T) {
	g := newFailureGuard()
	ip := net.ParseIP("10.0.0.6")
	for i := 0; i < webUIFailLimit; i++ {
		g.fail(ip)
	}
	ok, retry := g.check(ip)
	if ok || retry <= 0 {
		t.Fatalf("应当处于锁定状态：ok=%v retry=%d", ok, retry)
	}
	if retry > webUILockBaseSec {
		t.Errorf("首次锁定时长应不超过 %d 秒，得到 %d", webUILockBaseSec, retry)
	}
	g.succeed(ip)
	if ok, _ := g.check(ip); !ok {
		t.Error("成功一次后应当解除锁定")
	}
	if fails, locked := g.fail(ip); fails != 1 || locked != 0 {
		t.Errorf("成功应当清空计数：failures=%d locked=%d", fails, locked)
	}
}

func TestFailureGuardForgetsStaleEntries(t *testing.T) {
	g := newFailureGuard()
	ip := net.ParseIP("10.0.0.7")
	g.mu.Lock()
	g.entries[ip.String()] = &failEntry{failures: 4, lastFail: time.Now().Add(-25 * time.Hour)}
	g.mu.Unlock()

	fails, locked := g.fail(ip)
	if fails != 1 || locked != 0 {
		t.Errorf("超过 24 小时没有新失败应重新从 0 计：failures=%d locked=%d", fails, locked)
	}
}

func TestFailureGuardEntryCap(t *testing.T) {
	g := newFailureGuard()
	const extra = 50
	total := webUILimitEntries + extra
	for i := 0; i < total; i++ {
		g.entries[net.IPv4(10, byte(i>>8), byte(i&0xff), 1).String()] = &failEntry{failures: 1, lastFail: time.Now()}
	}
	g.mu.Lock()
	g.enforceCapLocked()
	size := len(g.entries)
	g.mu.Unlock()

	want := total * 3 / 4
	if size != want {
		t.Errorf("超限后应裁到 75%%（%d 条），实际 %d 条", want, size)
	}
}

// TestAdminGate 直接打闸门本身：公网来源必须被拒（这条没法从外部发请求测出来）。
func TestAdminGate(t *testing.T) {
	savedAuth, savedLimiter := *webAuth, loginLimiter
	defer func() { *webAuth = savedAuth; loginLimiter = savedLimiter }()

	webAuth.user = "admin"
	webAuth.password = "s3cret"
	webAuth.disabled = false
	webAuth.allowPublic = false
	webAuth.trusted = nil
	loginLimiter = newFailureGuard()

	handler := adminGate(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}, true)

	cases := []struct {
		name   string
		remote string
		user   string
		pass   string
		header bool
		want   int
	}{
		{"公网来源一律拒绝", "8.8.8.8:1111", "admin", "s3cret", true, http.StatusForbidden},
		{"内网但没带口令", "192.168.1.9:1111", "", "", true, http.StatusUnauthorized},
		{"内网但口令错误", "192.168.1.9:1111", "admin", "wrong", true, http.StatusUnauthorized},
		{"内网口令对但缺 CSRF 头", "192.168.1.9:1111", "admin", "s3cret", false, http.StatusForbidden},
		{"内网口令对且带头", "192.168.1.9:1111", "admin", "s3cret", true, http.StatusOK},
		{"对端不可解析时拒绝", "garbage", "admin", "s3cret", true, http.StatusForbidden},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "/admin/api/bots", nil)
		req.RemoteAddr = c.remote
		if c.user != "" || c.pass != "" {
			req.SetBasicAuth(c.user, c.pass)
		}
		if c.header {
			req.Header.Set(webUIHeader, "1")
		}
		rec := httptest.NewRecorder()
		handler(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: 得到 %d，期望 %d（body=%s）", c.name, rec.Code, c.want, rec.Body.String())
		}
	}
}

// TestAdminGateNoAuthKeepsSourceGate -no-auth 只关口令，不该把关掉的还有来源闸。
func TestAdminGateNoAuthKeepsSourceGate(t *testing.T) {
	savedAuth, savedLimiter := *webAuth, loginLimiter
	defer func() { *webAuth = savedAuth; loginLimiter = savedLimiter }()

	webAuth.disabled = true
	webAuth.allowPublic = false
	webAuth.trusted = nil
	loginLimiter = newFailureGuard()

	handler := adminGate(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, true)

	req := httptest.NewRequest("GET", "/admin/api/bots", nil)
	req.RemoteAddr = "8.8.8.8:1111"
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("-no-auth 下公网来源仍应被拒，得到 %d", rec.Code)
	}

	req = httptest.NewRequest("GET", "/admin/api/bots", nil)
	req.RemoteAddr = "192.168.1.9:1111"
	req.Header.Set(webUIHeader, "1")
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("-no-auth 下内网来源应放行，得到 %d", rec.Code)
	}
}

// TestAdminGateLockout 连续失败后同一个来源会被 429 挡下，且被挡的请求不再计数。
func TestAdminGateLockout(t *testing.T) {
	savedAuth, savedLimiter := *webAuth, loginLimiter
	defer func() { *webAuth = savedAuth; loginLimiter = savedLimiter }()

	webAuth.user = "admin"
	webAuth.password = "s3cret"
	webAuth.disabled = false
	webAuth.allowPublic = false
	loginLimiter = newFailureGuard()

	handler := adminGate(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, true)

	try := func(pass string) int {
		req := httptest.NewRequest("GET", "/admin/api/bots", nil)
		req.RemoteAddr = "10.9.9.9:2222"
		req.SetBasicAuth("admin", pass)
		req.Header.Set(webUIHeader, "1")
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec.Code
	}

	for i := 0; i < webUIFailLimit; i++ {
		if code := try("wrong"); code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次错误口令应返回 401，得到 %d", i+1, code)
		}
	}
	// 锁定后即使口令正确也先吃 429（锁定期内不校验、也不计数）
	if code := try("s3cret"); code != http.StatusTooManyRequests {
		t.Errorf("锁定期内应返回 429，得到 %d", code)
	}
}

// TestAdminGateErrorCodes 固定闸门返回的稳定 code。
// 前端按 error_code 做多语言渲染，所以这些值是接口契约，改动前要想清楚。
func TestAdminGateErrorCodes(t *testing.T) {
	savedAuth, savedLimiter := *webAuth, loginLimiter
	defer func() { *webAuth = savedAuth; loginLimiter = savedLimiter }()

	webAuth.user = "admin"
	webAuth.password = "s3cret"
	webAuth.disabled = false
	webAuth.allowPublic = false
	webAuth.trusted = nil
	loginLimiter = newFailureGuard()

	handler := adminGate(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, true)

	call := func(remote, user, pass string, header bool) (int, string) {
		req := httptest.NewRequest("GET", "/admin/api/bots", nil)
		req.RemoteAddr = remote
		if user != "" || pass != "" {
			req.SetBasicAuth(user, pass)
		}
		if header {
			req.Header.Set(webUIHeader, "1")
		}
		rec := httptest.NewRecorder()
		handler(rec, req)
		var body struct {
			ErrorCode string `json:"error_code"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body.ErrorCode
	}

	if code, ec := call("8.8.8.8:1111", "admin", "s3cret", true); code != http.StatusForbidden || ec != "admin_local_only" {
		t.Errorf("公网来源：得到 %d / %q，期望 403 / admin_local_only", code, ec)
	}
	if code, ec := call("192.168.1.9:1111", "", "", true); code != http.StatusUnauthorized || ec != "auth_required" {
		t.Errorf("未认证：得到 %d / %q，期望 401 / auth_required", code, ec)
	}
	if code, ec := call("192.168.1.9:1111", "admin", "s3cret", false); code != http.StatusForbidden || ec != "csrf_header_missing" {
		t.Errorf("缺 CSRF 头：得到 %d / %q，期望 403 / csrf_header_missing", code, ec)
	}
	for i := 0; i < webUIFailLimit; i++ {
		call("10.1.1.1:1111", "admin", "wrong", true)
	}
	if code, ec := call("10.1.1.1:1111", "admin", "s3cret", true); code != http.StatusTooManyRequests || ec != "too_many_failures" {
		t.Errorf("锁定后：得到 %d / %q，期望 429 / too_many_failures", code, ec)
	}
}

/* ---------------------------------------------------------------- 界面文案 */

func loadI18nDict(t *testing.T, code string) map[string]interface{} {
	t.Helper()
	data, err := staticFS.ReadFile(i18nDir + "/" + code + ".json")
	if err != nil {
		t.Fatalf("读不到 %s 的文案: %v", code, err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("%s.json 不是合法 JSON: %v", code, err)
	}
	return m
}

// TestI18nFilesHaveSameKeys 各语言文件的 key 必须完全一致：
// 加了新文案却漏了某个语言，这个测试会直接失败。
func TestI18nFilesHaveSameKeys(t *testing.T) {
	entries, err := fs.ReadDir(staticFS, i18nDir)
	if err != nil {
		t.Fatalf("读不到 %s: %v", i18nDir, err)
	}
	base := loadI18nDict(t, i18nDefaultLang)
	if len(base) < 40 {
		t.Fatalf("%s.json 只有 %d 个 key，像是没读全", i18nDefaultLang, len(base))
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		code := strings.TrimSuffix(e.Name(), ".json")
		if code == i18nDefaultLang {
			continue
		}
		got := loadI18nDict(t, code)
		for k := range base {
			if _, ok := got[k]; !ok {
				t.Errorf("%s.json 缺少 key %q（%s.json 里有）", code, k, i18nDefaultLang)
			}
		}
		for k := range got {
			if _, ok := base[k]; !ok {
				t.Errorf("%s.json 多出 key %q（%s.json 里没有）", code, k, i18nDefaultLang)
			}
		}
	}
}

// TestI18nKeysUsedByFrontend 页面里写死的 key 必须都在文案文件里。
// 只检查字面量 key；像 D('err.' + code) 这种拼出来的 key 由前端自己兜底。
func TestI18nKeysUsedByFrontend(t *testing.T) {
	html, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("读不到页面: %v", err)
	}
	dict := loadI18nDict(t, i18nDefaultLang)
	src := string(html)
	used := map[string]bool{}

	for _, m := range regexp.MustCompile(`data-i18n(?:-html|-ph|-title|-alt)?="([^"]+)"`).FindAllStringSubmatch(src, -1) {
		used[m[1]] = true
	}
	// T('x') / D('x')：引号后必须立刻收括号，才说明是完整 key（'err.' + code 会被排除）
	for _, m := range regexp.MustCompile(`\b(?:D|T)\('([^']+)'\)`).FindAllStringSubmatch(src, -1) {
		used[m[1]] = true
	}
	// tf('x', ...)
	for _, m := range regexp.MustCompile(`\btf\('([^']+)'\s*,`).FindAllStringSubmatch(src, -1) {
		used[m[1]] = true
	}

	if len(used) < 40 {
		t.Fatalf("只从页面里解析出 %d 个 key，正则大概没匹配上", len(used))
	}
	for k := range used {
		if _, ok := dict[k]; !ok {
			t.Errorf("页面用到了 %q，但 %s.json 里没有这个 key", k, i18nDefaultLang)
		}
	}
}
