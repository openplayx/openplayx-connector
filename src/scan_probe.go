package main

// Protocol fingerprinting of open TCP ports. Every probe uses a fresh
// connection, sends nothing secret and never logs in.
import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const probeTimeout = 1500 * time.Millisecond

func fingerprintTCP(ctx context.Context, host string, port int, hint scanResult) scanResult {
	address := net.JoinHostPort(host, strconv.Itoa(port))
	r := scanResult{Host: host, Port: port, Proto: "tcp", Type: "other"}
	defer func() {
		if r.Name == "" && hint.Name != "" {
			r.Name = hint.Name
		}
	}()
	// Well-known binary protocols first: they close on unexpected input.
	switch port {
	case 445, 139:
		if probeSMB(ctx, address, &r) {
			return r
		}
	case 2049:
		if probeNFS(ctx, address, &r) {
			return r
		}
	}
	if banner := readBanner(ctx, address, probeTimeout); banner != "" {
		line := cleanLine(banner)
		switch {
		case strings.HasPrefix(banner, "SSH-"):
			// SFTP is an SSH subsystem; confirming it needs a login.
			r.Type, r.Name, r.Detail = "sftp", "SSH", line+" · SFTP 需登录后确认"
		case strings.HasPrefix(banner, "220"):
			r.Type, r.Confirmed, r.Detail = "ftp", true, line
			if ftpAuthTLS(ctx, address) {
				r.Detail = "支持 FTPES (AUTH TLS) · " + line
			}
		default:
			r.Detail = line
		}
		return r
	}
	if state, banner, ok := probeTLS(ctx, address); ok {
		if strings.HasPrefix(banner, "220") {
			r.Type, r.Confirmed, r.Detail = "ftps", true, "隐式 FTPS · "+cleanLine(banner)
			return r
		}
		if identifyWeb(ctx, "https://"+address, &r, hint) {
			return r
		}
		r.Detail = "TLS 服务 · " + tls.VersionName(state.Version)
		return r
	}
	if rawHTTP(ctx, address) && identifyWeb(ctx, "http://"+address, &r, hint) {
		return r
	}
	if port != 445 && port != 139 && probeSMB(ctx, address, &r) {
		return r
	}
	if port != 2049 && probeNFS(ctx, address, &r) {
		return r
	}
	return r
}

func cleanLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	// Banners of binary protocols are not text: keep printable runes only.
	s = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || !unicode.IsPrint(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, " "))
	s = strings.Join(strings.Fields(s), " ")
	if runes := []rune(s); len(runes) > 80 {
		s = string(runes[:80])
	}
	return s
}

func dialProbe(ctx context.Context, address string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: probeTimeout}
	return dialer.DialContext(ctx, "tcp", address)
}

// readBanner returns what a server-first protocol (SSH, FTP, SMTP, ...) sends.
func readBanner(ctx context.Context, address string, wait time.Duration) string {
	conn, err := dialProbe(ctx, address)
	if err != nil {
		return ""
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(wait))
	buf := make([]byte, 512)
	n, _ := conn.Read(buf)
	return string(buf[:n])
}

func ftpAuthTLS(ctx context.Context, address string) bool {
	conn, err := dialProbe(ctx, address)
	if err != nil {
		return false
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	reader := bufio.NewReader(conn)
	if !readFTPReply(reader, "220") {
		return false
	}
	conn.Write([]byte("AUTH TLS\r\n"))
	ok := readFTPReply(reader, "234")
	conn.Write([]byte("QUIT\r\n"))
	return ok
}

// readFTPReply reads a possibly multi-line reply and checks its code.
func readFTPReply(reader *bufio.Reader, code string) bool {
	for i := 0; i < 20; i++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			return false
		}
		if len(line) >= 4 && line[3] == ' ' {
			return strings.HasPrefix(line, code)
		}
	}
	return false
}

// Discovery only: certificates of home services are commonly self-signed and
// nothing secret is sent, so the probes do not verify them.
func probeTLS(ctx context.Context, address string) (tls.ConnectionState, string, bool) {
	conn, err := dialProbe(ctx, address)
	if err != nil {
		return tls.ConnectionState{}, "", false
	}
	defer conn.Close()
	client := tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if client.HandshakeContext(ctx) != nil {
		return tls.ConnectionState{}, "", false
	}
	client.SetReadDeadline(time.Now().Add(800 * time.Millisecond))
	buf := make([]byte, 256)
	n, _ := client.Read(buf)
	return client.ConnectionState(), string(buf[:n]), true
}

