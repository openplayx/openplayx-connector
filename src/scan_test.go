package main

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

func serverPort(t *testing.T, address string) int {
	_, raw, _ := net.SplitHostPort(address)
	n, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// fakeTCP serves handle on a loopback port until the test ends.
func fakeTCP(t *testing.T, handle func(net.Conn)) int {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				handle(conn)
			}()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

func webServer(t *testing.T, tlsOn bool, handler http.HandlerFunc) int {
	server := httptest.NewUnstartedServer(handler)
	if tlsOn {
		server.StartTLS()
	} else {
		server.Start()
	}
	t.Cleanup(server.Close)
	return serverPort(t, server.Listener.Addr().String())
}

func loopbackScanner(ports []int) *lanScanner {
	return &lanScanner{workers: 16, dialTimeout: 300 * time.Millisecond, udpRate: 20000, ports: ports, hosts: func() ([]netip.Addr, map[netip.Addr]bool) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, map[netip.Addr]bool{}
	}}
}

func runScan(t *testing.T, s *lanScanner) scanStatus {
	if err := s.start(1, 65535); err != nil {
		t.Fatal(err)
	}
	if s.start(1, 65535) == nil {
		t.Fatal("concurrent scan allowed")
	}
	select {
	case <-s.done:
	case <-time.After(90 * time.Second):
		t.Fatal("scan did not finish")
	}
	status := s.view()
	if status.Running || status.Phase != "done" {
		t.Fatalf("status: %+v", status)
	}
	return status
}

