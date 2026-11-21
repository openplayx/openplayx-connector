package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func enrollmentJSON(api, token string, expires time.Time) []byte {
	raw, _ := json.Marshal(obj{"schemaVersion": 1, "type": enrollmentType, "api": api, "name": "Home NAS", "enrollmentToken": token, "expiresAt": expires.UTC().Format(time.RFC3339)})
	return raw
}

func TestParseEnrollmentRejectsForeignExpiredAndMalformedFiles(t *testing.T) {
	token := "opxe_" + strings.Repeat("A", 43)
	future := time.Now().Add(time.Hour)
	if _, err := parseEnrollment(enrollmentJSON(platformOrigin, token, future), platformOrigin, time.Now()); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{
		"origin":  enrollmentJSON("https://evil.example", token, future),
		"expired": enrollmentJSON(platformOrigin, token, time.Now().Add(-time.Minute)),
		"token":   enrollmentJSON(platformOrigin, "opxe_short", future),
		"type":    []byte(`{"schemaVersion":1,"type":"other","name":"x","enrollmentToken":"` + token + `","expiresAt":"2999-01-01T00:00:00Z"}`),
		"garbage": []byte("not json"),
	} {
		if _, err := parseEnrollment(raw, platformOrigin, time.Now()); err == nil {
			t.Fatal("accepted", name)
		}
	}
}

func TestEnrollmentFileIsImportedRemovedAndClaimedWithoutPairingCode(t *testing.T) {
	token := "opxe_" + strings.Repeat("b", 43)
	claims := 0
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/relay/connector/bindings" {
			t.Error("pre-registered Connector requested a pairing code")
		}
		if r.URL.Path == "/v1/relay/connector/bindings/claim" {
			claims++
			var body obj
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["proof"] != token {
				t.Error("claim did not use the enrollment token")
			}
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(obj{"connector": obj{"connectorId": "connector_a", "userId": "user_a"}, "machineToken": "private-machine-token"})
			return
		}
		if r.URL.Path == "/v1/relay/connector/services" {
			json.NewEncoder(w).Encode(obj{"items": []any{}, "revision": "1"})
			return
		}
		profile, _, _ := platformFixture()
		profile["nodes"] = []any{}
		json.NewEncoder(w).Encode(profile)
	}))
	defer mock.Close()
	store, _ := NewStore(t.TempDir(), roleConnector)
	source := store.file("download.json")
	if err := os.WriteFile(source, enrollmentJSON("", token, time.Now().Add(time.Hour)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stageEnrollment(source, store); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(store.file(enrollmentFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("staged enrollment missing or not private", err)
	}
	p, err := newConnectorPlatform(store, NewRuntime(roleConnector))
	if err != nil {
		t.Fatal(err)
	}
	if exists(store.file(enrollmentFile)) {
		t.Fatal("enrollment file kept after import")
	}
	view, _ := json.Marshal(p.view())
	if strings.Contains(string(view), token) || p.view()["enrollmentName"] != "Home NAS" || p.view()["approvalURL"] != nil {
		t.Fatal("unexpected pending view", string(view))
	}
	if p.begin(context.Background(), "other") != nil || p.state.Proof != token {
		t.Fatal("pairing replaced the pre-registration")
	}
	p.origin = mock.URL
	p.client = mock.Client()
	if err := p.sync(context.Background()); err == nil || err.Error() != "selected_node_unavailable" {
		t.Fatal(err)
	}
	if claims != 1 || p.state.MachineToken != "private-machine-token" || p.state.Proof != "" || p.state.Enrollment != "" {
		t.Fatal("enrollment not claimed", claims)
	}
	if _, err := p.importEnrollment(enrollmentJSON("", token, time.Now().Add(time.Hour))); err == nil || err.Error() != "connector_already_bound" {
		t.Fatal("bound Connector accepted another enrollment", err)
	}
	if err := os.WriteFile(source, enrollmentJSON("", token, time.Now().Add(-time.Hour)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stageEnrollment(source, store); err != nil || exists(store.file(enrollmentFile)) {
		t.Fatal("re-running --enroll on a bound Connector must be a no-op", err)
	}
}

func TestRejectedEnrollmentIsClearedSoPairingCanStartAgain(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(obj{"error": obj{"code": "invalid_challenge"}})
	}))
	defer mock.Close()
	store, _ := NewStore(t.TempDir(), roleConnector)
	p, _ := newConnectorPlatform(store, NewRuntime(roleConnector))
	p.origin = mock.URL
	p.client = mock.Client()
	if _, err := p.importEnrollment(enrollmentJSON(mock.URL, "opxe_"+strings.Repeat("c", 43), time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := p.sync(context.Background()); err == nil || err.Error() != "invalid_challenge" {
		t.Fatal(err)
	}
	if p.state.Proof != "" || p.state.Enrollment != "" || p.view()["enrollmentName"] != nil {
		t.Fatal("cancelled enrollment kept")
	}
}
