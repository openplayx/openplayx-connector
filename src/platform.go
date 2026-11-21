package main

// Platform enrollment keeps account passwords in the first-party HTTPS page.
// Only a one-use binding proof and the resulting scoped machine token live here.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type platformRegistration struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Status string `json:"status"`
}
type platformTarget struct {
	Registration *platformRegistration `json:"registration,omitempty"`
	ID           string                `json:"id"`
	Disabled     bool                  `json:"disabled,omitempty"`
	Deleted      bool                  `json:"deleted,omitempty"`
	Synced       bool                  `json:"synced"`
	Generation   uint64                `json:"generation,omitempty"`
	ServiceID    string                `json:"serviceId,omitempty"`
	Name         string                `json:"name"`
	Type         string                `json:"type"`
	Host         string                `json:"host"`
	Port         int                   `json:"port"`
}
type platformState struct {
	HighestRevision string           `json:"highestRevision,omitempty"`
	Schema          int              `json:"schemaVersion"`
	MachineToken    string           `json:"machineToken,omitempty"`
	ConnectorID     string           `json:"connectorId,omitempty"`
	UserID          string           `json:"userId,omitempty"`
	Proof           string           `json:"proof,omitempty"`
	Code            string           `json:"userCode,omitempty"`
	Enrollment      string           `json:"enrollmentName,omitempty"`
	ExpiresAt       time.Time        `json:"expiresAt,omitempty"`
	NodeID          string           `json:"nodeId,omitempty"`
	Targets         []platformTarget `json:"targets"`
}
type connectorPlatform struct {
	mu       sync.Mutex
	store    *Store
	runtime  *Runtime
	state    platformState
	profile  obj
	client   *http.Client
	origin   string
	baseHash string
	planHash string
	fault    string
	lastSync time.Time
	// Node selection (node_select.go).
	probe        func(context.Context, string, int) (time.Duration, error)
	health       map[string]*nodeHealth
	lastProbe    time.Time
	reportedNode string
	reportedAt   time.Time
	reportFault  string
}

const platformOrigin = "https://api.openplayx.com"