func TestLANScanFingerprintsProtocols(t *testing.T) {
	ports := map[string]int{}
	ports["jellyfin"] = webServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/System/Info/Public" {
			w.Write([]byte(`{"ServerName":"Home","Version":"10.10.0","Id":"abc","ProductName":"Jellyfin Server"}`))
			return
		}
		w.WriteHeader(404)
	})
	ports["emby"] = webServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/System/Info/Public" {
			w.Write([]byte(`{"ServerName":"Emby","Version":"4.8","Id":"def"}`))
			return
		}
		w.WriteHeader(404)
	})
	ports["plex"] = webServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/identity" {
			w.Write([]byte(`<MediaContainer size="0" machineIdentifier="x" version="1.40"/>`))
			return
		}
		w.WriteHeader(401)
	})
	ports["webdav"] = webServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "OPTIONS" {
			w.Header().Set("DAV", "1, 2")
		}
		w.WriteHeader(401)
	})
	ports["authdav"] = webServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(401)
	})
	ports["http"] = webServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><title>Router</title></html>"))
	})
	ports["https"] = webServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Alt-Svc", `h3=":443"`)
		w.Write([]byte("ok"))
	})
	ports["ssh"] = fakeTCP(t, func(c net.Conn) { c.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n")); io.Copy(io.Discard, c) })
	ports["ftp"] = fakeTCP(t, func(c net.Conn) {
		c.Write([]byte("220-Welcome\r\n220 FTP ready\r\n"))
		reader := bufio.NewReader(c)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(line, "AUTH TLS") {
				c.Write([]byte("234 Proceed\r\n"))
			}
		}
	})
	cert := httptest.NewTLSServer(http.NotFoundHandler())
	certificates := cert.TLS.Certificates
	cert.Close()
	ports["ftps"] = fakeTCP(t, func(c net.Conn) {
		server := tls.Server(c, &tls.Config{Certificates: certificates})
		if server.Handshake() == nil {
			server.Write([]byte("220 secure FTP\r\n"))
			io.Copy(io.Discard, server)
		}
	})
	ports["smb"] = fakeTCP(t, func(c net.Conn) {
		request := make([]byte, 8)
		if _, err := io.ReadFull(c, request); err != nil || string(request[4:]) != "\xfeSMB" {
			return
		}
		reply := make([]byte, 4+64+8)
		binary.BigEndian.PutUint32(reply, 64+8)
		copy(reply[4:], "\xfeSMB")
		binary.LittleEndian.PutUint16(reply[4+64+4:], 0x0302)
		c.Write(reply)
	})
	ports["nfs"] = fakeTCP(t, func(c net.Conn) {
		call := make([]byte, 44)
		if _, err := io.ReadFull(c, call); err != nil || binary.BigEndian.Uint32(call[16:]) != 100003 {
			return
		}
		reply := make([]byte, 36)
		binary.BigEndian.PutUint32(reply, 0x80000000|32)
		copy(reply[4:8], call[4:8])
		binary.BigEndian.PutUint32(reply[8:], 1)
		binary.BigEndian.PutUint32(reply[24:], 2) // PROG_MISMATCH
		binary.BigEndian.PutUint32(reply[28:], 4)
		binary.BigEndian.PutUint32(reply[32:], 4)
		c.Write(reply)
	})
	ports["silent"] = fakeTCP(t, func(c net.Conn) { io.Copy(io.Discard, c) })

	listener, err := quic.ListenAddr("127.0.0.1:0", &tls.Config{Certificates: certificates, NextProtos: []string{"h3"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept(t.Context())
			if err != nil {
				return
			}
			go func() { time.Sleep(time.Second); conn.CloseWithError(0, "") }()
		}
	}()
	quicPort := listener.Addr().(*net.UDPAddr).Port

	closed := fakeTCP(t, func(net.Conn) {})
	list := []int{quicPort, closed}
	for _, p := range ports {
		list = append(list, p)
	}
	status := runScan(t, loopbackScanner(list))
	got := map[string]scanResult{}
	for _, r := range status.Results {
		got[r.Proto+strconv.Itoa(r.Port)] = r
	}
	expect := map[string]struct {
		kind, service string
		confirmed     bool
	}{
		"jellyfin": {"jellyfin", "jellyfin", true}, "emby": {"emby", "emby", true}, "plex": {"plex", "other_tcp", true},
		"webdav": {"webdav", "webdav", true}, "authdav": {"webdav", "webdav", false}, "http": {"http", "other_tcp", true},
		"https": {"https", "other_tcp", true}, "ssh": {"sftp", "other_tcp", false}, "ftp": {"ftp", "", true},
		"ftps": {"ftps", "", true}, "smb": {"smb", "smb", true}, "nfs": {"nfs", "other_tcp", true}, "silent": {"other", "other_tcp", false},
	}
	for name, want := range expect {
		r, ok := got["tcp"+strconv.Itoa(ports[name])]
		if !ok || r.Type != want.kind || r.Service != want.service || r.Confirmed != want.confirmed {
			t.Errorf("%s: got %+v", name, r)
		}
	}
	if r := got["tcp"+strconv.Itoa(ports["jellyfin"])]; r.Name != "Home" {
		t.Errorf("jellyfin name: %+v", r)
	}
	if r := got["tcp"+strconv.Itoa(ports["ftp"])]; !strings.Contains(r.Detail, "FTPES") {
		t.Errorf("ftpes: %+v", r)
	}
	if r := got["tcp"+strconv.Itoa(ports["smb"])]; r.Detail != "SMB 3.0.2" {
		t.Errorf("smb dialect: %+v", r)
	}
	if r := got["tcp"+strconv.Itoa(ports["nfs"])]; r.Detail != "NFSv4" {
		t.Errorf("nfs version: %+v", r)
	}
	if r := got["tcp"+strconv.Itoa(ports["https"])]; !strings.Contains(r.Detail, "Alt-Svc: h3") {
		t.Errorf("alt-svc: %+v", r)
	}
	if r := got["tcp"+strconv.Itoa(ports["http"])]; r.Name != "Router" {
		t.Errorf("title: %+v", r)
	}
	if r := got["udp"+strconv.Itoa(quicPort)]; r.Type != "h3" || !r.Confirmed || r.Service != "" {
		t.Errorf("h3: %+v", r)
	}
	if _, ok := got["tcp"+strconv.Itoa(closed)]; !ok {
		t.Error("open port that closes immediately was not reported")
	}
}

func TestLANScanCancelAndRange(t *testing.T) {
	s := loopbackScanner(nil)
	s.udpRate = 0
	for _, bad := range [][2]int{{0, 10}, {10, 70000}, {20, 10}} {
		if s.start(bad[0], bad[1]) == nil {
			t.Fatal("accepted range", bad)
		}
	}
	slow := func() ([]netip.Addr, map[netip.Addr]bool) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("192.0.2.1")}, map[netip.Addr]bool{netip.MustParseAddr("192.0.2.1"): true}
	}
	s.hosts = slow
	if err := s.start(1, 65535); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	s.stop()
	if status := s.view(); status.Running || status.Phase != "canceled" {
		t.Fatalf("status: %+v", status)
	}
}

