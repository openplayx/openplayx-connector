# OpenPlayX Home

家庭常开设备上的 **frpc + sing-box 客户端**。frpc 只通过本机 SOCKS5 连接 [openplayx-relay](https://github.com/openplayx/openplayx-relay)，无论家庭所在地区，均不提供绕过 sing-box 的直连模式。

固定 sing-box **1.12.12**、frp **0.65.0**；发布包 URL/SHA-256 见 `versions.lock.json`。自动安装支持 Linux amd64/arm64、systemd、Python 3.10+；macOS arm64 已完成真实上游进程的本机链路验证。未验证厂商 NAS、Windows 服务、Docker 或其他架构，不假定 NAS 的 GUI 可以直接执行 systemd 安装器。

## 安装

从 relay 管理员取得 **home.json 和 frps.crt**。该配置包含家庭端专属 UUID 和 frp token，不应交给 App；App 使用单独配对文件。

1. 将 home.json 存为 `deployment.json`，将 frps.crt 放入可信的本地路径。
2. 修改 `frpCAPath` 为本机证书绝对路径；保留 `frpServerName` 对应证书身份。
3. 在 `services` 中填写实际 NAS 地址和端口，确认服务 ID、remotePort 与 relay 一致。
4. 检查该主机能够访问媒体服务，再安装。家庭路由器无需开放入站端口。

```bash
chmod 600 deployment.json
sudo python3 scripts/manage.py install --config deployment.json
sudo python3 scripts/manage.py status
```

服务先启动 sing-box，确认其回环 SOCKS5 握手成功后才启动 frpc；frpc 固定使用 TCP + `transport.proxyURL`，验证 frps TLS 证书和身份，并持续重试连接。代理不可用时不会改成直连。代理就绪只代表本地入口正常，上游链路和服务可达性仍需验收。

安装把 CA 复制到受限版本目录，使用 DynamicUser + 专用组读取。重复安装/更新使用既有配置，不生成或覆盖凭据；不要在命令行参数或日志里打印 UUID、token。

## 维护

```bash
sudo python3 scripts/manage.py stop
sudo python3 scripts/manage.py start
sudo python3 scripts/manage.py update --config deployment.json
sudo python3 scripts/manage.py rollback
sudo python3 scripts/manage.py uninstall
```

固定版本、校验后安装；配置先通过 `sing-box check`、`frpc verify`，再原子切换 current。更新前保留完整上版目录；启动失败自动回退，亦可显式 rollback。卸载保留 `/opt/openplayx-home` 中的配置和备份。新上游版本须先审核 lock 文件，更新不会隐式跟随 latest。

不安装系统服务也可生成配置：

```bash
python3 scripts/manage.py render --config deployment.json --output generated
python3 scripts/manage.py check --output generated --bin /path/to/upstream/bin
python3 -m unittest discover -s tests
```

常用映射示例见 [examples/services.json](examples/services.json)，故障排查见 [docs/operations.md](docs/operations.md)。只转发明确配置的服务，不开放整个家庭网段或 NAS 管理端口。VPS frp token 不是媒体服务密码，媒体服务自身的认证须保留。

## 上游与许可证

部署工程使用独立上游二进制。sing-box：GPL-3.0-or-later，另有命名限制；frp：Apache-2.0。分发时附带对应许可证和适用源码。固定版本来源：[sing-box v1.12.12](https://github.com/SagerNet/sing-box/tree/v1.12.12)、[frp v0.65.0](https://github.com/fatedier/frp/tree/v0.65.0)。
