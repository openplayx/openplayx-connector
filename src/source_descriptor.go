package main

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// Metadata users explicitly publish to their account devices. It never
// contains the Connector's forwarding target or media account credentials.
func validateSourceDescriptor(value obj, kind string) (obj, error) {
	if value == nil {
		return nil, nil
	}
	bad := errors.New("invalid_source_descriptor")
	if checkKeys(value, []string{"schemaVersion", "kind", "application", "localAccess"}, []string{"schemaVersion", "kind", "application"}) != nil {
		return nil, bad
	}
	version, ok := intValue(value["schemaVersion"])
	if !ok || version != 1 || stringValue(value["kind"]) != kind || kind == "other_tcp" {
		return nil, bad
	}
	app, ok := value["application"].(map[string]any)
	if !ok {
		return nil, bad
	}
	if checkKeys(app, []string{"scheme", "basePath", "tlsServerName"}, []string{"scheme"}) != nil {
		return nil, bad
	}
	scheme := stringValue(app["scheme"])
	if kind == "smb" {
		if scheme != "smb" || len(app) != 1 || value["localAccess"] != nil {
			return nil, bad
		}
		return cloneObj(value), nil
	}
	if scheme != "http" && scheme != "https" {
		return nil, bad
	}
	if raw, exists := app["basePath"]; exists {
		if _, ok := raw.(string); !ok {
			return nil, bad
		}
	}
	if raw, exists := app["tlsServerName"]; exists {
		if _, ok := raw.(string); !ok {
			return nil, bad
		}
	}
	path := stringValue(app["basePath"])
	if len(path) > 1024 || path != "" && (!strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "?#\\ \r\n\t")) {
		return nil, bad
	}
	host := stringValue(app["tlsServerName"])
	if host != "" && (!regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9.-]*[a-zA-Z0-9])?$`).MatchString(host) || strings.Contains(host, "..") || len(host) > 253) {
		return nil, bad
	}
	if scheme == "https" && host == "" {
		return nil, bad
	}
	if value["localAccess"] != nil {
		local, ok := value["localAccess"].(map[string]any)
		if !ok || checkKeys(local, []string{"mode", "endpoint", "identity"}, []string{"mode", "endpoint", "identity"}) != nil || local["mode"] != "direct_https" {
			return nil, bad
		}
		endpoint := stringValue(local["endpoint"])
		uri, err := url.Parse(endpoint)
		if err != nil || len(endpoint) > 2048 || uri.Scheme != "https" || uri.Hostname() == "" || uri.User != nil || uri.RawQuery != "" || uri.ForceQuery || uri.Fragment != "" || strings.TrimSuffix(uri.Path, "/") != strings.TrimSuffix(path, "/") {
			return nil, bad
		}
		identity, ok := local["identity"].(map[string]any)
		if !ok || checkKeys(identity, []string{"kind", "value"}, []string{"kind", "value"}) != nil || identity["kind"] != "tls_certificate_sha256" || !regexp.MustCompile(`^[a-fA-F0-9]{64}$`).MatchString(stringValue(identity["value"])) {
			return nil, bad
		}
	}
	result := cloneObj(value)
	result["application"].(obj)["basePath"] = path
	if local, ok := result["localAccess"].(obj); ok {
		uri, _ := url.Parse(stringValue(local["endpoint"]))
		uri.Host = strings.ToLower(uri.Host)
		if uri.Port() == "443" {
			uri.Host = uri.Hostname()
			if strings.Contains(uri.Host, ":") {
				uri.Host = "[" + uri.Host + "]"
			}
		}
		if uri.Path == "" {
			uri.Path = "/"
		}
		local["endpoint"] = uri.String()
		identity := local["identity"].(obj)
		identity["value"] = strings.ToLower(stringValue(identity["value"]))
	}
	return result, nil
}
func descriptorsEqual(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

func cloneDescriptor(value obj) obj {
	if value == nil {
		return nil
	}
	return cloneObj(value)
}
func descriptorFromRequest(data obj) obj {
	if data["sourceDescriptor"] == nil {
		return nil
	}
	if value, ok := data["sourceDescriptor"].(map[string]any); ok {
		return value
	}
	// Force validation failure, rather than silently discarding malformed input.
	return obj{"invalid": true}
}
