package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"os/signal"
	"syscall"
)

const DefaultBaseURL = "https://ilinkai.weixin.qq.com"

type UserConfig struct {
	BotToken      string `json:"bot_token"`
	BotID         string `json:"bot_id"`
	GetUpdatesBuf string `json:"get_updates_buf"`
	IlinkUserID   string `json:"ilink_user_id"`
	ContextToken  string `json:"context_token"`
	APIToken      string `json:"api_token"`
}

type AppConfig struct {
	Bots map[string]*UserConfig `json:"bots"`
}

var (
	configPath = "./config/auth.json"
	cfg        AppConfig
	configLock sync.Mutex
)

// botRuntime 保存每个账号的监听协程状态，解绑时用它把监听真正停掉。
type botRuntime struct {
	cancel  context.CancelFunc
	running bool
	started time.Time
	recv    int64
}

var (
	runtimeLock sync.Mutex
	runtimes    = make(map[string]*botRuntime)
)

// startMonitor 为账号启动监听协程；已在运行时直接返回。
func startMonitor(user *UserConfig) {
	runtimeLock.Lock()
	if rt, ok := runtimes[user.BotID]; ok && rt.running {
		runtimeLock.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt := &botRuntime{cancel: cancel, running: true, started: time.Now()}
	runtimes[user.BotID] = rt
	runtimeLock.Unlock()

	go monitorWeixin(ctx, user, rt)
}

// stopMonitor 取消账号的监听协程，正在进行的微信长轮询会被立刻中断。
func stopMonitor(botID string) {
	runtimeLock.Lock()
	rt, ok := runtimes[botID]
	if ok {
		rt.running = false
		delete(runtimes, botID)
	}
	runtimeLock.Unlock()

	if ok && rt.cancel != nil {
		rt.cancel()
	}
}

// registerBot 保存扫码得到的凭证并启动监听。扫码登录只从网页发起。
func registerBot(botToken, botID, ilinkUserID string) *UserConfig {
	configLock.Lock()
	user := &UserConfig{
		BotToken:    botToken,
		BotID:       botID,
		IlinkUserID: ilinkUserID,
		APIToken:    generateToken(16),
	}
	// 同一账号重新扫码时沿用旧的游标与上下文，避免丢消息
	if old, ok := cfg.Bots[botID]; ok {
		user.GetUpdatesBuf = old.GetUpdatesBuf
		user.ContextToken = old.ContextToken
	}
	cfg.Bots[botID] = user
	configLock.Unlock()

	saveConfig()

	// 重新扫码时先停掉旧监听协程，否则旧的 BotToken 还在长轮询，
	// 新的上下文与游标也写不进新配置里。
	stopMonitor(botID)
	startMonitor(user)
	return user
}

// removeBot 解绑账号：停掉监听协程、删除本地配置并落盘。
// 微信侧的 ClawBot 连接不会因此断开，需要用户自行到微信里操作。
func removeBot(botID string) bool {
	configLock.Lock()
	if _, ok := cfg.Bots[botID]; !ok {
		configLock.Unlock()
		return false
	}
	delete(cfg.Bots, botID)
	configLock.Unlock()

	stopMonitor(botID)
	saveConfig()
	return true
}

func generateToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func main() {
	port := flag.Int("port", 26322, "API server port")
	logFile := flag.String("log-file", "", "Optional path to append API call logs as JSONL (empty = memory only)")
	logSize := flag.Int("log-size", 1000, "Number of API call log entries kept in memory")
	noAuth := flag.Bool("no-auth", false, "Disable the WebUI password (NOT recommended; the source restriction still applies)")
	trustCIDR := flag.String("trust-cidr", "", "Extra CIDRs allowed to reach the WebUI, comma separated, e.g. 100.64.0.0/10 for Tailscale")
	allowPublic := flag.Bool("allow-public", false, "Allow the WebUI from ANY source address, including the internet (NOT recommended)")
	flag.Parse()

	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		log.Fatalf("Init config dir failed: %v", err)
	}

	// 先抢监听端口，而且必须在读配置之前抢：
	//   抢得到 → 本进程是唯一的那个实例，继续完整启动
	//   抢不到 → 这里已经有一个实例在跑了（典型场景：docker exec 进来又执行了一次本程序）。
	//            此时还没有 loadConfig/saveConfig，所以这个进程既不会起第二套消息监听，
	//            也不会拿自己内存里的旧配置把主实例刚写进去的账号覆盖掉。
	//            直接打印提示并退出——管理请用网页。
	addr := fmt.Sprintf(":%d", *port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Printf("\n端口 %s 已被占用：这个环境里已经有一个实例在运行（也可能是别的程序占着这个端口）。\n", addr)
		fmt.Printf("WebUI: http://<这台机器的IP>:%d/\n", *port)
		fmt.Println("旧版的 /login、/bots、/del 控制台已经移除，这些操作现在都在网页上。")
		return
	}

	initWebUIAuth(*noAuth, *trustCIDR, *allowPublic)

	apiLogs = newLogStore(*logSize)
	if *logFile != "" {
		if err := apiLogs.enableFile(*logFile); err != nil {
			log.Printf("Warning: cannot append log file %s: %v", *logFile, err)
		} else {
			fmt.Printf("API call logs are also appended to %s\n", *logFile)
		}
	}

	loadConfig()

	if len(cfg.Bots) == 0 {
		fmt.Println("No login bots found. Open the WebUI in a browser and click \"Add account\" to scan a QR code.")
	} else {
		fmt.Printf("Loaded %d bots.\n", len(cfg.Bots))
	}

	configLock.Lock()
	// 为已存在但缺 token 的用户补齐 APIToken
	for _, user := range cfg.Bots {
		if user.APIToken == "" {
			user.APIToken = generateToken(16)
		}
	}
	configLock.Unlock()
	saveConfig()

	// 监听退出信号，安全退出
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println("\nReceived shutdown signal. Saving config and exiting...")
		saveConfig()
		os.Exit(0)
	}()

	// 注册全部路由（路由要先注册好，再开始 serve）
	registerWebUI()
	registerBotsAPI()
	printAccessSummary()

	// 启动所有已有账号的监听协程（端口已经在前面抢到手，所以这里一定是唯一的那个实例）
	for _, userCfg := range cfg.Bots {
		startMonitor(userCfg)
	}

	go func() {
		fmt.Printf("API Server listening on http://0.0.0.0%s\n", addr)
		fmt.Printf("WebUI  available at http://0.0.0.0%s/\n", addr)
		if err := http.Serve(ln, nil); err != nil {
			log.Printf("HTTP server stopped: %v", err)
		}
	}()

	// 没有控制台了：进程只做一件事——提供服务，直到收到退出信号
	select {}
}

