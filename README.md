# OpenPlayX Home

家庭常开设备上的 **frpc + sing-box 客户端**。frpc 只通过本机 SOCKS5 连接 [openplayx-relay](https://github.com/openplayx/openplayx-relay)，无论家庭所在地区，均不提供绕过 sing-box 的直连模式。

固定 sing-box **1.12.12**、frp **0.65.0**；发布包 URL/SHA-256 见 `versions.lock.json`。自动安装支持 Linux amd64/arm64、systemd、Python 3.10+；macOS arm64 已完成真实上游进程的本机链路验证。未验证厂商 NAS、Windows 服务、Docker 或其他架构，不假定 NAS 的 GUI 可以直接执行 systemd 安装器。

## 安装

从 Relay 管理员取得配对工具生成的整个 **home/** 目录（含 home.json、私有 secrets.json、grants.json 和受控 TLS 资产），不要接收 relay/ 或 App 配对文件。

将目录保存为 `/var/lib/openplayx-home/`，目录权限 0700、文件 0600。先通过管理页编辑家庭目标 IP/端口；服务 ID 和 relayPort 必须在导入授权内，不能在 Home 页面单方面扩大。

```bash
# 先启动 Web 管理页（可在组件尚未安装时编辑和保存）。
sudo python3 manager/install.py
# 在 http://Home局域网IP:9080 登录，填写实际 NAS 目标并保存。
# 然后安装固定版本组件，读取同一 home.json 和机密引用。
sudo python3 scripts/manage.py install --config /var/lib/openplayx-home/home.json
sudo python3 scripts/manage.py status
```

原有 home.json 若包含明文旧部署格式，先用 `manager/server.py --import-legacy` 迁移到另一个私有目录；不要直接把旧格式当作新权威文件覆盖。详见 [迁移与配置](docs/configuration.md)。

服务先启动 sing-box，确认其回环 SOCKS5 握手成功后才启动 frpc；frpc 固定使用 TCP + `transport.proxyURL`，验证 frps TLS 证书和身份，并持续重试连接。代理不可用时不会改成直连。代理就绪只代表本地入口正常，上游链路和服务可达性仍需验收。

安装把 CA 复制到受限版本目录，使用 DynamicUser + 专用组读取。重复安装/更新使用既有配置，不生成或覆盖凭据；不要在命令行参数或日志里打印 UUID、token。

## 维护

```bash
sudo python3 scripts/manage.py stop
sudo python3 scripts/manage.py start
sudo python3 scripts/manage.py update --config /var/lib/openplayx-home/home.json
sudo python3 scripts/manage.py rollback --config /var/lib/openplayx-home/home.json
sudo python3 scripts/manage.py uninstall
```

固定版本、校验后安装；配置先通过 `sing-box check`、`frpc verify`，再原子切换 current。更新前保留完整上版目录；启动失败自动回退，亦可显式 rollback。卸载保留 `/opt/openplayx-home` 中的配置和备份。新上游版本须先审核 lock 文件，更新不会隐式跟随 latest。

不安装系统服务也可生成配置：

```bash
python3 scripts/manage.py render --config /var/lib/openplayx-home/home.json --output generated
python3 scripts/manage.py check --output generated --bin /path/to/upstream/bin
python3 -m unittest discover -s tests
```

常用映射示例见 [examples/services.json](examples/services.json)，故障排查见 [docs/operations.md](docs/operations.md)。只转发明确配置的服务，不开放整个家庭网段或 NAS 管理端口。VPS frp token 不是媒体服务密码，媒体服务自身的认证须保留。

## 上游与许可证

部署工程使用独立上游二进制。sing-box：GPL-3.0-or-later，另有命名限制；frp：Apache-2.0。分发时附带对应许可证和适用源码。固定版本来源：[sing-box v1.12.12](https://github.com/SagerNet/sing-box/tree/v1.12.12)、[frp v0.65.0](https://github.com/fatedier/frp/tree/v0.65.0)。

## Web 配置管理

已提供类型化 Web 表单、管理员登录、草稿校验与测试、保存/应用分离、状态、脱敏导出和备份/损坏恢复。权威配置为 `home.json`，实际 UUID/token 仅存于私有 `secrets.json`。旧 `deployment.json` 仅作为显式迁移输入，不再是 Web 管理的权威源。启动、迁移、权限与验证说明见 [docs/configuration.md](docs/configuration.md)。

## 家庭局域网直接访问

默认浏览器访问 `http://<Home机器的局域网IP>:9080`，例如 `http://192.168.1.10:9080`，无需 SSH 转发。管理页监听 `0.0.0.0:9080`，保留管理员/15 分钟初始化凭据认证与 CSRF 校验，HTTP 层只接受回环或家庭私有网段来源、到达本机局域网接口的请求。本机 IP 自动识别，额外 DNS 名称仍通过 allowedHosts 配置；不使用任意 Host 白名单。

已有 `home.json` 的显式监听不会被升级静默覆盖。更新代码后，在 Home 仓库执行：

```bash
sudo python3 manager/install.py --enable-lan
```

该命令把已有回环配置保存为新的局域网访问修订、更新管理服务代码并重启服务；媒体代理与家庭映射不改变。已有自定义配置目录时加 `--config-dir /实际目录`。主机防火墙需要允许家庭网段访问 TCP 9080，不配置路由器的公网端口映射。

没有使用 systemd 的手动部署，可先执行 `python3 manager/server.py --config-dir /实际目录 --enable-lan`，然后重新启动同一个管理进程。配置损坏时仍使用回环恢复入口，不会把未知配置公开到局域网。
