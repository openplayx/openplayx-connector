package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func platformFixture() (obj, obj, []platformTarget) {
	node := obj{"nodeId": "node_sydney", "name": "syd1", "host": "relay.example.com", "tlsServerName": "relay.example.com", "port": 443, "frpsControlPort": 7000}
	target := platformTarget{ID: "local-key", ServiceID: "service_a", Name: "Jellyfin", Type: "jellyfin", Host: "127.0.0.1", Port: 18096}
	profile := obj{"schemaVersion": 1, "revision": "1", "connectorId": "connector_a", "userId": "user_a", "credential": obj{"type": "vless", "uuid": "11111111-1111-4111-8111-111111111111"}, "frpCredential": obj{"kind": "connector_frp_auth", "value": strings.Repeat("secret", 8)}, "nodes": []any{node}, "services": []any{obj{"serviceId": "service_a", "connectorId": "connector_a", "remotePort": 18000}}}
	return profile, node, []platformTarget{target}
}
func TestPlatformPlanUsesVerifiedTLSAndOnlyOwnedServices(t *testing.T) {
	profile, node, targets := platformFixture()
	plan, base, hash, err := platformPlan(profile, node, targets)
	if err != nil {
		t.Fatal(err)
	}
	common, proxies, err := frpClientConfig(plan.FRP)
	if err != nil || len(proxies) != 1 {
		t.Fatal(err, len(proxies))
	}
	if common.Transport.TLS.TrustedCaFile == "" || common.Transport.TLS.ServerName != "relay.example.com" {
		t.Fatal("TLS verification missing")
	}
	if common.Transport.ProxyURL != "socks5://127.0.0.1:1082" {
		t.Fatal("direct transport enabled")
	}
	if strings.Contains(string(plan.FRP), "InsecureSkipVerify") {
		t.Fatal("unverified TLS")
	}
	targets[0].Port = 18097
	_, nextBase, nextHash, err := platformPlan(profile, node, targets)
	if err != nil || nextBase != base || nextHash == hash {
		t.Fatal("service update restarted base transport", err)
	}
	targets[0].ServiceID = "other-account-service"
	plan, _, _, err = platformPlan(profile, node, targets)
	if err != nil {
		t.Fatal(err)
	}
	_, proxies, _ = frpClientConfig(plan.FRP)
	if len(proxies) != 0 {
		t.Fatal("cross-owner mapping")
	}
	targets[0].ServiceID = "service_a"
	targets[0].Host = "0.0.0.0"
	if _, _, _, err = platformPlan(profile, node, targets); err == nil {
		t.Fatal("unspecified destination accepted")
	}
}
func TestPlatformEnrollmentPersistenceIdempotentRegistrationAndSecretProjection(t *testing.T) {
	store, _ := NewStore(t.TempDir(), roleConnector)
	p, _ := newConnectorPlatform(store, NewRuntime(roleConnector))
	var creates int
	keys := map[string]bool{}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/relay/connector/bindings" {
			json.NewEncoder(w).Encode(obj{"proof": "private-proof", "userCode": "ABCDEFGHJK"})
			return
		}
		if r.URL.Path == "/v1/relay/connector/bindings/claim" {
			json.NewEncoder(w).Encode(obj{"connector": obj{"connectorId": "connector_a", "userId": "user_a"}, "machineToken": "private-machine-token"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer private-machine-token" {
			t.Error("incorrect machine identity")
		}
		if r.URL.Path == "/v1/relay/connector/services" {
			creates++
			key := r.Header.Get("Idempotency-Key")
			if key == "" {
				t.Error("missing idempotency")
			}
			keys[key] = true
			json.NewEncoder(w).Encode(obj{"service": obj{"serviceId": "service_a"}})
			return
		}
		profile, _, _ := platformFixture()
		profile["nodes"] = []any{}
		json.NewEncoder(w).Encode(profile)
	}))
	defer mock.Close()
	p.origin = mock.URL
	p.client = mock.Client()
	if err := p.begin(context.Background(), "VM"); err != nil {
		t.Fatal(err)
	}
	if err := p.sync(context.Background()); err == nil {
		t.Fatal("no available node must be reported")
	}
	if err := p.add("Jellyfin", "jellyfin", "127.0.0.1", 18096); err != nil {
		t.Fatal(err)
	}
	_ = p.sync(context.Background())
	_ = p.sync(context.Background())
	if creates != 1 || len(keys) != 1 {
		t.Fatal("duplicate registration")
	}
	state, err := newConnectorPlatform(store, NewRuntime(roleConnector))
	if err != nil || state.state.Targets[0].ServiceID != "service_a" {
		t.Fatal("state lost", err)
	}
	text, _ := json.Marshal(p.view())
	for _, secret := range []string{"private-machine-token", "private-proof"} {
		if strings.Contains(string(text), secret) {
			t.Fatal("public secret leak")
		}
	}
	info, _ := os.Stat(store.file("platform.json"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("insecure state permissions")
	}
	if p.add("bad", "smb", "8.8.8.8", 445) == nil || p.add("bad", "smb", "127.0.0.1", 9080) == nil {
		t.Fatal("unsafe service accepted")
	}
}
func TestPlatformClientRefusesRedirectsWithoutLeakingToken(t *testing.T) {
	called := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(302)
		io.WriteString(w, `{}`)
	}))
	defer origin.Close()
	s, _ := NewStore(t.TempDir(), roleConnector)
	p, _ := newConnectorPlatform(s, NewRuntime(roleConnector))
	p.origin = origin.URL
	if _, _, err := p.request(context.Background(), "/connector/config", "private-machine-token", nil, ""); err == nil || called {
		t.Fatal("redirect followed")
	}
}

