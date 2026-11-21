# OpenPlayX Connector

Repository: [openplayx/openplayx-connector](https://github.com/openplayx/openplayx-connector). Jenkins releases use `vMAJOR.MINOR.PATCH` tags starting at `v0.1.0`, automatically incrementing PATCH after checks and packaging pass.

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
`openplayx-connector serve` in the foreground.

## Build

Requires Go 1.24+. The Go sources, module files and the embedded web page
(`src/web/`) are under `src/`; apart from `src/role.go`, `src/` is identical in
the Relay and Connector repositories.

The pages use the shared OpenPlayX UI kit in `src/web/ui/` (Basecoat plus the
OpenPlayX theme). It is a synced copy of `ui/` in the `openplayx/web`
repository; edit it there, not here. `go test` fails if the copy is modified.

```bash
scripts/build.sh                       # dist/openplayx-connector-linux-{amd64,arm64} + SHA256SUMS
(cd src && go test ./...)             # unit tests + in-process end-to-end tests
```

## Platform-managed Relay nodes

For API-managed nodes such as `syd1`, install the systemd service, then set
`OPENPLAYX_CONNECTOR_PLATFORM=1` in its systemd environment and restart it.
Alternatively run `serve --platform --config-dir <directory>`.
The LAN manager provides one-time enrollment and explicit local service mappings.
"Scan LAN services" runs in the background (about 1–3 minutes for ports 1–65535): it finds live hosts
(NetBIOS, ARP, a short TCP sweep) and self-announcing services (SSDP/DLNA, Plex GDM, Jellyfin/Emby UDP 7359,
mDNS), connect-scans the chosen TCP range on loopback and the attached private /24 networks, sweeps the same
UDP range for QUIC, and fingerprints every open port: SMB, NFS, FTP/FTPES, implicit FTPS, SSH/SFTP, HTTP,
HTTPS, HTTP/3, WebDAV, DLNA, Jellyfin, Emby and Plex. "Use" fills the service form; FTP, DLNA and UDP results
are shown but cannot be mapped. Nothing is registered until you save.
The Relay node is chosen automatically: the Connector measures TCP connect time to
every enabled node and uses the fastest reachable one. It stays on that node while it
remains listed and reachable, and switches when the node is removed or fails three
probes in a row (checked every 30 s). It reports its current node to the platform,
and devices follow that node; there is no manual node selection. Approve enrollment with the same phone account at
`https://openplayx.com/account/connector`; view synchronized services at
`https://openplayx.com/account/`. All signed-in devices of the same account can access its enabled services without per-device grants.
Account passwords stay on the first-party HTTPS origin; the Connector stores only
its scoped machine identity and local targets in a private 0600 state file.
See [the platform installation steps](README.zh-CN.md#平台模式api-管理的-relay).

### Pre-registration from the account page

Instead of a pairing code, you can pre-register a Connector at
`https://openplayx.com/account/connectors` ("Pre-register Connector"): choose a
name and platform, then download the matching package and the one-time
enrollment file (`openplayx-connector-enrollment.json`, valid for 7 days, shown
only once). Install and start, and it connects without further steps:

```sh
# Linux with systemd (writes the platform drop-in and imports the file)
tar xzf openplayx-connector-<version>-linux-<arch>-<sha>.tar.gz
sudo ./openplayx-connector-<version>-linux-<arch>/openplayx-connector install --enroll openplayx-connector-enrollment.json
# macOS / FreeBSD / others, foreground
./openplayx-connector serve --enroll openplayx-connector-enrollment.json --config-dir "$HOME/.openplayx-connector"
```

`--enroll` implies platform mode. The file is validated (issuing API origin,
expiry, token format), copied into the private state directory, imported into
`platform.json` and deleted; the token is then used once as the binding proof.
An unclaimed pre-registration reserves Connector quota until it is claimed,
expires or is cancelled on the account page. A cancelled or expired file is
cleared locally so a pairing code can be used instead.

## Quick start: install

Get the whole **connector/** directory produced by `openplayx-relay pair` from the
Relay administrator (connector.json, the private secrets.json and the managed TLS
assets). Do not accept relay/ or app.json.

Put its contents into `/var/lib/openplayx-connector/` (directory 0700, files 0600),
then:

```bash
sudo ./openplayx-connector install
# Log in at http://<home LAN IP>:9080 (first time: /var/lib/openplayx-connector/bootstrap-token),
# enter the real NAS targets in frpc.proxies, save and click Apply; or after saving, run locally:
sudo openplayx-connector apply
sudo openplayx-connector status
```

`install` creates the system account `openplayx-connector`, copies the binary to
`/opt/openplayx-connector/releases/<timestamp>/` and switches `current`, links
`/usr/local/bin/openplayx-connector`, and writes `openplayx-connector.service` (see
`deploy/openplayx-connector.service`). The service runs as that account; the admin
page can edit and save before any components are applied. `frpc.proxies` is
the service registration: each `remotePort` must be inside the Relay's
`frps.allowPorts`, otherwise frps rejects that proxy. Common mappings are in
[examples/proxies.json](examples/proxies.json) (the `frpc.proxies` field of
`connector.json`).

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
`connector.json`; the real UUIDs and tokens are only in the private `secrets.json`.
Structure, startup, permissions and verification are described in
[docs/configuration.md](docs/configuration.md).

## Web configuration

The page has two parts, **transport link** and **available services**. It shows
the saved configuration; expand a section to edit it. The Connector manages the
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

An explicit loopback listener in an existing `connector.json` is not silently
replaced by an upgrade. To enable LAN access:

```bash
sudo ./openplayx-connector install --enable-lan
```

This saves the existing loopback configuration as a new LAN-access revision and
restarts the service (components restart with the applied revision); media
proxying and home mappings do not change. Add `--config-dir /actual/dir` for a
custom config directory. The host firewall must allow TCP 9080 from the home
network; do not forward the port on the router.

For manual deployments without systemd, run
`openplayx-connector enable-lan --config-dir /actual/dir`, then restart the `serve`
process. With a damaged configuration the program still uses the loopback
recovery entry and never exposes an unknown configuration to the LAN.

## Maintenance

```bash
sudo systemctl stop|start|restart openplayx-connector
sudo openplayx-connector status
sudo openplayx-connector check            # validate the saved configuration with the built-in sing-box/frpc
sudo ./openplayx-connector update         # install a newly built binary; restores the old one if startup fails
sudo openplayx-connector rollback         # switch back to the previously installed binary
sudo openplayx-connector uninstall        # remove the systemd service, keep configuration and releases
```

Upgrading upstream means changing `src/go.mod`, rebuilding and reviewing; the
program never looks for or downloads new versions itself. To roll back
configuration, use **restore previous configuration** on the web page and apply.

Run in the foreground or generate configuration without installing a service:

```bash
./openplayx-connector serve  --config-dir /path/to/state
./openplayx-connector render --config-dir /path/to/state --output generated   # output contains credentials
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

Platform-mode services are managed on this Connector: add, edit name/type/local target, disable and delete in the LAN manager. Desired state is persisted privately and reconciled using the scoped machine identity; target addresses and ports never leave the machine. Frozen registration requests preserve idempotency after uncertain responses; deletion tombstones survive outages and stop local forwarding immediately. The cloud user page is read-only for service configuration.
