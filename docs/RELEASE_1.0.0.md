<!--
这份文件的内容 = GitHub Release 1.0.0 的正文，发布时整段复制过去。
刻意不加 H1 标题：Release 页面自己会显示版本号，加了会重复。
-->

中文在前，English below.

## 中文

在 [cp0204/WeClawBot-API](https://github.com/cp0204/WeClawBot-API) 基础上加了网页端管理：扫码加账号、解绑、测试推送、看 API 调用日志，不用再 `docker exec` 进容器敲命令。功能和用法见 README。

单文件二进制 9.3 MB，只依赖 `rsc.io/qr`（原项目 4 个依赖）。

### 安装

下载下面两个包里跟机器架构匹配的那个：

```bash
uname -m                        # x86_64 → x86-64 那份，aarch64 → arm64 那份
docker load -i WeClawBot-Api-WebUI_Docker_1.0.0_x86-64.tar.gz
docker run -d --name weclawbot-api-webui --network bridge \
  -p 26322:26322 -v ./config:/app/config \
  --restart unless-stopped weclawbot-api-webui:1.0.0
```

浏览器打开 `http://<机器IP>:26322/`。用户名 `admin`，口令在启动日志里（`docker logs weclawbot-api-webui | head`）；想自己指定就加 `-e WEBUI_USER=xxx -e WEBUI_PASSWORD=xxx`。界面中英双语，按浏览器语言自动选。

### 注意

- **不要把 26322 端口映射到公网**（HTTP 明文，口令会被抓包）

---

## English

An extended version of [cp0204/WeClawBot-API](https://github.com/cp0204/WeClawBot-API): the same HTTP API, plus a built-in WebUI — add accounts by scanning a QR code, unbind them, send test pushes and read the API call log, all from the browser. See the README for details.

Single binary, 9.3 MB, one dependency (`rsc.io/qr`; upstream has four).

### Install

Download the package matching your architecture:

```bash
uname -m                        # x86_64 → the x86-64 package, aarch64 → the arm64 one
docker load -i WeClawBot-Api-WebUI_Docker_1.0.0_x86-64.tar.gz
docker run -d --name weclawbot-api-webui --network bridge \
  -p 26322:26322 -v ./config:/app/config \
  --restart unless-stopped weclawbot-api-webui:1.0.0
```

Open `http://<host-ip>:26322/`. The username is `admin` and the password is printed in the startup log (`docker logs weclawbot-api-webui | head`); pass `-e WEBUI_USER=xxx -e WEBUI_PASSWORD=xxx` to set your own. The UI is bilingual and follows your browser language.

### Notes

- **Do not expose port 26322 to the internet** (plain HTTP — the password would be sniffable).
