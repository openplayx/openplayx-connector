# OpenPlayX Home

English | [简体中文](README.zh-CN.md)

[![License: GPL v3+](https://img.shields.io/badge/license-GPLv3%2B-blue.svg)](COPYING)

The home side of [OpenPlayX](https://github.com/openplayx/openplayx) remote
access, running on an always-on device at home: one Go program with a web
configuration manager, **sing-box 1.12.12** (local SOCKS5 → VLESS + TLS) and
**frpc 0.65.0**. It registers the home services you configure explicitly
(Jellyfin, Emby, SMB, …) with [openplayx-relay](https://github.com/openplayx/openplayx-relay)
on a VPS, so the app can reach them from outside.

- **One binary**: sing-box and frp are compiled in as Go libraries, pinned in
  `src/go.mod` / `src/go.sum`. No Python at runtime, no upstream binaries
  downloaded.
- **No direct mode**: frpc reaches the Relay only through the local SOCKS5
  proxy, wherever the home is. There is no way around sing-box, and the program
  never falls back to a direct connection when the proxy is down.
- **Only explicitly configured services** are forwarded; never a whole home
  network or a NAS admin port.
- **Web configuration**, opened directly by IP inside the home network, with
  admin login, draft validation, separate save and apply, backups and recovery.

Automatic installation supports Linux amd64/arm64 with systemd; the real
in-process link is also verified on macOS arm64. Vendor NAS systems, Windows
services, Docker and other architectures are not verified, and a NAS GUI is not
assumed to run the systemd installer. Devices without systemd can run
`openplayx-home serve` in the foreground.

## Build

Requires Go 1.24+. The Go sources, module files and the embedded web page
(`src/web/`) are under `src/`; apart from `src/role.go`, `src/` is identical in
the Relay and Home repositories.

```bash
scripts/build.sh                       # dist/openplayx-home-linux-{amd64,arm64} + SHA256SUMS
(cd src && go test ./...)             # unit tests + in-process end-to-end tests
```

## Quick start: install

Get the whole **home/** directory produced by `openplayx-relay pair` from the
Relay administrator (home.json, the private secrets.json and the managed TLS
assets). Do not accept relay/ or app.json.

Put its contents into `/var/lib/openplayx-home/` (directory 0700, files 0600),
then:

```bash
sudo ./openplayx-home install
# Log in at http://<home LAN IP>:9080 (first time: /var/lib/openplayx-home/bootstrap-token),
# enter the real NAS targets in frpc.proxies, save and click Apply; or after saving, run locally:
sudo openplayx-home apply
sudo openplayx-home status
```

`install` creates the system account `openplayx-home`, copies the binary to
`/opt/openplayx-home/releases/<timestamp>/` and switches `current`, links
`/usr/local/bin/openplayx-home`, and writes `openplayx-home.service` (see
`deploy/openplayx-home.service`). The service runs as that account; the admin
page can edit and save before any components are applied. `frpc.proxies` is
the service registration: each `remotePort` must be inside the Relay's
`frps.allowPorts`, otherwise frps rejects that proxy. Common mappings are in
[examples/proxies.json](examples/proxies.json) (the `frpc.proxies` field of
`home.json`).

On apply, sing-box starts first; frpc starts only after a successful SOCKS5
handshake on loopback. frpc always uses TCP + `transport.proxyURL`, verifies the
frps TLS certificate and identity, and keeps retrying. A component showing
`active` only means the local entry point works; the upstream link and the
reachability of services still need checking.

Reinstalling or updating keeps the existing configuration and never creates or
overwrites credentials. Do not print UUIDs or tokens in command-line arguments
or logs. The VPS frp token is not a media service password: keep the media
services' own authentication.

## Configuration

The current format is `schemaVersion: 2`: sing-box fields for the network layer
(`singBox.inbound` / `singBox.outbound`) and frp fields for the registration
layer (`frpc`). The old format (`homeId`, `services`, `grants.json`) is no
longer read and not imported automatically. The authoritative configuration is
`home.json`; the real UUIDs and tokens are only in the private `secrets.json`.
Structure, startup, permissions and verification are described in
[docs/configuration.md](docs/configuration.md).

## Web configuration

The page has two parts, **transport link** and **available services**. It shows
the saved configuration; expand a section to edit it. The Home manages the
actual service mappings; the Relay manages the registrable port range and
identity permissions. Certificates, logs and admin access are under the link
maintenance options. Save at the top of the page, then apply the saved
configuration.

### Direct access from the home network

By default, open `http://<home machine's LAN IP>:9080` in a browser, for example
`http://192.168.1.10:9080`; no SSH forwarding needed. The admin page listens on
`0.0.0.0:9080` and keeps admin / 15-minute bootstrap credential authentication
and CSRF checks. At the HTTP layer it accepts only requests from loopback or
private home networks that arrive on a local LAN interface. Local IPs are
detected automatically; extra DNS names go into allowedHosts. There is no
wildcard Host allowlist.

An explicit loopback listener in an existing `home.json` is not silently
replaced by an upgrade. To enable LAN access:

```bash
sudo ./openplayx-home install --enable-lan
```

This saves the existing loopback configuration as a new LAN-access revision and
restarts the service (components restart with the applied revision); media
proxying and home mappings do not change. Add `--config-dir /actual/dir` for a
custom config directory. The host firewall must allow TCP 9080 from the home
network; do not forward the port on the router.

For manual deployments without systemd, run
`openplayx-home enable-lan --config-dir /actual/dir`, then restart the `serve`
process. With a damaged configuration the program still uses the loopback
recovery entry and never exposes an unknown configuration to the LAN.

## Maintenance

```bash
sudo systemctl stop|start|restart openplayx-home
sudo openplayx-home status
sudo openplayx-home check            # validate the saved configuration with the built-in sing-box/frpc
sudo ./openplayx-home update         # install a newly built binary; restores the old one if startup fails
sudo openplayx-home rollback         # switch back to the previously installed binary
sudo openplayx-home uninstall        # remove the systemd service, keep configuration and releases
```

Upgrading upstream means changing `src/go.mod`, rebuilding and reviewing; the
program never looks for or downloads new versions itself. To roll back
configuration, use **restore previous configuration** on the web page and apply.

Run in the foreground or generate configuration without installing a service:

```bash
./openplayx-home serve  --config-dir /path/to/state
./openplayx-home render --config-dir /path/to/state --output generated   # output contains credentials
```

Topology, troubleshooting and the limits of what the tests prove:
[docs/operations.md](docs/operations.md).

## Contributing

Issues and pull requests are welcome, see [CONTRIBUTING.md](CONTRIBUTING.md).
Report security problems privately as described in [SECURITY.md](SECURITY.md).

## Disclaimer

This project is for reaching your own services at home. Map only services you
are entitled to access, and follow the laws on networking and encrypted
communication where you live. Availability on any network, including
cross-border links, is not guaranteed.

## Upstream and license

sing-box is licensed under GPL-3.0-or-later (with additional naming
restrictions). This program links sing-box statically, so the source in this
repository and the distributed binaries are provided as a whole under
**GPL-3.0-or-later**; the full text is in [COPYING](COPYING). frp is
Apache-2.0. When distributing binaries, provide the corresponding source.
Upstream sources: [sing-box v1.12.12](https://github.com/SagerNet/sing-box/tree/v1.12.12),
[frp v0.65.0](https://github.com/fatedier/frp/tree/v0.65.0).