func sendJSON(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(data)
}

func getReqParam(r *http.Request, key string, jsonBody map[string]interface{}) string {
	if val, ok := jsonBody[key]; ok {
		return fmt.Sprint(val)
	}
	return r.FormValue(key)
}

// registerBotsAPI 注册对外推送接口 /bots/*。它只注册路由、不负责监听：
// 监听统一由 main 里抢到的那个 listener 承担，"抢不到端口"才能成为
// "同一个环境里已有实例在跑"的可靠信号。
func registerBotsAPI() {
	http.HandleFunc("/bots/", apiLogMiddleware(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/bots/")
		parts := strings.Split(path, "/")
		if len(parts) < 2 {
			sendJSON(w, http.StatusNotFound, map[string]interface{}{"code": 404, "error": "Not Found"})
			return
		}

		botID := parts[0]
		action := parts[1]

		// 解析参数：支持 JSON, Multipart, Form, Query
		jsonBody := make(map[string]interface{})
		ct := r.Header.Get("Content-Type")
		if strings.Contains(ct, "application/json") {
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &jsonBody)
		} else if strings.Contains(ct, "multipart/form-data") {
			r.ParseMultipartForm(10 << 20)
		} else {
			r.ParseForm()
		}

		token := ""
		authHeader := r.Header.Get("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			token = strings.TrimPrefix(authHeader, "Bearer ")
		} else {
			token = getReqParam(r, "token", jsonBody)
		}

		configLock.Lock()
		user, exists := cfg.Bots[botID]
		configLock.Unlock()

		if !exists {
			sendJSON(w, http.StatusNotFound, map[string]interface{}{"code": 404, "error": "Bot not found"})
			return
		}
		if user.APIToken != token || token == "" {
			sendJSON(w, http.StatusUnauthorized, map[string]interface{}{"code": 401, "error": "Unauthorized"})
			return
		}

		switch action {
		case "messages":
			text := getReqParam(r, "text", jsonBody)
			if text == "" {
				sendJSON(w, http.StatusBadRequest, map[string]interface{}{"code": 400, "error": "Missing text"})
				return
			}
			if user.IlinkUserID == "" || user.ContextToken == "" {
				sendJSON(w, http.StatusBadRequest, map[string]interface{}{"code": 400, "error": "Context not ready"})
				return
			}
			if err := sendMessage(user, user.IlinkUserID, text, user.ContextToken); err != nil {
				sendJSON(w, http.StatusInternalServerError, map[string]interface{}{"code": 500, "error": err.Error()})
			} else {
				sendJSON(w, http.StatusOK, map[string]interface{}{"code": 200, "message": "OK"})
			}

		case "typing":
			statusStr := getReqParam(r, "status", jsonBody)
			status, _ := strconv.Atoi(statusStr)
			if status == 0 {
				status = 1 // Default to typing
			}
			if err := sendTypingWeixin(user, status); err != nil {
				sendJSON(w, http.StatusInternalServerError, map[string]interface{}{"code": 500, "error": err.Error()})
			} else {
				sendJSON(w, http.StatusOK, map[string]interface{}{"code": 200, "message": "OK"})
			}
		default:
			sendJSON(w, http.StatusNotFound, map[string]interface{}{"code": 404, "error": "Unknown action"})
		}
	}))
}