func TestPlatformManagerUsesExistingAuthCSRFAndKeepsStatePrivate(t *testing.T) {
	s, _ := NewStore(t.TempDir(), roleConnector)
	management := s.blank()["management"].(obj)
	server, err := NewManagerServer(s, management, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := newConnectorPlatform(s, NewRuntime(roleConnector))
	p.state.MachineToken = "private-machine"
	p.state.Proof = "private-proof"
	server.platform = p
	go server.Serve()
	defer server.Close()
	authority := server.listener.Addr().String()
	c := &client{t: t, base: "http://" + authority, host: authority, origin: "http://" + authority}
	if status, _, _ := c.request("GET", "/api/platform", nil, nil); status != 401 {
		t.Fatal("anonymous access", status)
	}
	bootstrap, _ := os.ReadFile(s.file("bootstrap-token"))
	status, data, response := c.request("POST", "/api/login", obj{"password": strings.TrimSpace(string(bootstrap))}, nil)
	if status != 200 {
		t.Fatal(status)
	}
	c.cookie = strings.Split(response.Header.Get("Set-Cookie"), ";")[0]
	if status, _, _ := c.request("POST", "/api/platform/services", obj{"name": "SMB", "type": "smb", "host": "127.0.0.1", "port": 445}, nil); status != 403 {
		t.Fatal("missing CSRF allowed")
	}
	c.csrf = stringValue(data["csrfToken"])
	status, data, _ = c.request("GET", "/api/platform", nil, nil)
	if status != 200 || strings.Contains(canonical(data), "private-machine") || strings.Contains(canonical(data), "private-proof") {
		t.Fatal("secret exposure")
	}
	if status, _, _ := c.request("POST", "/api/save", obj{}, nil); status != 409 {
		t.Fatal("manual runtime mutation allowed")
	}
	if status, _, _ := c.request("POST", "/api/platform/services", obj{"name": "SMB", "type": "smb", "host": "127.0.0.1", "port": 445}, nil); status != 200 {
		t.Fatal("service rejected", status)
	}
}
