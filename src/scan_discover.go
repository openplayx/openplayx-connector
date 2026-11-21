package main

// Host discovery: which LAN addresses are in use, their names, and services
// that announce themselves (SSDP/DLNA, Plex GDM, Jellyfin/Emby, mDNS).
import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/miekg/dns"
)

const discoverWindow = 2500 * time.Millisecond

func discoverLAN(ctx context.Context, hosts []netip.Addr) *discovery {
	d := newDiscovery()
	candidates := map[netip.Addr]bool{}
	for _, h := range hosts {
		candidates[h] = true
	}
	var wait sync.WaitGroup
	for _, task := range []func(){
		func() { netbiosSweep(ctx, hosts, d) },
		func() { livenessSweep(ctx, hosts, d) },
		func() { ssdpDiscover(ctx, d) },
		func() { gdmDiscover(ctx, d) },
		func() { mediaServerDiscover(ctx, "who is JellyfinServer?", "jellyfin", d) },
		func() { mediaServerDiscover(ctx, "who is EmbyServer?", "emby", d) },
		func() { mdnsDiscover(ctx, d) },
	} {
		wait.Add(1)
		go func(task func()) { defer wait.Done(); task() }(task)
	}
	wait.Wait()
	for addr := range arpNeighbours(ctx) {
		if candidates[addr] {
			d.alive(addr, "")
		}
	}
	// Only addresses that belong to the scanned networks count.
	for addr := range d.live {
		if !candidates[addr] {
			delete(d.live, addr)
		}
	}
	return d
}

// udpExchange sends payload to every target from one socket and hands each
// reply to handle until the window closes.
func udpExchange(ctx context.Context, payload []byte, targets []*net.UDPAddr, window time.Duration, handle func(netip.Addr, []byte)) {
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	for _, target := range targets {
		for attempt := 0; attempt < 3; attempt++ {
			if _, err := conn.WriteToUDP(payload, target); !errors.Is(err, syscall.ENOBUFS) {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	conn.SetReadDeadline(time.Now().Add(window))
	buf := make([]byte, 9000)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if addr, ok := netip.AddrFromSlice(from.IP.To4()); ok {
			handle(addr, append([]byte(nil), buf[:n]...))
		}
	}
}

func broadcastTargets(port int) []*net.UDPAddr {
	targets := []*net.UDPAddr{{IP: net.IPv4bcast, Port: port}}
	for _, network := range localNetworks() {
		prefix := network.Masked()
		last := prefix.Addr().As4()
		bits := prefix.Bits()
		for i := bits; i < 32; i++ {
			last[i/8] |= 1 << (7 - uint(i%8))
		}
		targets = append(targets, &net.UDPAddr{IP: net.IP(last[:]), Port: port})
	}
	return targets
}

// livenessSweep connects to a few common ports. A refused connection proves
// the host exists as much as an accepted one; the attempts also fill the ARP
// cache for hosts that silently drop them.
func livenessSweep(ctx context.Context, hosts []netip.Addr, d *discovery) {
	ports := []int{80, 443, 445, 22, 8080, 139, 53, 5000, 62078}
	jobs := make(chan string)
	var wait sync.WaitGroup
	for i := 0; i < 256; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			dialer := net.Dialer{Timeout: 300 * time.Millisecond}
			for address := range jobs {
				conn, err := dialer.DialContext(ctx, "tcp", address)
				if err == nil {
					conn.Close()
				}
				if err == nil || errors.Is(err, syscall.ECONNREFUSED) {
					host, _, _ := net.SplitHostPort(address)
					d.alive(netip.MustParseAddr(host), "")
				}
			}
		}()
	}
feed:
	for _, p := range ports {
		for _, h := range hosts {
			select {
			case jobs <- net.JoinHostPort(h.String(), strconv.Itoa(p)):
			case <-ctx.Done():
				break feed
			}
		}
	}
	close(jobs)
	wait.Wait()
}

// netbiosSweep sends a NetBIOS node status query (NBSTAT "*") to every host;
// Windows, Samba and most NAS systems answer with their computer name.
func netbiosSweep(ctx context.Context, hosts []netip.Addr, d *discovery) {
	query := []byte{0x6f, 0x70, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0x20, 'C', 'K'}
	for i := 0; i < 30; i++ {
		query = append(query, 'A')
	}
	query = append(query, 0, 0, 0x21, 0, 1)
	targets := []*net.UDPAddr{}
	for _, h := range hosts {
		if !h.IsLoopback() {
			targets = append(targets, &net.UDPAddr{IP: net.IP(h.AsSlice()), Port: 137})
		}
	}
	udpExchange(ctx, query, targets, 1500*time.Millisecond, func(from netip.Addr, reply []byte) {
		d.alive(from, parseNBSTAT(reply))
	})
}