func getBotConfig(user *UserConfig) (string, error) {
	reqData := map[string]interface{}{
		"ilink_user_id": user.IlinkUserID,
		"context_token": user.ContextToken,
		"base_info": map[string]string{
			"channel_version": "1.0.0",
		},
	}
	b, _ := json.Marshal(reqData)
	req, _ := http.NewRequest("POST", DefaultBaseURL+"/ilink/bot/getconfig", bytes.NewReader(b))
	commonHeaders(req, true, user.BotToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var res struct {
		Ret          int    `json:"ret"`
		TypingTicket string `json:"typing_ticket"`
	}
	json.NewDecoder(resp.Body).Decode(&res)
	if res.Ret != 0 {
		return "", fmt.Errorf("getconfig ret %d", res.Ret)
	}
	return res.TypingTicket, nil
}

func sendTypingWeixin(user *UserConfig, status int) error {
	ticket, err := getBotConfig(user)
	if err != nil {
		return err
	}

	reqData := map[string]interface{}{
		"ilink_user_id": user.IlinkUserID,
		"typing_ticket": ticket,
		"status":        status,
		"base_info": map[string]string{
			"channel_version": "1.0.0",
		},
	}
	b, _ := json.Marshal(reqData)
	req, _ := http.NewRequest("POST", DefaultBaseURL+"/ilink/bot/sendtyping", bytes.NewReader(b))
	commonHeaders(req, true, user.BotToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var res struct {
		Ret int `json:"ret"`
	}
	json.NewDecoder(resp.Body).Decode(&res)
	if res.Ret != 0 {
		return fmt.Errorf("sendtyping ret %d", res.Ret)
	}
	return nil
}

func loadConfig() {
	configLock.Lock()
	defer configLock.Unlock()
	data, err := os.ReadFile(configPath)
	if err == nil {
		_ = json.Unmarshal(data, &cfg)
	}
	if cfg.Bots == nil {
		cfg.Bots = make(map[string]*UserConfig)
	}
}

func saveConfig() {
	configLock.Lock()
	defer configLock.Unlock()
	data, _ := json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile(configPath, data, 0644)
}

func randomWechatUin() string {
	b := make([]byte, 4)
	rand.Read(b)
	val := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	return base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(uint64(val), 10)))
}

