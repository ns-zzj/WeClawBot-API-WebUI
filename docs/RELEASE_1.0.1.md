<!--
这份文件的内容 = GitHub Release 1.0.1 的正文，发布时整段复制过去。
刻意不加 H1 标题：Release 页面自己会显示版本号，加了会重复。
-->

中文在前，English below.

## 中文

补上第三方组件的许可声明，并划清两位版权人各自覆盖的范围。

**没有功能改动**：Go 代码一行没动，二进制与 1.0.0 完全相同，只是镜像里多了一个声明文件。已经在用 1.0.0 的不必升级。

### 变更

- 新增 [THIRD_PARTY_NOTICES.md](https://github.com/ns-zzj/WeClawBot-API-WebUI/blob/main/THIRD_PARTY_NOTICES.md)。依赖的 `rsc.io/qr` 是 BSD 3-Clause，按其要求逐字收录版权声明与许可全文。该文件已随镜像分发，容器内路径 `/app/THIRD_PARTY_NOTICES.md`
- `LICENSE` 中 `NS-ZZJ` 一行加 `(modifications)` 括注，标明该版权仅覆盖修改部分；上游 `cp0204` 那行不受影响
- 中英 README 与 Dockerfile 相应更新

### 安装

下载下面两个包里跟机器架构匹配的那个：

```bash
docker load -i WeClawBot-Api-WebUI_Docker_1.0.1_<架构>.tar.gz
docker run -d --name weclawbot-api-webui --network bridge \
  -p 26322:26322 -v ./config:/app/config \
  --restart unless-stopped weclawbot-api-webui:1.0.1
```

浏览器打开 `http://<机器IP>:26322/`。用户名 `admin`，口令在启动日志里（`docker logs weclawbot-api-webui | head`）；想自己指定就加 `-e WEBUI_USER=xxx -e WEBUI_PASSWORD=xxx`。界面中英双语，按浏览器语言自动选。

### 注意

- **不要把 26322 端口映射到公网**（HTTP 明文，口令会被抓包）
- 本项目自有代码为 MIT；依赖的 `rsc.io/qr` 为 BSD 3-Clause，其声明见 [THIRD_PARTY_NOTICES.md](https://github.com/ns-zzj/WeClawBot-API-WebUI/blob/main/THIRD_PARTY_NOTICES.md)

---

## English

Adds the third-party component notices and clarifies which copyright holder covers what.

**No functional change**: not a line of Go code was touched — the binary is identical to 1.0.0; the image just carries one extra file now. No need to upgrade if you are already on 1.0.0.

### Changes

- Added [THIRD_PARTY_NOTICES.md](https://github.com/ns-zzj/WeClawBot-API-WebUI/blob/main/THIRD_PARTY_NOTICES.md). The bundled `rsc.io/qr` is BSD 3-Clause, so its copyright notice and full license text are reproduced verbatim as that license requires. The file ships inside the image at `/app/THIRD_PARTY_NOTICES.md`.
- The `NS-ZZJ` line in `LICENSE` now carries a `(modifications)` qualifier, scoping that copyright to the changes made here; the upstream `cp0204` line is unaffected.
- Both READMEs and the Dockerfile updated accordingly.

### Install

Download the package matching your architecture:

```bash
docker load -i WeClawBot-Api-WebUI_Docker_1.0.1_<Arch>.tar.gz
docker run -d --name weclawbot-api-webui --network bridge \
  -p 26322:26322 -v ./config:/app/config \
  --restart unless-stopped weclawbot-api-webui:1.0.1
```

Open `http://<host-ip>:26322/`. The username is `admin` and the password is printed in the startup log (`docker logs weclawbot-api-webui | head`); pass `-e WEBUI_USER=xxx -e WEBUI_PASSWORD=xxx` to set your own. The UI is bilingual and follows your browser language.

### Notes

- **Do not expose port 26322 to the internet** (plain HTTP — the password would be sniffable).
- This project's own code is MIT; the bundled `rsc.io/qr` is BSD 3-Clause — see [THIRD_PARTY_NOTICES.md](https://github.com/ns-zzj/WeClawBot-API-WebUI/blob/main/THIRD_PARTY_NOTICES.md).
