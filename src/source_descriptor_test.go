package main

import (
	"strings"
	"testing"
)

func TestSourceDescriptorValidation(t *testing.T) {
	good := obj{"schemaVersion": 1, "kind": "jellyfin", "application": obj{"scheme": "https", "basePath": "/jellyfin", "tlsServerName": "media.example.test"}, "localAccess": obj{"mode": "direct_https", "endpoint": "https://media.example.test:8920/jellyfin", "identity": obj{"kind": "tls_certificate_sha256", "value": strings.Repeat("a", 64)}}}
	next, err := validateSourceDescriptor(good, "jellyfin")
	if err != nil {
		t.Fatal(err)
	}
	good["kind"] = "emby"
	if next["kind"] != "jellyfin" {
		t.Fatal("descriptor aliases mutable input")
	}
	if _, err := validateSourceDescriptor(good, "jellyfin"); err == nil {
		t.Fatal("kind mismatch accepted")
	}
	good["kind"] = "jellyfin"
	for _, endpoint := range []string{"http://nas.test/jellyfin", "https://user:password@nas.test/jellyfin", "https://nas.test/jellyfin?token=secret", "https://nas.test/wrong"} {
		changed := cloneObj(good)
		changed["localAccess"].(obj)["endpoint"] = endpoint
		if _, err := validateSourceDescriptor(changed, "jellyfin"); err == nil {
			t.Fatal("unsafe local entry accepted", endpoint)
		}
	}
	invalid := cloneObj(good)
	invalid["password"] = "secret"
	if _, err := validateSourceDescriptor(invalid, "jellyfin"); err == nil {
		t.Fatal("secret accepted")
	}
	smb := obj{"schemaVersion": 1, "kind": "smb", "application": obj{"scheme": "smb"}}
	if _, err := validateSourceDescriptor(smb, "smb"); err != nil {
		t.Fatal(err)
	}
	smb["localAccess"] = obj{"endpoint": "smb://nas.test"}
	if _, err := validateSourceDescriptor(smb, "smb"); err == nil {
		t.Fatal("unverified SMB accepted")
	}
}