func commonHeaders(req *http.Request, isJson bool, token string) {
	if isJson {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("AuthorizationType", "ilink_bot_token")
	req.Header.Set("X-WECHAT-UIN", randomWechatUin())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func monitorWeixin(ctx context.Context, user *UserConfig, rt *botRuntime) {
	// 退出时把运行状态标记掉，网页上的"监听中"徽章据此变灰
	defer func() {
		runtimeLock.Lock()
		if cur, ok := runtimes[user.BotID]; ok && cur == rt {
			cur.running = false
		}
		runtimeLock.Unlock()
	}()

	fmt.Printf("[Bot: %s] Started listening for messages...\n", user.BotID)
	client := &http.Client{Timeout: 45 * time.Second}
	timeoutMs := 35000

	for {
		if ctx.Err() != nil {
			fmt.Printf("[Bot: %s] Listener stopped.\n", user.BotID)
			return
		}

		reqData := map[string]interface{}{
			"get_updates_buf": user.GetUpdatesBuf,
			"base_info": map[string]string{
				"channel_version": "1.0.0",
			},
		}
		b, _ := json.Marshal(reqData)

		// 带上 ctx，解绑时可以立刻中断正在进行的 35 秒长轮询
		req, _ := http.NewRequestWithContext(ctx, "POST", DefaultBaseURL+"/ilink/bot/getupdates", bytes.NewReader(b))
		commonHeaders(req, true, user.BotToken)

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				continue // 回到循环开头统一判断退出
			}
			time.Sleep(2 * time.Second)
			continue
		}

		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			time.Sleep(2 * time.Second)
			continue
		}

		type MessageItem struct {
			Type     int `json:"type"`
			TextItem struct {
				Text string `json:"text"`
			} `json:"text_item"`
		}

		type WeixinMessage struct {
			FromUserID   string        `json:"from_user_id"`
			ContextToken string        `json:"context_token"`
			ItemList     []MessageItem `json:"item_list"`
		}

		var updateRes struct {
			Ret                  int             `json:"ret"`
			Errcode              int             `json:"errcode"`
			GetUpdatesBuf        string          `json:"get_updates_buf"`
			LongpollingTimeoutMs int             `json:"longpolling_timeout_ms"`
			Msgs                 []WeixinMessage `json:"msgs"`
		}

		json.Unmarshal(bodyBytes, &updateRes)

		if updateRes.Ret != 0 || updateRes.Errcode != 0 {
			time.Sleep(2 * time.Second)
			continue
		}

		if updateRes.LongpollingTimeoutMs > 0 {
			timeoutMs = updateRes.LongpollingTimeoutMs
			client.Timeout = time.Duration(timeoutMs+10000) * time.Millisecond
		}

		if updateRes.GetUpdatesBuf != "" {
			configLock.Lock()
			user.GetUpdatesBuf = updateRes.GetUpdatesBuf
			configLock.Unlock()
			saveConfig()
		}

		for _, msg := range updateRes.Msgs {
			atomic.AddInt64(&rt.recv, 1)
			if msg.FromUserID != "" {
				configLock.Lock()
				if msg.ContextToken != "" {
					user.ContextToken = msg.ContextToken
				}
				configLock.Unlock()
				saveConfig()
			}

			for _, item := range msg.ItemList {
				if item.Type == 1 && item.TextItem.Text != "" {
					fmt.Printf("\n[Bot: %s | Message from %s]: %s\n> ", user.BotID, msg.FromUserID, item.TextItem.Text)
				} else {
					fmt.Printf("\n[Bot: %s | Message from %s]: <Media/Other type %d>\n> ", user.BotID, msg.FromUserID, item.Type)
				}
			}
		}
	}
}

func sendMessage(user *UserConfig, to string, text string, contextToken string) error {
	reqData := map[string]interface{}{
		"msg": map[string]interface{}{
			"from_user_id":  "",
			"to_user_id":    to,
			"client_id":     fmt.Sprintf("openclaw-weixin:%d-%x", time.Now().UnixMilli(), func() []byte { b := make([]byte, 4); rand.Read(b); return b }()),
			"message_type":  2,
			"message_state": 2,
			"context_token": contextToken,
			"item_list": []map[string]interface{}{
				{
					"type": 1,
					"text_item": map[string]string{
						"text": text,
					},
				},
			},
		},
		"base_info": map[string]string{
			"channel_version": "1.0.2",
		},
	}

	b, _ := json.Marshal(reqData)
	req, _ := http.NewRequest("POST", DefaultBaseURL+"/ilink/bot/sendmessage", bytes.NewReader(b))
	commonHeaders(req, true, user.BotToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	var res struct {
		Ret     int    `json:"ret"`
		Errcode int    `json:"errcode"`
		Errmsg  string `json:"errmsg"`
		ErrMsg  string `json:"err_msg"`
	}
	json.Unmarshal(bodyBytes, &res)

	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	if res.Ret != 0 || res.Errcode != 0 {
		msg := res.Errmsg
		if msg == "" {
			msg = res.ErrMsg
		}
		if msg == "" {
			msg = string(bodyBytes) // 如果没有明确的消息字段，显示完整响应体
		}
		return fmt.Errorf("API Error: ret=%d, errcode=%d, msg=%s", res.Ret, res.Errcode, msg)
	}
	return nil
}