func newConnectorPlatform(s *Store, r *Runtime) (*connectorPlatform, error) {
	p := &connectorPlatform{store: s, runtime: r, origin: platformOrigin, client: &http.Client{Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, probe: probeTCP}
	p.state.Schema = 1
	if exists(s.file("platform.json")) {
		raw, err := readJSON(s.file("platform.json"))
		if err != nil {
			return nil, errors.New("platform state cannot be read")
		}
		data, _ := json.Marshal(raw)
		if json.Unmarshal(data, &p.state) != nil || p.state.Schema != 1 {
			return nil, errors.New("unsupported platform state")
		}
	}
	p.pickupEnrollment()
	return p, nil
}
func (p *connectorPlatform) save() error { return atomicWrite(p.store.file("platform.json"), p.state) }
func (p *connectorPlatform) request(ctx context.Context, path, token string, body any, key string) (obj, int, error) {
	method := "GET"
	if body != nil {
		method = "POST"
	}
	return p.requestAs(ctx, method, path, token, body, key, "")
}
func (p *connectorPlatform) requestAs(ctx context.Context, method, path, token string, body any, key, revision string) (obj, int, error) {
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.origin+"/v1/relay"+path, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if revision != "" {
		req.Header.Set("If-Match", "\""+revision+"\"")
	}
	req.Header.Set("User-Agent", "OpenPlayX-Connector/"+version)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	response, err := p.client.Do(req)
	if err != nil {
		return nil, 0, errors.New("platform_unavailable")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil || len(raw) > 1024*1024 {
		return nil, response.StatusCode, errors.New("platform_invalid_response")
	}
	value, err := decodeJSON(raw)
	result, ok := value.(obj)
	if err != nil || !ok {
		return nil, response.StatusCode, errors.New("platform_invalid_response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		code := "platform_request_failed"
		if detail, ok := result["error"].(map[string]any); ok {
			v := stringValue(detail["code"])
			if len(v) < 80 && !strings.ContainsAny(v, "\n\r <>\"'") {
				code = v
			}
		}
		return nil, response.StatusCode, errors.New(code)
	}
	return result, response.StatusCode, nil
}
func (p *connectorPlatform) begin(ctx context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state.MachineToken != "" {
		return errors.New("connector_already_bound")
	}
	if p.state.Proof != "" && time.Now().Before(p.state.ExpiresAt) {
		return nil
	}
	if len(strings.TrimSpace(name)) < 1 || len(name) > 128 {
		return errors.New("invalid_name")
	}
	result, _, err := p.request(ctx, "/connector/bindings", "", obj{"name": name}, "")
	if err != nil {
		return err
	}
	p.state.Proof = stringValue(result["proof"])
	p.state.Code = stringValue(result["userCode"])
	p.state.ExpiresAt = time.Now().Add(10 * time.Minute)
	return p.save()
}
func platformHost(value string) bool {
	if value == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(value)
	return err == nil && (ip.IsPrivate() || ip.IsLoopback()) && !ip.IsUnspecified() && !ip.IsMulticast()
}
func validPlatformTarget(name, kind, host string, port int) error {
	allowed := map[string]bool{"smb": true, "webdav": true, "jellyfin": true, "emby": true, "other_tcp": true}
	if name == "" || len(name) > 128 || !allowed[kind] || !platformHost(host) || port < 1 || port > 65535 {
		return errors.New("invalid_service")
	}
	if port == 9080 || port == 9082 || port == 1080 || port == 1082 || port == 22 {
		return errors.New("management_port_denied")
	}
	return nil
}
func (p *connectorPlatform) add(name, kind, host string, port int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state.MachineToken == "" {
		return errors.New("connector_not_bound")
	}
	if err := validPlatformTarget(name, kind, host, port); err != nil {
		return err
	}
	if len(p.state.Targets) >= 32 {
		return errors.New("service_limit")
	}
	for _, t := range p.state.Targets {
		if !t.Deleted && t.Name == name {
			return errors.New("duplicate_service_name")
		}
	}
	p.state.Targets = append(p.state.Targets, platformTarget{ID: tokenURLSafe(16), Name: name, Type: kind, Host: host, Port: port})
	return p.save()
}

// Local configuration is authoritative; deletion tombstones survive outages.
func (p *connectorPlatform) change(id, name, kind, host string, port int, disabled, remove bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state.MachineToken == "" {
		return errors.New("connector_not_bound")
	}
	index := -1
	for i, target := range p.state.Targets {
		if target.ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return errors.New("service_not_found")
	}
	target := p.state.Targets[index]
	if remove {
		target.Deleted = true
	} else {
		if target.Deleted {
			return errors.New("service_deleting")
		}
		if err := validPlatformTarget(name, kind, host, port); err != nil {
			return err
		}
		for i, other := range p.state.Targets {
			if i != index && !other.Deleted && other.Name == name {
				return errors.New("duplicate_service_name")
			}
		}
		target.Name = name
		target.Type = kind
		target.Host = host
		target.Port = port
		target.Disabled = disabled
	}
	target.Synced = false
	p.state.Targets[index] = target
	if err := p.save(); err != nil {
		return err
	}
	if p.profile != nil {
		if err := p.applyProfile(p.profile); err != nil {
			// Never retain a removed mapping if a local dynamic update fails.
			_ = p.runtime.Stop()
			p.baseHash = ""
			p.planHash = ""
			p.fault = "service_update_failed"
		}
	}
	return nil
}
func (p *connectorPlatform) synchronizeTargets(ctx context.Context) error {
	for index, target := range p.state.Targets {
		if target.ServiceID != "" {
			continue
		}
		key := target.ID
		if target.Generation > 0 {
			key = fmt.Sprintf("%s-%d", key, target.Generation)
		}
		if target.Registration == nil {
			status := "enabled"
			if target.Disabled || target.Deleted {
				status = "disabled"
			}
			target.Registration = &platformRegistration{Name: target.Name, Type: target.Type, Status: status}
			p.state.Targets[index] = target
			if err := p.save(); err != nil {
				return err
			}
		}
		result, _, err := p.request(ctx, "/connector/services", p.state.MachineToken, target.Registration, key)
		if err != nil {
			return err
		}
		service, _ := result["service"].(map[string]any)
		target.ServiceID = stringValue(service["serviceId"])
		if target.ServiceID == "" {
			return errors.New("invalid_service_identity")
		}
		target.Registration = nil
		p.state.Targets[index] = target
		if err = p.save(); err != nil {
			return err
		}
	}
	catalog, _, err := p.request(ctx, "/connector/services?limit=100", p.state.MachineToken, nil, "")
	if err != nil {
		return err
	}
	if catalog["nextCursor"] != nil && catalog["nextCursor"] != "" {
		return errors.New("service_catalog_incomplete")
	}
	items, ok := catalog["items"].([]any)
	if !ok {
		return errors.New("invalid_service_catalog")
	}
	revision := stringValue(catalog["revision"])
	if revision == "" {
		return errors.New("invalid_profile_revision")
	}
	remote := map[string]obj{}
	for _, item := range items {
		service, ok := item.(map[string]any)
		if !ok || service["connectorId"] != p.state.ConnectorID {
			return errors.New("profile_identity_mismatch")
		}
		remote[stringValue(service["serviceId"])] = service
	}
	for index := 0; index < len(p.state.Targets); {
		target := p.state.Targets[index]
		service, found := remote[target.ServiceID]
		if target.Deleted {
			if found && service["status"] != "deleting" && service["status"] != "deleted" {
				result, _, err := p.requestAs(ctx, "DELETE", "/connector/services/"+url.PathEscape(target.ServiceID), p.state.MachineToken, obj{}, "", revision)
				if err != nil {
					return err
				}
				revision = stringValue(result["revision"])
			}
			delete(remote, target.ServiceID)
			p.state.Targets = append(p.state.Targets[:index], p.state.Targets[index+1:]...)
			if err := p.save(); err != nil {
				return err
			}
			continue
		}
		if !found {
			// Persist a fresh generation instead of replaying a retired registration.
			target.ServiceID = ""
			target.Generation++
			target.Synced = false
		} else if service["status"] == "deleting" {
			target.Synced = false
		} else {
			status := "enabled"
			if target.Disabled {
				status = "disabled"
			}
			if service["name"] != target.Name || service["type"] != target.Type || service["status"] != status {
				result, _, err := p.requestAs(ctx, "PATCH", "/connector/services/"+url.PathEscape(target.ServiceID), p.state.MachineToken, obj{"name": target.Name, "type": target.Type, "status": status}, "", revision)
				if err != nil {
					return err
				}
				revision = stringValue(result["revision"])
			}
			target.Synced = true
		}
		p.state.Targets[index] = target
		if err := p.save(); err != nil {
			return err
		}
		index++
	}
	// A complete catalog may contain stale registrations left by an older
	// local configuration. Only this Connector's records are reconciled.
	desired := map[string]bool{}
	for _, target := range p.state.Targets {
		desired[target.ServiceID] = true
	}
	for id, service := range remote {
		if desired[id] || service["status"] == "deleting" || service["status"] == "deleted" {
			continue
		}
		result, _, err := p.requestAs(ctx, "DELETE", "/connector/services/"+url.PathEscape(id), p.state.MachineToken, obj{}, "", revision)
		if err != nil {
			return err
		}
		revision = stringValue(result["revision"])
	}
	return nil
}
func (p *connectorPlatform) view() obj {
	p.mu.Lock()
	defer p.mu.Unlock()
	nodes := []any{}
	if p.profile != nil {
		nodes, _ = p.profile["nodes"].([]any)
	}
	result := obj{"bound": p.state.MachineToken != "", "connectorId": p.state.ConnectorID, "nodeId": p.state.NodeID, "nodes": nodes, "services": p.state.Targets, "error": p.fault, "components": p.runtime.Components(), "version": version, "lastSync": p.lastSync, "nodeHealth": p.nodeLatencies(), "reportedNodeId": p.reportedNode, "reportError": p.reportFault}
	if p.state.Proof != "" && time.Now().Before(p.state.ExpiresAt) {
		result["userCode"] = p.state.Code
		if p.state.Code != "" {
			result["approvalURL"] = "https://openplayx.com/account/connector#code=" + p.state.Code
		}
		if p.state.Enrollment != "" {
			result["enrollmentName"] = p.state.Enrollment
		}
	}
	return result
}
func (p *connectorPlatform) sync(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state.MachineToken == "" {
		if p.state.Proof == "" || time.Now().After(p.state.ExpiresAt) {
			return nil
		}
		result, status, err := p.request(ctx, "/connector/bindings/claim", "", obj{"proof": p.state.Proof}, "")
		if err != nil {
			if err.Error() == "invalid_challenge" {
				// Expired, cancelled or already used: allow a fresh binding.
				p.state.Proof, p.state.Code, p.state.Enrollment = "", "", ""
				_ = p.save()
			}
			return err
		}
		if status == 202 {
			return nil
		}
		connector, _ := result["connector"].(map[string]any)
		p.state.MachineToken = stringValue(result["machineToken"])
		p.state.ConnectorID = stringValue(connector["connectorId"])
		p.state.UserID = stringValue(connector["userId"])
		if p.state.MachineToken == "" || p.state.ConnectorID == "" {
			return errors.New("invalid_connector_identity")
		}
		p.state.Proof = ""
		p.state.Code = ""
		p.state.Enrollment = ""
		if err = p.save(); err != nil {
			return err
		}
	}
	if err := p.synchronizeTargets(ctx); err != nil {
		if err.Error() == "unauthorized" {
			_ = p.runtime.Stop()
			p.planHash = ""
			p.baseHash = ""
		}
		return err
	}
	profile, status, err := p.request(ctx, "/connector/config", p.state.MachineToken, nil, "")
	if err != nil {
		if status == 401 || status == 403 {
			_ = p.runtime.Stop()
			p.planHash = ""
			p.baseHash = ""
		}
		return err
	}
	if profile["connectorId"] != p.state.ConnectorID || profile["userId"] != p.state.UserID {
		return errors.New("profile_identity_mismatch")
	}
	if version, ok := intValue(profile["schemaVersion"]); !ok || version != 1 {
		return errors.New("unsupported_profile")
	}
	revision, err := strconv.ParseUint(stringValue(profile["revision"]), 10, 64)
	if err != nil {
		return errors.New("invalid_profile_revision")
	}
	highest, _ := strconv.ParseUint(p.state.HighestRevision, 10, 64)
	if revision < highest {
		return errors.New("profile_revision_rollback")
	}
	if p.state.HighestRevision != stringValue(profile["revision"]) {
		p.state.HighestRevision = stringValue(profile["revision"])
		if err = p.save(); err != nil {
			return err
		}
	}
	p.lastSync = time.Now()
	if _, err = p.ensureNode(ctx, profile); err != nil {
		return err
	}
	if err = p.applyProfile(profile); err != nil {
		return err
	}
	p.reportNode(ctx)
	return nil
}

// Caller holds p.mu. Service-only changes reuse the existing transport.
func (p *connectorPlatform) applyProfile(profile obj) error {
	p.profile = profile
	nodes, _ := profile["nodes"].([]any)
	// sync() chooses the node (ensureNode) before applying the profile.
	var node obj
	for _, item := range nodes {
		n, _ := item.(map[string]any)
		if n["nodeId"] == p.state.NodeID {
			node = n
		}
	}
	if node == nil {
		_ = p.runtime.Stop()
		p.planHash = ""
		p.baseHash = ""
		return errors.New("selected_node_unavailable")
	}
	plan, baseHash, hash, err := platformPlan(profile, node, p.state.Targets)
	if err != nil {
		return err
	}
	if p.planHash == hash {
		return nil
	}
	if p.baseHash == baseHash && p.planHash != "" {
		_, proxies, err := frpClientConfig(plan.FRP)
		if err != nil {
			return err
		}
		p.runtime.mu.Lock()
		client := p.runtime.frpc
		if client != nil {
			err = client.UpdateAllConfigurer(proxies, nil)
		} else {
			err = errors.New("client_unavailable")
		}
		p.runtime.mu.Unlock()
		if err != nil {
			return errors.New("service_update_failed")
		}
	} else {
		if err = p.runtime.Restart(nil, plan); err != nil {
			return errors.New("connector_start_failed")
		}
	}
	p.baseHash = baseHash
	p.planHash = hash
	return nil
}

// frp skips certificate verification when trustedCaFile is empty. Always pass
// an OS root bundle explicitly and fail closed when none is available.
func platformCAFile() string {
	for _, path := range []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/ssl/cert.pem", "/etc/pki/tls/certs/ca-bundle.crt", "/usr/local/share/certs/ca-root-nss.crt"} {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return "/nonexistent/openplayx-required-ca-bundle"
}
func platformHash(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func platformPlan(profile, node obj, targets []platformTarget) (*Plan, string, string, error) {
	credential, _ := profile["credential"].(map[string]any)
	frpCredential, _ := profile["frpCredential"].(map[string]any)
	uuid := stringValue(credential["uuid"])
	auth := stringValue(frpCredential["value"])
	connector := stringValue(profile["connectorId"])
	if !parseUUID(uuid) || len(auth) < 32 || connector == "" {
		return nil, "", "", errors.New("invalid_platform_credentials")
	}
	if _, err := host(node["host"]); err != nil {
		return nil, "", "", err
	}
	if _, err := host(node["tlsServerName"]); err != nil {
		return nil, "", "", err
	}
	number, err := port(node["port"])
	if err != nil {
		return nil, "", "", err
	}
	control, err := port(node["frpsControlPort"])
	if err != nil {
		return nil, "", "", err
	}
	sing := obj{"log": obj{"disabled": true}, "inbounds": []any{obj{"type": "socks", "tag": "socks", "listen": "127.0.0.1", "listen_port": 1082}}, "outbounds": []any{obj{"type": "vless", "tag": "relay", "server": node["host"], "server_port": number, "uuid": uuid, "tls": obj{"enabled": true, "server_name": node["tlsServerName"]}}}, "route": obj{"final": "relay"}}
	frp := obj{"serverAddr": "127.0.0.1", "serverPort": control, "user": connector, "loginFailExit": false, "auth": obj{"method": "token", "token": auth, "additionalScopes": []any{"HeartBeats", "NewWorkConns"}}, "transport": obj{"protocol": "tcp", "proxyURL": "socks5://127.0.0.1:1082", "tls": obj{"enable": true, "serverName": node["tlsServerName"], "trustedCaFile": platformCAFile()}}, "log": obj{"to": "console", "level": "error"}}
	base := platformHash(obj{"sing": sing, "frp": frp})
	proxies := []any{}
	services, _ := profile["services"].([]any)
	for _, target := range targets {
		if target.Deleted || target.Disabled {
			continue
		}
		for _, item := range services {
			service, _ := item.(map[string]any)
			if service["serviceId"] != target.ServiceID || service["connectorId"] != connector {
				continue
			}
			remote, err := port(service["remotePort"])
			if err != nil || !platformHost(target.Host) || target.Port < 1 || target.Port > 65535 {
				return nil, "", "", errors.New("invalid_service_mapping")
			}
			proxies = append(proxies, obj{"name": target.ServiceID, "type": "tcp", "localIP": target.Host, "localPort": target.Port, "remotePort": remote})
		}
	}
	frp["proxies"] = proxies
	singBytes, _ := json.Marshal(sing)
	frpBytes, _ := json.Marshal(frp)
	plan := &Plan{Role: roleConnector, Revision: profile["revision"], SingBox: singBytes, FRP: frpBytes, SocksPort: 1082, Retry: 5 * time.Second, LogLevel: "error", secrets: obj{"uuid": uuid, "token": auth}}
	if err = checkPlan(plan); err != nil {
		return nil, "", "", fmt.Errorf("platform plan invalid: %w", err)
	}
	return plan, base, platformHash(obj{"sing": sing, "frp": frp}), nil
}
func (p *connectorPlatform) run(ctx context.Context) {
	for {
		err := p.sync(ctx)
		p.mu.Lock()
		p.fault = ""
		if err != nil {
			p.fault = err.Error()
		}
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}
func (m *ManagerServer) platformRequest(w http.ResponseWriter, r *http.Request, data obj) error {
	p := m.platform
	if p == nil {
		return cfgErr("notFound", "仅 Connector 支持平台连接。", 404)
	}
	switch {
	case r.RequestURI == "/api/platform/scan" && r.Method == "GET":
		m.reply(w, 200, connectorScanner.view(), nil)
		return nil
	case r.RequestURI == "/api/platform/scan" && r.Method == "POST":
		first, okFirst := intValue(data["portStart"])
		last, okLast := intValue(data["portEnd"])
		if !okFirst || !okLast {
			first, last = 1, 65535
		}
		if err := connectorScanner.start(int(first), int(last)); err != nil {
			return cfgErr("scanFailed", "局域网扫描未开始："+err.Error(), 409)
		}
		m.reply(w, 200, connectorScanner.view(), nil)
		return nil
	case r.RequestURI == "/api/platform/scan/cancel" && r.Method == "POST":
		connectorScanner.stop()
		m.reply(w, 200, connectorScanner.view(), nil)
		return nil
	}
	var err error
	switch {
	case r.Method == "GET" && r.RequestURI == "/api/platform":
	case r.Method == "POST" && r.RequestURI == "/api/platform/bind":
		err = p.begin(r.Context(), stringValue(data["name"]))
	case r.Method == "POST" && r.RequestURI == "/api/platform/services":
		number, ok := intValue(data["port"])
		if !ok {
			return errors.New("invalid_port")
		}
		err = p.add(strings.TrimSpace(stringValue(data["name"])), stringValue(data["type"]), strings.TrimSpace(stringValue(data["host"])), int(number))
	case r.Method == "POST" && r.RequestURI == "/api/platform/services/update":
		number, ok := intValue(data["port"])
		if !ok {
			return errors.New("invalid_port")
		}
		disabled, _ := data["disabled"].(bool)
		err = p.change(stringValue(data["id"]), strings.TrimSpace(stringValue(data["name"])), stringValue(data["type"]), strings.TrimSpace(stringValue(data["host"])), int(number), disabled, false)
	case r.Method == "POST" && r.RequestURI == "/api/platform/services/remove":
		err = p.change(stringValue(data["id"]), "", "", "", 0, false, true)
	default:
		return cfgErr("notFound", "接口不存在。", 404)
	}
	if err != nil {
		return cfgErr("platformFailed", "平台操作未完成："+err.Error(), 400)
	}
	m.reply(w, 200, p.view(), nil)
	return nil
}
