# OpenPlayX Home

家庭常开设备上的服务：一个 Go 程序，内含 Web 配置管理、**sing-box 1.12.12**（本机 SOCKS5 → VLESS + TLS）和 **frpc 0.65.0**。frpc 只通过本机 SOCKS5 连接 [openplayx-relay](https://github.com/openplayx/openplayx-relay)，无论家庭所在地区，均不提供绕过 sing-box 的直连模式。

sing-box 与 frp 以 Go 库的形式编译进同一个二进制，版本固定在 `src/go.mod` / `src/go.sum`。运行时不需要 Python，也不再下载上游二进制。自动安装支持 Linux amd64/arm64 + systemd；macOS arm64 已完成进程内真实链路验证。未验证厂商 NAS、Windows 服务、Docker 或其他架构，不假定 NAS 的 GUI 可以直接执行 systemd 安装器；没有 systemd 的设备可以前台运行 `openplayx-home serve`。

## 构建

需要 Go 1.24+。Go 源码、模块文件和内嵌的 Web 页面（`src/web/`）都在 `src/` 下；除 `src/role.go` 外，Relay 与 Home 两个仓库的 `src/` 内容相同。

```bash
scripts/build.sh                       # dist/openplayx-home-linux-{amd64,arm64} + SHA256SUMS
(cd src && go test ./...)             # 单元测试 + 进程内端到端测试
```

## 安装

从 Relay 管理员取得配对工具生成的整个 **home/** 目录（含 home.json、私有 secrets.json、grants.json 和受控 TLS 资产），不要接收 relay/ 或 App 配对文件。

将目录内容放到 `/var/lib/openplayx-home/`（目录 0700、文件 0600），然后：

```bash
sudo ./openplayx-home install
# 在 http://Home局域网IP:9080 登录（首次使用 /var/lib/openplayx-home/bootstrap-token），
# 填写实际 NAS 目标并保存，再点击“应用”；或保存后在本机执行：
sudo openplayx-home apply
sudo openplayx-home status
```

`install` 创建系统账户 `openplayx-home`，把二进制复制到 `/opt/openplayx-home/releases/<时间戳>/` 并切换 `current`，链接 `/usr/local/bin/openplayx-home`，写入 `openplayx-home.service`（参考 `deploy/openplayx-home.service`）。服务以该账户运行，管理页在组件尚未应用时即可编辑和保存。服务 ID 和 relayPort 必须在导入授权内，不能在 Home 页面单方面扩大。

应用时先启动 sing-box，确认其回环 SOCKS5 握手成功后才启动 frpc；frpc 固定使用 TCP + `transport.proxyURL`，验证 frps TLS 证书和身份，并持续重试连接。代理不可用时不会改成直连。组件为 `active` 只代表本地入口正常，上游链路和服务可达性仍需验收。

重复安装/更新使用既有配置，不生成或覆盖凭据；不要在命令行参数或日志里打印 UUID、token。

## 维护

```bash
sudo systemctl stop|start|restart openplayx-home
sudo openplayx-home status
sudo openplayx-home check            # 用内置 sing-box/frpc 校验已保存配置
sudo ./openplayx-home update         # 安装新构建的二进制；启动失败自动恢复原版本
sudo openplayx-home rollback         # 切回上一个已安装二进制
sudo openplayx-home uninstall        # 删除 systemd 服务，保留配置与 releases
```

上游升级需要修改 `src/go.mod` 后重新构建并审核，程序不会自行查询或下载新版本。配置的回退在 Web 管理页使用“恢复上一份配置”再应用。

不安装系统服务也可前台运行或生成配置：

```bash
./openplayx-home serve  --config-dir /path/to/state
./openplayx-home render --config-dir /path/to/state --output generated   # 输出含凭据
```

常用映射示例见 [examples/services.json](examples/services.json)（`home.json` 的 `services` 字段），故障排查见 [docs/operations.md](docs/operations.md)。只转发明确配置的服务，不开放整个家庭网段或 NAS 管理端口。VPS frp token 不是媒体服务密码，媒体服务自身的认证须保留。

### 从 Python 版本迁移

在已有部署上执行 `sudo ./openplayx-home install` 即可：它停用旧的 `openplayx-home-{manager,sing-box,frpc}` 单元，复用原 `/var/lib/openplayx-home` 中的 home.json、secrets.json、grants.json、admin.json、TLS 资产和 last-good.json（管理员密码保持有效），新服务启动后按最近一次成功应用的修订运行组件。新服务确认 active 后才删除旧单元文件；启动失败时重新启用旧单元。

只有旧的扁平 deployment.json 时，用 `openplayx-home import-legacy --config-dir 新的私有目录 --file deployment.json` 导入，不要直接把旧格式当作新权威文件覆盖。详见 [迁移与配置](docs/configuration.md)。

## 上游与许可证

sing-box 以 GPL-3.0-or-later 授权（另有命名限制），本程序静态链接 sing-box，因此本仓库源码及分发的二进制整体按 **GPL-3.0-or-later** 提供，许可证全文见 [COPYING](COPYING)。frp 为 Apache-2.0。分发二进制时须同时提供对应源码。上游源码：[sing-box v1.12.12](https://github.com/SagerNet/sing-box/tree/v1.12.12)、[frp v0.65.0](https://github.com/fatedier/frp/tree/v0.65.0)。

## Web 配置管理

类型化 Web 表单、管理员登录、草稿校验与测试、保存/应用分离、状态、脱敏导出和备份/损坏恢复。权威配置为 `home.json`，实际 UUID/token 仅存于私有 `secrets.json`。启动、迁移、权限与验证说明见 [docs/configuration.md](docs/configuration.md)。

## 家庭局域网直接访问

默认浏览器访问 `http://<Home机器的局域网IP>:9080`，例如 `http://192.168.1.10:9080`，无需 SSH 转发。管理页监听 `0.0.0.0:9080`，保留管理员/15 分钟初始化凭据认证与 CSRF 校验，HTTP 层只接受回环或家庭私有网段来源、到达本机局域网接口的请求。本机 IP 自动识别，额外 DNS 名称仍通过 allowedHosts 配置；不使用任意 Host 白名单。

已有 `home.json` 的显式回环监听不会被升级静默覆盖。需要局域网访问时执行：

```bash
sudo ./openplayx-home install --enable-lan
```

该命令把已有回环配置保存为新的局域网访问修订并重启服务（组件按已应用修订一并重启）；媒体代理与家庭映射不改变。已有自定义配置目录时加 `--config-dir /实际目录`。主机防火墙需要允许家庭网段访问 TCP 9080，不配置路由器的公网端口映射。

没有使用 systemd 的手动部署，可先执行 `openplayx-home enable-lan --config-dir /实际目录`，然后重新启动 `serve` 进程。配置损坏时仍使用回环恢复入口，不会把未知配置公开到局域网。