func TestFinishResultsDedupesAndHidesConnectorPorts(t *testing.T) {
	self := map[netip.Addr]bool{netip.MustParseAddr("192.168.1.5"): true}
	found := newDiscovery()
	found.names[netip.MustParseAddr("192.168.1.9")] = "NAS"
	results := finishResults([]scanResult{
		{Host: "127.0.0.1", Port: 8096, Proto: "tcp", Type: "jellyfin"},
		{Host: "192.168.1.5", Port: 8096, Proto: "tcp", Type: "jellyfin"},
		{Host: "192.168.1.5", Port: 9080, Proto: "tcp", Type: "http"},
		{Host: "192.168.1.9", Port: 22, Proto: "tcp", Type: "sftp"},
		{Host: "192.168.1.9", Port: 445, Proto: "tcp", Type: "smb"},
		{Host: "192.168.1.9", Port: 21, Proto: "tcp", Type: "ftp"},
	}, found, self)
	if len(results) != 4 || results[0].Type != "jellyfin" || results[0].Host != "127.0.0.1" || results[1].Type != "smb" || results[1].HostName != "NAS" {
		t.Fatalf("%+v", results)
	}
	for _, r := range results {
		if (r.Type == "sftp" || r.Type == "ftp") && r.Service != "" {
			t.Fatal("ssh port or ftp offered as a service mapping", r)
		}
	}
}

func TestDiscoveryParsers(t *testing.T) {
	reply := make([]byte, 12+34+10)
	reply = append(reply, 2)
	reply = append(reply, []byte("WORKGROUP      \x00\x80\x00")...)
	reply = append(reply, []byte("DS920          \x00\x04\x00")...)
	if name := parseNBSTAT(reply); name != "DS920" {
		t.Fatal(name)
	}
	if line := cleanLine("\x80\x01 RTSP\xff\x00ok\r\nnext"); line != "RTSP ok" {
		t.Fatalf("%q", line)
	}
	if name := unescapeDNS(`My\ NAS\032(2)\.`); name != "My NAS (2)." {
		t.Fatal(name)
	}
	headers := httpHeaders([]byte("HTTP/1.0 200 OK\r\nContent-Type: plex/media-server\r\nName: Den\r\nPort: 32400\r\n"))
	if headers["name"] != "Den" || headers["port"] != "32400" {
		t.Fatal(headers)
	}
	scid := []byte("12345678")
	vn := append([]byte{0x80, 0, 0, 0, 0, 8}, scid...)
	vn = append(vn, 0, 0, 0, 0, 0, 0, 0, 1)
	if !isVersionNegotiation(vn, scid) || isVersionNegotiation(vn, []byte("87654321")) || isVersionNegotiation(quicProbePacket(scid), scid) {
		t.Fatal("version negotiation parsing")
	}
	if len(quicProbePacket(scid)) < 1200 {
		t.Fatal("probe too short")
	}
}

func TestLANHostsAreLocal(t *testing.T) {
	hosts, _ := lanHosts()
	if len(hosts) == 0 || hosts[0].String() != "127.0.0.1" || len(hosts) > scanHostLimit {
		t.Fatal(len(hosts))
	}
	for _, h := range hosts {
		if !h.IsPrivate() && !h.IsLoopback() {
			t.Fatal("non-private host", h)
		}
	}
}

func TestScanScopeShowsActualCandidateRanges(t *testing.T) {
	hosts := []netip.Addr{}
	for _, ip := range []string{"127.0.0.1", "192.168.1.1", "192.168.1.254", "10.0.2.1", "10.0.2.126"} {
		hosts = append(hosts, netip.MustParseAddr(ip))
	}
	networks := []netip.Prefix{}
	for _, cidr := range []string{"192.168.1.107/24", "192.168.1.108/24", "10.0.2.30/25", "172.16.0.1/24"} {
		networks = append(networks, netip.MustParsePrefix(cidr))
	}
	scope := scanScope(hosts, networks)
	if len(scope) != 2 || scope[0] != (scanRange{"192.168.1.0/24", "192.168.1.1", "192.168.1.254"}) || scope[1] != (scanRange{"10.0.2.0/25", "10.0.2.1", "10.0.2.126"}) {
		t.Fatalf("unexpected scan ranges: %+v", scope)
	}
	// A network partially omitted by the global host limit must not promise
	// addresses which are absent from the actual scan plan.
	limited := scanScope(hosts[:2], networks)
	if len(limited) != 1 || limited[0].End != "192.168.1.1" {
		t.Fatalf("scope exceeds selected hosts: %+v", limited)
	}
}