func parseNBSTAT(reply []byte) string {
	// header 12 + name 34 + type 2 + class 2 + ttl 4 + rdlength 2 + count 1
	const offset = 12 + 34 + 10
	if len(reply) < offset+1 {
		return ""
	}
	count := int(reply[offset])
	for i := 0; i < count; i++ {
		entry := offset + 1 + i*18
		if entry+18 > len(reply) {
			break
		}
		suffix, flags := reply[entry+15], binary.BigEndian.Uint16(reply[entry+16:])
		if suffix == 0 && flags&0x8000 == 0 { // workstation name, not a group
			return cleanLine(strings.TrimRight(string(reply[entry:entry+15]), " \x00"))
		}
	}
	return ""
}

// arpNeighbours reads the kernel's neighbour table.
func arpNeighbours(ctx context.Context) map[netip.Addr]bool {
	result := map[netip.Addr]bool{}
	if raw, err := os.ReadFile("/proc/net/arp"); err == nil {
		scanner := bufio.NewScanner(bytes.NewReader(raw))
		scanner.Scan() // header
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) >= 4 && fields[2] != "0x0" && fields[3] != "00:00:00:00:00:00" {
				if addr, err := netip.ParseAddr(fields[0]); err == nil {
					result[addr] = true
				}
			}
		}
		return result
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/arp", "-an").Output()
	if err != nil {
		return result
	}
	entry := regexp.MustCompile(`\((\d+\.\d+\.\d+\.\d+)\) at ([0-9a-fA-F:]+)`)
	for _, match := range entry.FindAllStringSubmatch(string(out), -1) {
		if addr, err := netip.ParseAddr(match[1]); err == nil && match[2] != "ff:ff:ff:ff:ff:ff" {
			result[addr] = true
		}
	}
	return result
}

