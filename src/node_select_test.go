package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func nodeSelectFixture(t *testing.T, rtts map[string]time.Duration) *connectorPlatform {
	t.Helper()
	store, _ := NewStore(t.TempDir(), roleConnector)
	p, _ := newConnectorPlatform(store, NewRuntime(roleConnector))
	p.probe = func(_ context.Context, host string, _ int) (time.Duration, error) {
		if rtt, ok := rtts[host]; ok {
			return rtt, nil
		}
		return 0, errors.New("unreachable")
	}
	return p
}

func nodesProfile(ids ...string) obj {
	nodes := []any{}
	for _, id := range ids {
		nodes = append(nodes, obj{"nodeId": id, "host": id + ".example.com", "port": 443})
	}
	return obj{"nodes": nodes}
}

func TestEnsureNodePicksLowestLatencyAndStaysSticky(t *testing.T) {
	rtts := map[string]time.Duration{"a.example.com": 80 * time.Millisecond, "b.example.com": 20 * time.Millisecond}
	p := nodeSelectFixture(t, rtts)
	if changed, err := p.ensureNode(context.Background(), nodesProfile("a", "b")); err != nil || !changed || p.state.NodeID != "b" {
		t.Fatal("first selection must use the lowest latency", changed, err, p.state.NodeID)
	}
	saved, _ := newConnectorPlatform(p.store, p.runtime)
	if saved.state.NodeID != "b" {
		t.Fatal("selection not persisted")
	}
	// A faster node later must not tear down a healthy tunnel.
	rtts["a.example.com"] = time.Millisecond
	p.lastProbe = time.Time{}
	if changed, _ := p.ensureNode(context.Background(), nodesProfile("a", "b")); changed || p.state.NodeID != "b" {
		t.Fatal("healthy node switched", p.state.NodeID)
	}
}

func TestEnsureNodeSwitchesWhenCurrentNodeIsRemoved(t *testing.T) {
	p := nodeSelectFixture(t, map[string]time.Duration{"old.example.com": time.Millisecond, "new.example.com": 30 * time.Millisecond})
	p.state.NodeID = "old"
	p.lastProbe = time.Now()
	if changed, _ := p.ensureNode(context.Background(), nodesProfile("new")); !changed || p.state.NodeID != "new" {
		t.Fatal("removed node kept", p.state.NodeID)
	}
}

func TestEnsureNodeSwitchesOnlyAfterRepeatedFailures(t *testing.T) {
	rtts := map[string]time.Duration{"b.example.com": 50 * time.Millisecond, "c.example.com": 40 * time.Millisecond}
	p := nodeSelectFixture(t, rtts)
	p.state.NodeID = "a" // listed but unreachable
	for i := 1; i < nodeFailureLimit; i++ {
		p.lastProbe = time.Time{}
		if changed, _ := p.ensureNode(context.Background(), nodesProfile("a", "b", "c")); changed {
			t.Fatal("switched after", i, "failures")
		}
	}
	p.lastProbe = time.Time{}
	if changed, _ := p.ensureNode(context.Background(), nodesProfile("a", "b", "c")); !changed || p.state.NodeID != "c" {
		t.Fatal("failed node kept", p.state.NodeID)
	}
}

func TestEnsureNodeWithNothingReachable(t *testing.T) {
	p := nodeSelectFixture(t, map[string]time.Duration{})
	p.state.NodeID = "a"
	for i := 0; i <= nodeFailureLimit; i++ {
		p.lastProbe = time.Time{}
		_, _ = p.ensureNode(context.Background(), nodesProfile("a", "b"))
	}
	if p.state.NodeID != "a" {
		t.Fatal("listed node dropped for another unreachable node")
	}
	if changed, _ := p.ensureNode(context.Background(), nodesProfile("x", "y")); !changed || p.state.NodeID != "x" {
		t.Fatal("unlisted node kept with nothing reachable", p.state.NodeID)
	}
}

func TestReportNodeTellsPlatformAndRetries(t *testing.T) {
	var reports []string
	fail := true
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/relay/connector/node" || r.Method != "PUT" || r.Header.Get("Authorization") != "Bearer machine" {
			t.Error("unexpected report", r.Method, r.URL.Path)
		}
		var body obj
		_ = json.NewDecoder(r.Body).Decode(&body)
		reports = append(reports, stringValue(body["nodeId"]))
		if fail {
			w.WriteHeader(503)
			json.NewEncoder(w).Encode(obj{"error": obj{"code": "unavailable"}})
			return
		}
		json.NewEncoder(w).Encode(obj{"nodeId": body["nodeId"]})
	}))
	defer mock.Close()
	p := nodeSelectFixture(t, nil)
	p.origin, p.client = mock.URL, mock.Client()
	p.state.MachineToken, p.state.NodeID = "machine", "b"
	p.reportNode(context.Background())
	if len(reports) != 0 {
		t.Fatal("reported before the tunnel was applied")
	}
	p.planHash = "applied"
	p.reportNode(context.Background())
	if p.reportedNode != "" || p.reportFault == "" {
		t.Fatal("failed report recorded as done")
	}
	fail = false
	p.reportNode(context.Background())
	p.reportNode(context.Background())
	if len(reports) != 2 || p.reportedNode != "b" {
		t.Fatal("report not retried once", reports)
	}
	p.state.NodeID = "c"
	p.reportNode(context.Background())
	if reports[len(reports)-1] != "c" {
		t.Fatal("switch not reported", reports)
	}
}
