# OpenPlayX Connector

仓库：[openplayx/openplayx-connector](https://github.com/openplayx/openplayx-connector)。程序名、系统服务及配置目录统一使用 `openplayx-connector`，主配置文件为 `connector.json`。Jenkins 发布在检查和打包通过后使用`v主版本.次版本.补丁版本` tag，首次 `v0.1.0`，之后自动递增补丁号。

[English](README.md) | 简体中文

[![License: GPL v3+](https://img.shields.io/badge/license-GPLv3%2B-blue.svg)](COPYING)

[OpenPlayX](https://github.com/openplayx/openplayx) 远程访问的家庭端服务，运行在家里常开的设备上：一个 Go 程序，内含 Web 配置管理、**sing-box 1.12.12**（本机 SOCKS5 → VLESS + TLS）和 **frpc 0.65.0**。它把你明确配置的家庭服务（Jellyfin、Emby、SMB 等）注册到 VPS 上的 [openplayx-relay](https://github.com/openplayx/openplayx-relay)，让 App 在外面也能访问。

- **单个二进制**：sing-box 与 frp 以 Go 库的形式编译进来，版本固定在 `src/go.mod` / `src/go.sum`。运行时不需要 Python，也不下载上游二进制。
- **没有直连模式**：frpc 只通过本机 SOCKS5 连接 Relay，无论家庭所在地区，都不提供绕过 sing-box 的直连。代理不可用时不会改成直连。
- **只转发明确配置的服务**：不开放整个家庭网段或 NAS 管理端口。
- **Web 配置管理**：在家庭局域网内直接用 IP 打开，带管理员登录、草稿校验、保存/应用分离、备份与恢复。

自动安装支持 Linux amd64/arm64 + systemd；macOS arm64 已完成进程内真实链路验证。未验证厂商 NAS、Windows 服务、Docker 或其他架构，不假定 NAS 的 GUI 可以直接执行 systemd 安装器；没有 systemd 的设备可以前台运行 `openplayx-connector serve`。

## 构建

需要 Go 1.24+。Go 源码、模块文件和内嵌的 Web 页面（`src/web/`）都在 `src/` 下；除 `src/role.go` 外，Relay 与 Connector 两个仓库的 `src/` 内容相同。

页面使用 `src/web/ui/` 中的 OpenPlayX 共享 UI 套件（Basecoat 加 OpenPlayX 主题）。它同步自 `openplayx/web` 仓库的 `ui/`，请在那里修改，不要直接改这里；副本被改动时 `go test` 会失败。

```bash
scripts/build.sh                       # dist/openplayx-connector-linux-{amd64,arm64} + SHA256SUMS
(cd src && go test ./...)             # 单元测试 + 进程内端到端测试
```

## 平台模式（API 管理的 Relay）

当前平台节点（例如 `syd1`）使用此模式，不能使用旧的 `pair` 配置代替账号绑定。
Linux/systemd 安装：

```sh
sudo ./openplayx-connector install
sudo mkdir -p /etc/systemd/system/openplayx-connector.service.d
printf '[Service]\nEnvironment=OPENPLAYX_CONNECTOR_PLATFORM=1\n' | sudo tee /etc/systemd/system/openplayx-connector.service.d/platform.conf >/dev/null
sudo systemctl daemon-reload
sudo systemctl restart openplayx-connector
```

管理页 `http://<VM 局域网 IP>:9080`，本机首次凭据位于
`/var/lib/openplayx-connector/bootstrap-token`，有效 15 分钟；首次登录后设置本机管理员密码。
前台运行可用 `serve --platform --config-dir <目录>`。
管理页生成配对码后，在 `https://openplayx.com/account/connector` 用手机相同账号确认。
平台账号密码只提交给第一方 HTTPS API，不提交给 VM 管理页。
绑定后机器凭据保存在私有 `platform.json`（0600），不返回到管理页面。
Relay 节点自动选择：Connector 测量本机到各启用节点的 TCP 连接耗时，使用最快的可达节点；
节点仍在列表中且可达时保持不变，被移除或连续 3 次探测失败（每 30 秒一次）时自动切换。
当前节点上报给平台，App 跟随该节点，不再提供手动选择。本地服务填写名称、类型、目标 IP 和端口（例如
Jellyfin `127.0.0.1:18096`、SMB `127.0.0.1:445`）。只支持显式服务映射；目标限回环和私网地址。
也可点「扫描局域网服务」，在后台运行（端口 1–65535 约需 1–3 分钟）：先发现在线主机（NetBIOS、ARP、TCP 探测）
和自报服务（SSDP/DLNA、Plex GDM、Jellyfin/Emby UDP 7359、mDNS），再对本机和所在私网 /24 网段做所选范围的 TCP 端口扫描，
同时对相同 UDP 范围做 QUIC 探测，最后逐个识别开放端口的协议：SMB、NFS、FTP/FTPES、隐式 FTPS、SSH/SFTP、HTTP、HTTPS、
HTTP/3、WebDAV、DLNA、Jellyfin、Emby、Plex。结果点「使用」填入表单；FTP、DLNA 和 UDP 结果只展示、不能映射。保存前不会注册任何服务。
平台自动分配远程端口，服务注册幂等重试；本地目标地址不上传到 API。
配置完成后在 `https://openplayx.com/account/` 查看同步结果；同账号的手机、平板无需逐台授权，刷新平台配置后可访问自己的启用服务。

服务更新通过 frpc 动态重载，不重建 sing-box / frpc 连接；节点、凭据变化可能重建传输。
平台刷新失败保留现有路由；明确身份撤销时停止传输；当前节点被移除或不可达时切换到延迟最低的可达节点。
frpc 明确加载系统 CA 文件并验证节点主机名，不使用跳过 TLS 验证的默认行为。
请仅在可信局域网或通过 SSH 隧道访问本机管理页，不做公网端口映射。

### 在网页预注册

也可以不用配对码：在 `https://openplayx.com/account/connectors` 点击「预注册 Connector」，填写名称并选择平台，
下载对应安装包和一次性注册文件（`openplayx-connector-enrollment.json`，7 天有效，只显示一次）。安装并启动后即自动连上：

```sh
# Linux（systemd）：写入平台模式 drop-in 并导入注册文件
tar xzf openplayx-connector-<版本>-linux-<架构>-<sha>.tar.gz
sudo ./openplayx-connector-<版本>-linux-<架构>/openplayx-connector install --enroll openplayx-connector-enrollment.json
# macOS / FreeBSD / 其他：前台运行
./openplayx-connector serve --enroll openplayx-connector-enrollment.json --config-dir "$HOME/.openplayx-connector"
```

`--enroll` 隐含平台模式。程序校验文件（签发 API、有效期、令牌格式）后复制到私有状态目录，导入 `platform.json` 并删除文件；
令牌只作为一次性绑定凭据使用。未领取的预注册占用 Connector 配额，直到被领取、过期或在网页上取消。
已取消或过期的注册文件会在本机清除，之后可改用配对码绑定。

## 快速开始：安装

从 Relay 管理员取得 `openplayx-relay pair` 生成的整个 **connector/** 目录（含 connector.json、私有 secrets.json 和受控 TLS 资产），不要接收 relay/ 或 app.json。

将目录内容放到 `/var/lib/openplayx-connector/`（目录 0700、文件 0600），然后：

```bash
sudo ./openplayx-connector install
# 在 http://Home局域网IP:9080 登录（首次使用 /var/lib/openplayx-connector/bootstrap-token），
# 在 frpc.proxies 中填写实际 NAS 目标并保存，再点击“应用”；或保存后在本机执行：
sudo openplayx-connector apply
sudo openplayx-connector status
```

`install` 创建系统账户 `openplayx-connector`，把二进制复制到 `/opt/openplayx-connector/releases/<时间戳>/` 并切换 `current`，链接 `/usr/local/bin/openplayx-connector`，写入 `openplayx-connector.service`（参考 `deploy/openplayx-connector.service`）。服务以该账户运行，管理页在组件尚未应用时即可编辑和保存。`frpc.proxies` 即服务注册：`remotePort` 必须在 Relay 的 `frps.allowPorts` 内，否则 frps 拒绝该 proxy。常用映射示例见 [examples/proxies.json](examples/proxies.json)（`connector.json` 的 `frpc.proxies` 字段）。

应用时先启动 sing-box，确认其回环 SOCKS5 握手成功后才启动 frpc；frpc 固定使用 TCP + `transport.proxyURL`，验证 frps TLS 证书和身份，并持续重试连接。组件为 `active` 只代表本地入口正常，上游链路和服务可达性仍需验收。

重复安装/更新使用既有配置，不生成或覆盖凭据；不要在命令行参数或日志里打印 UUID、token。VPS frp token 不是媒体服务密码，媒体服务自身的认证须保留。

## 配置

当前为 `schemaVersion: 2`：网络层用 sing-box 字段（`singBox.inbound` / `singBox.outbound`），注册层用 frp 字段（`frpc`）。旧格式（`homeId`、`services`、`grants.json`）不再读取，也不提供自动导入。权威配置为 `connector.json`，实际 UUID/token 仅存于私有 `secrets.json`。配置结构、启动、权限与验证说明见 [docs/configuration.zh-CN.md](docs/configuration.zh-CN.md)。

## Web 配置管理

页面分为“传输链路”和“可用服务”两部分，默认展示已保存配置，展开对应区域可修改。Connector 管理实际服务映射；Relay 管理允许注册的端口范围与身份访问权限。证书、日志和管理入口收在链路维护选项中。修改后在页面顶部保存，再应用已保存配置。

### 家庭局域网直接访问

默认在浏览器访问 `http://<Home机器的局域网IP>:9080`，例如 `http://192.168.1.10:9080`，无需 SSH 转发。管理页监听 `0.0.0.0:9080`，保留管理员/15 分钟初始化凭据认证与 CSRF 校验，HTTP 层只接受回环或家庭私有网段来源、到达本机局域网接口的请求。本机 IP 自动识别，额外 DNS 名称仍通过 allowedHosts 配置；不使用任意 Host 白名单。

已有 `connector.json` 的显式回环监听不会被升级静默覆盖。需要局域网访问时执行：

```bash
sudo ./openplayx-connector install --enable-lan
```

该命令把已有回环配置保存为新的局域网访问修订并重启服务（组件按已应用修订一并重启）；媒体代理与家庭映射不改变。已有自定义配置目录时加 `--config-dir /实际目录`。主机防火墙需要允许家庭网段访问 TCP 9080，不要在路由器上做公网端口映射。

没有使用 systemd 的手动部署，可先执行 `openplayx-connector enable-lan --config-dir /实际目录`，然后重新启动 `serve` 进程。配置损坏时仍使用回环恢复入口，不会把未知配置公开到局域网。

## 维护

```bash
sudo systemctl stop|start|restart openplayx-connector
sudo openplayx-connector status
sudo openplayx-connector check            # 用内置 sing-box/frpc 校验已保存配置
sudo ./openplayx-connector update         # 安装新构建的二进制；启动失败自动恢复原版本
sudo openplayx-connector rollback         # 切回上一个已安装二进制
sudo openplayx-connector uninstall        # 删除 systemd 服务，保留配置与 releases
```

上游升级需要修改 `src/go.mod` 后重新构建并审核，程序不会自行查询或下载新版本。配置的回退在 Web 管理页使用“恢复上一份配置”再应用。

不安装系统服务也可前台运行或生成配置：

```bash
./openplayx-connector serve  --config-dir /path/to/state
./openplayx-connector render --config-dir /path/to/state --output generated   # 输出含凭据
```

部署拓扑、故障排查和验证边界见 [docs/operations.zh-CN.md](docs/operations.zh-CN.md)。

## 参与贡献

欢迎提 issue 和 PR，见 [CONTRIBUTING.md](CONTRIBUTING.md)（英文）。安全问题请按 [SECURITY.md](SECURITY.md)（英文）私下报告。

## 声明

本项目用于远程访问你自己家里的服务。只映射你有权访问的服务，并遵守你所在地区关于网络和加密通信的法律。不保证任何网络环境（包括跨境线路）下的可用性。

## 上游与许可证

sing-box 以 GPL-3.0-or-later 授权（另有命名限制），本程序静态链接 sing-box，因此本仓库源码及分发的二进制整体按 **GPL-3.0-or-later** 提供，许可证全文见 [COPYING](COPYING)。frp 为 Apache-2.0。分发二进制时须同时提供对应源码。上游源码：[sing-box v1.12.12](https://github.com/SagerNet/sing-box/tree/v1.12.12)、[frp v0.65.0](https://github.com/fatedier/frp/tree/v0.65.0)。

平台模式的服务新增、名称/类型/本地目标编辑、启停和删除都在本机管理页完成。配置仅存在私有 platform.json，由 Connector 用机器身份同步云端元数据；本地 IP、端口和服务密码不会上传。注册请求先冻结并持久化正文，响应丢失后可幂等重试；删除标记在断网后保留，先停止本地映射，再重试清理云端。云端用户中心不能修改服务。