// httpHeaders parses the header block of an SSDP/GDM datagram.
func httpHeaders(reply []byte) map[string]string {
	headers := map[string]string{}
	for _, line := range strings.Split(string(reply), "\n") {
		if key, value, ok := strings.Cut(line, ":"); ok {
			headers[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
		}
	}
	return headers
}

func ssdpDiscover(ctx context.Context, d *discovery) {
	search := []byte("M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\nST: urn:schemas-upnp-org:device:MediaServer:1\r\n\r\n")
	locations := map[string]netip.Addr{}
	var mu sync.Mutex
	udpExchange(ctx, search, []*net.UDPAddr{{IP: net.IPv4(239, 255, 255, 250), Port: 1900}}, discoverWindow, func(from netip.Addr, reply []byte) {
		if location := httpHeaders(reply)["location"]; location != "" {
			mu.Lock()
			locations[location] = from
			mu.Unlock()
		}
	})
	for location, from := range locations {
		target, err := url.Parse(location)
		// Only fetch descriptions served by the device that answered.
		if err != nil || target.Scheme != "http" || target.Hostname() != from.String() {
			continue
		}
		port, _ := strconv.Atoi(target.Port())
		if port == 0 {
			port = 80
		}
		name, kind := upnpDescription(ctx, location)
		if strings.Contains(kind, "MediaServer") {
			d.hint(scanResult{Host: from.String(), Port: port, Proto: "tcp", Type: "dlna", Name: name, Confirmed: true})
		}
	}
}

func upnpDescription(ctx context.Context, location string) (string, string) {
	resp, raw := scanRequest(ctx, "GET", location)
	if resp == nil || resp.StatusCode != http.StatusOK {
		return "", ""
	}
	var description struct {
		Device struct {
			DeviceType   string `xml:"deviceType"`
			FriendlyName string `xml:"friendlyName"`
		} `xml:"device"`
	}
	if xml.Unmarshal(raw, &description) != nil {
		return "", ""
	}
	return cleanLine(description.Device.FriendlyName), description.Device.DeviceType
}

// gdmDiscover uses Plex's "G'Day Mate" discovery on UDP 32414.
func gdmDiscover(ctx context.Context, d *discovery) {
	targets := append([]*net.UDPAddr{{IP: net.IPv4(239, 0, 0, 250), Port: 32414}}, broadcastTargets(32414)...)
	udpExchange(ctx, []byte("M-SEARCH * HTTP/1.1\r\n\r\n"), targets, discoverWindow, func(from netip.Addr, reply []byte) {
		headers := httpHeaders(reply)
		if !strings.Contains(headers["content-type"], "plex/media-server") {
			return
		}
		port, err := strconv.Atoi(headers["port"])
		if err != nil {
			port = 32400
		}
		d.hint(scanResult{Host: from.String(), Port: port, Proto: "tcp", Type: "plex", Name: cleanLine(headers["name"]), Confirmed: true})
	})
}

// mediaServerDiscover uses the Jellyfin/Emby discovery protocol on UDP 7359.
func mediaServerDiscover(ctx context.Context, question, kind string, d *discovery) {
	udpExchange(ctx, []byte(question), broadcastTargets(7359), discoverWindow, func(from netip.Addr, reply []byte) {
		var answer struct{ Address, Id, Name string }
		if json.Unmarshal(reply, &answer) != nil || answer.Address == "" {
			return
		}
		target, err := url.Parse(answer.Address)
		if err != nil {
			return
		}
		port, _ := strconv.Atoi(target.Port())
		if port == 0 {
			port = map[string]int{"http": 80, "https": 443}[target.Scheme]
		}
		d.hint(scanResult{Host: from.String(), Port: port, Proto: "tcp", Type: kind, Name: cleanLine(answer.Name), Confirmed: true})
	})
}

var mdnsServices = map[string]string{
	"_smb._tcp": "smb", "_nfs._tcp": "nfs", "_webdav._tcp": "webdav", "_webdavs._tcp": "webdav",
	"_ftp._tcp": "ftp", "_sftp-ssh._tcp": "sftp", "_ssh._tcp": "sftp", "_http._tcp": "http",
	"_https._tcp": "https", "_plexmediasvr._tcp": "plex",
}

// mdnsDiscover sends one legacy unicast mDNS query (RFC 6762 §6.7) for the
// service types above and reads SRV/A records from the answers.
func mdnsDiscover(ctx context.Context, d *discovery) {
	query := new(dns.Msg)
	for service := range mdnsServices {
		query.Question = append(query.Question, dns.Question{Name: service + ".local.", Qtype: dns.TypePTR, Qclass: dns.ClassINET})
	}
	payload, err := query.Pack()
	if err != nil {
		return
	}
	type srv struct {
		instance, target string
		port             int
		from             netip.Addr
	}
	var mu sync.Mutex
	records := []srv{}
	addresses := map[string]netip.Addr{}
	udpExchange(ctx, payload, []*net.UDPAddr{{IP: net.IPv4(224, 0, 0, 251), Port: 5353}}, discoverWindow, func(from netip.Addr, reply []byte) {
		var msg dns.Msg
		if msg.Unpack(reply) != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		for _, rr := range append(msg.Answer, msg.Extra...) {
			switch record := rr.(type) {
			case *dns.SRV:
				records = append(records, srv{record.Hdr.Name, record.Target, int(record.Port), from})
			case *dns.A:
				if addr, ok := netip.AddrFromSlice(record.A.To4()); ok {
					addresses[strings.ToLower(record.Hdr.Name)] = addr
				}
			}
		}
	})
	for _, record := range records {
		lower := strings.ToLower(record.instance)
		for service, kind := range mdnsServices {
			suffix := "." + service + ".local."
			if !strings.HasSuffix(lower, suffix) {
				continue
			}
			host := record.from
			if addr, ok := addresses[strings.ToLower(record.target)]; ok {
				host = addr
			}
			hostName := strings.TrimSuffix(strings.TrimSuffix(record.target, "."), ".local")
			d.alive(host, hostName)
			d.hint(scanResult{Host: host.String(), Port: record.port, Proto: "tcp", Type: kind, Name: unescapeDNS(record.instance[:len(record.instance)-len(suffix)])})
		}
	}
}

// unescapeDNS turns a presentation-format label ("My\ NAS\032x") into text.
func unescapeDNS(label string) string {
	var out strings.Builder
	for i := 0; i < len(label); i++ {
		if label[i] != '\\' || i+1 >= len(label) {
			out.WriteByte(label[i])
			continue
		}
		if i+3 < len(label) && isDigits(label[i+1:i+4]) {
			n, _ := strconv.Atoi(label[i+1 : i+4])
			out.WriteByte(byte(n))
			i += 3
			continue
		}
		out.WriteByte(label[i+1])
		i++
	}
	return cleanLine(out.String())
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