func rawHTTP(ctx context.Context, address string) bool {
	conn, err := dialProbe(ctx, address)
	if err != nil {
		return false
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(probeTimeout))
	conn.Write([]byte("HEAD / HTTP/1.0\r\nHost: " + address + "\r\n\r\n"))
	buf := make([]byte, 5)
	n, _ := io.ReadFull(conn, buf)
	return string(buf[:n]) == "HTTP/"
}

var scanHTTPClient = &http.Client{
	Timeout:       3 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, Proxy: nil, DisableKeepAlives: true},
}

func scanRequest(ctx context.Context, method, target string) (*http.Response, []byte) {
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, nil
	}
	if method == "PROPFIND" {
		req.Header.Set("Depth", "0")
	}
	resp, err := scanHTTPClient.Do(req)
	if err != nil {
		return nil, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	return resp, raw
}

var htmlTitle = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// identifyWeb classifies an HTTP(S) service: media servers, WebDAV and DLNA
// first, otherwise plain HTTP/HTTPS with its title or Server header.
func identifyWeb(ctx context.Context, base string, r *scanResult, hint scanResult) bool {
	root, body := scanRequest(ctx, "GET", base+"/")
	if root == nil {
		return false
	}
	scheme := strings.SplitN(base, ":", 2)[0]
	r.Type, r.Confirmed = scheme, true
	details := []string{}
	if server := cleanLine(root.Header.Get("Server")); server != "" {
		details = append(details, server)
	}
	if strings.Contains(root.Header.Get("Alt-Svc"), "h3") {
		details = append(details, "Alt-Svc: h3")
	}
	if match := htmlTitle.FindSubmatch(body); match != nil {
		r.Name = cleanLine(string(match[1]))
	}
	defer func() { r.Detail = strings.Join(append([]string{strings.ToUpper(scheme)}, details...), " · ") }()

	if resp, raw := scanRequest(ctx, "GET", base+"/System/Info/Public"); resp != nil && resp.StatusCode == 200 {
		var info struct{ ServerName, Version, Id, ProductName string }
		if json.Unmarshal(raw, &info) == nil && info.Id != "" && info.Version != "" {
			r.Type, r.Name = "emby", cleanLine(info.ServerName)
			if strings.Contains(strings.ToLower(info.ProductName), "jellyfin") {
				r.Type = "jellyfin"
			}
			details = append(details, "版本 "+cleanLine(info.Version))
			return true
		}
	}
	if resp, raw := scanRequest(ctx, "GET", base+"/identity"); resp != nil && resp.StatusCode == 200 && strings.Contains(string(raw), "machineIdentifier") {
		r.Type = "plex"
		if hint.Type == "plex" && hint.Name != "" {
			r.Name = hint.Name
		} else {
			r.Name = "Plex Media Server"
		}
		return true
	}
	if hint.Type == "dlna" {
		r.Type, r.Name = "dlna", hint.Name
		details = append(details, "UPnP MediaServer")
		return true
	}
	if resp, _ := scanRequest(ctx, "OPTIONS", base+"/"); resp != nil && resp.Header.Get("DAV") != "" {
		r.Type = "webdav"
		return true
	}
	if resp, _ := scanRequest(ctx, "PROPFIND", base+"/"); resp != nil {
		switch {
		case resp.StatusCode == 207:
			r.Type = "webdav"
			return true
		// Servers that require login hide the DAV header. A challenge for
		// PROPFIND on a non-HTML root suggests WebDAV rather than a web page.
		case resp.StatusCode == 401 && !strings.Contains(root.Header.Get("Content-Type"), "html"):
			r.Type, r.Confirmed = "webdav", false
			details = append(details, "需要登录，按 PROPFIND 推测")
			return true
		}
	}
	if hint.Type == "webdav" {
		r.Type = "webdav"
		details = append(details, "mDNS 广播")
	}
	return true
}

// probeSMB sends an SMB2 NEGOTIATE request. Any SMB1/SMB2 reply, or a
// NetBIOS session-service answer on port 139, identifies SMB.
func probeSMB(ctx context.Context, address string, r *scanResult) bool {
	conn, err := dialProbe(ctx, address)
	if err != nil {
		return false
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(probeTimeout))
	if _, err = conn.Write(smbNegotiate()); err != nil {
		return false
	}
	reply := make([]byte, 4+64+8)
	n, _ := io.ReadFull(conn, reply)
	if n >= 4 && (reply[0] == 0x82 || reply[0] == 0x83) {
		r.Type, r.Confirmed, r.Detail = "smb", true, "NetBIOS 会话服务"
		return true
	}
	if n < 8 || (string(reply[4:8]) != "\xfeSMB" && string(reply[4:8]) != "\xffSMB") {
		return false
	}
	r.Type, r.Confirmed = "smb", true
	if string(reply[4:8]) == "\xffSMB" {
		r.Detail = "SMB1"
	} else if n >= 4+64+6 {
		dialects := map[uint16]string{0x0202: "SMB 2.0.2", 0x0210: "SMB 2.1", 0x0300: "SMB 3.0", 0x0302: "SMB 3.0.2", 0x0311: "SMB 3.1.1"}
		if name, ok := dialects[binary.LittleEndian.Uint16(reply[4+64+4:])]; ok {
			r.Detail = name
		}
	}
	return true
}

func smbNegotiate() []byte {
	header := make([]byte, 64)
	copy(header, "\xfeSMB")
	header[4] = 64 // StructureSize
	header[14] = 1 // CreditRequest
	dialects := []uint16{0x0202, 0x0210, 0x0300, 0x0302}
	body := make([]byte, 36+2*len(dialects))
	body[0] = 36 // StructureSize
	body[2] = byte(len(dialects))
	body[4] = 1 // SecurityMode: signing enabled
	copy(body[12:28], "openplayx-scan!!")
	for i, d := range dialects {
		binary.LittleEndian.PutUint16(body[36+2*i:], d)
	}
	message := append(header, body...)
	frame := make([]byte, 4, 4+len(message))
	binary.BigEndian.PutUint32(frame, uint32(len(message)))
	return append(frame, message...)
}

// probeNFS sends an ONC RPC NULL call for the NFS program (100003, v3).
// SUCCESS or PROG_MISMATCH (other versions only) both mean NFS is served.
func probeNFS(ctx context.Context, address string, r *scanResult) bool {
	conn, err := dialProbe(ctx, address)
	if err != nil {
		return false
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(probeTimeout))
	call := make([]byte, 44)
	binary.BigEndian.PutUint32(call[0:], 0x80000000|40) // last fragment, 40 bytes
	binary.BigEndian.PutUint32(call[4:], 0x6f707873)    // xid
	binary.BigEndian.PutUint32(call[12:], 2)            // RPC version
	binary.BigEndian.PutUint32(call[16:], 100003)       // NFS
	binary.BigEndian.PutUint32(call[20:], 3)            // version 3, procedure 0 (NULL), AUTH_NONE
	if _, err = conn.Write(call); err != nil {
		return false
	}
	reply := make([]byte, 36)
	n, _ := io.ReadFull(conn, reply)
	if n < 28 || binary.BigEndian.Uint32(reply[4:]) != 0x6f707873 || binary.BigEndian.Uint32(reply[8:]) != 1 || binary.BigEndian.Uint32(reply[12:]) != 0 {
		return false
	}
	switch binary.BigEndian.Uint32(reply[24:]) {
	case 0:
		r.Type, r.Confirmed, r.Detail = "nfs", true, "NFSv3"
	case 2:
		r.Type, r.Confirmed, r.Detail = "nfs", true, "NFS"
		if n >= 36 {
			low, high := binary.BigEndian.Uint32(reply[28:]), binary.BigEndian.Uint32(reply[32:])
			r.Detail = "NFSv" + strconv.Itoa(int(low))
			if high != low {
				r.Detail += "–v" + strconv.Itoa(int(high))
			}
		}
	default:
		return false
	}
	return true
}
