package main

// LAN discovery helps the administrator pick targets for platform services.
// It only reports candidates; nothing is registered until the administrator
// saves a service explicitly.
//
// A scan runs in the background: host discovery (broadcast protocols, NetBIOS,
// ARP and a short TCP sweep), a TCP connect scan of the chosen port range on
// live hosts, a QUIC version-negotiation sweep of the same UDP range, and
// finally protocol fingerprinting of every open port.
import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type scanResult struct {
	Host      string `json:"host"`
	HostName  string `json:"hostName,omitempty"`
	Port      int    `json:"port"`
	Proto     string `json:"proto"` // tcp | udp
	Type      string `json:"type"`  // smb nfs ftp ftps sftp webdav dlna http https h3 jellyfin emby plex other
	Name      string `json:"name,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Confirmed bool   `json:"confirmed"`
	Service   string `json:"service,omitempty"` // platform service type when the result can be mapped
}

type scanStatus struct {
	Running    bool         `json:"running"`
	Phase      string       `json:"phase,omitempty"` // discover | ports | identify | done | canceled | failed
	Done       int64        `json:"done"`
	Total      int64        `json:"total"`
	Hosts      int          `json:"hosts"`
	Live       int          `json:"live"`
	PortStart  int          `json:"portStart,omitempty"`
	PortEnd    int          `json:"portEnd,omitempty"`
	StartedAt  *time.Time   `json:"startedAt,omitempty"`
	FinishedAt *time.Time   `json:"finishedAt,omitempty"`
	Error      string       `json:"error,omitempty"`
	Results    []scanResult `json:"results"`
}

// discovery collects what broadcast protocols and liveness checks found.
type discovery struct {
	mu    sync.Mutex
	live  map[netip.Addr]bool
	names map[netip.Addr]string
	hints map[string]scanResult // key: scanKey(host, proto, port)
}

func newDiscovery() *discovery {
	return &discovery{live: map[netip.Addr]bool{}, names: map[netip.Addr]string{}, hints: map[string]scanResult{}}
}

func (d *discovery) alive(addr netip.Addr, name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.live[addr] = true
	if name != "" && d.names[addr] == "" {
		d.names[addr] = name
	}
}

func (d *discovery) hint(r scanResult) {
	addr, err := netip.ParseAddr(r.Host)
	if err != nil || !platformHost(r.Host) || r.Port < 1 || r.Port > 65535 {
		return
	}
	d.alive(addr, "")
	d.mu.Lock()
	defer d.mu.Unlock()
	key := scanKey(r.Host, r.Proto, r.Port)
	if old, ok := d.hints[key]; !ok || (!old.Confirmed && r.Confirmed) || old.Name == "" {
		if r.Name == "" {
			r.Name = old.Name
		}
		d.hints[key] = r
	}
}

func scanKey(host, proto string, port int) string {
	return proto + "/" + net.JoinHostPort(host, strconv.Itoa(port))
}

type lanScanner struct {
	mu     sync.Mutex
	status scanStatus
	cancel context.CancelFunc
	done   chan struct{}
	swept  atomic.Int64

	hosts       func() ([]netip.Addr, map[netip.Addr]bool)
	discover    func(context.Context, []netip.Addr) *discovery // nil: only loopback and own addresses are live
	dialTimeout time.Duration
	workers     int
	udpRate     int   // QUIC sweep packets per second; 0 disables it
	ports       []int // overrides the port range (tests)
}

const (
	scanHostLimit   = 1024
	scanResultLimit = 512
)

var connectorScanner = &lanScanner{hosts: lanHosts, discover: discoverLAN, dialTimeout: 400 * time.Millisecond, workers: 512, udpRate: 15000}

// lanHosts returns loopback plus every address of each private IPv4 network
// this machine is attached to. Networks wider than /24 are narrowed to the
// /24 around the local address to keep a scan short and local.
func lanHosts() ([]netip.Addr, map[netip.Addr]bool) {
	loopback := netip.MustParseAddr("127.0.0.1")
	hosts, self, seen := []netip.Addr{loopback}, map[netip.Addr]bool{}, map[netip.Addr]bool{loopback: true}
	for _, network := range localNetworks() {
		self[network.Addr()] = true
		prefix := network.Masked()
		for a := prefix.Addr().Next(); prefix.Contains(a) && prefix.Contains(a.Next()); a = a.Next() { // skip broadcast
			if !seen[a] && len(hosts) < scanHostLimit {
				seen[a] = true
				hosts = append(hosts, a)
			}
		}
	}
	return hosts, self
}

// localNetworks lists this machine's private IPv4 addresses with their
// prefix, clamped to at least /24.
func localNetworks() []netip.Prefix {
	result := []netip.Prefix{}
	interfaces, _ := net.Interfaces()
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			network, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(network.IP.To4())
			if !ok || !ip.IsPrivate() {
				continue
			}
			bits, _ := network.Mask.Size()
			if bits < 24 {
				bits = 24
			}
			if bits > 30 {
				continue
			}
			result = append(result, netip.PrefixFrom(ip, bits))
		}
	}
	return result
}

func (s *lanScanner) view() scanStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.status
	if status.Running && status.Phase == "ports" {
		status.Done = s.swept.Load()
	}
	if status.Results == nil {
		status.Results = []scanResult{}
	}
	return status
}

func (s *lanScanner) start(portStart, portEnd int) error {
	if portStart < 1 || portEnd > 65535 || portStart > portEnd {
		return errors.New("invalid_port_range")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.Running {
		return errors.New("scan_in_progress")
	}
	now := time.Now()
	s.status = scanStatus{Running: true, Phase: "discover", PortStart: portStart, PortEnd: portEnd, StartedAt: &now}
	s.swept.Store(0)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	s.cancel = cancel
	done := make(chan struct{})
	s.done = done
	go func() {
		defer close(done)
		defer cancel()
		results, err := s.run(ctx, portStart, portEnd)
		s.mu.Lock()
		defer s.mu.Unlock()
		finished := time.Now()
		s.status.Running, s.status.FinishedAt, s.status.Results = false, &finished, results
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			s.status.Phase = "canceled"
		case err != nil:
			s.status.Phase, s.status.Error = "failed", err.Error()
		default:
			s.status.Phase = "done"
		}
	}()
	return nil
}

// stop cancels a running scan and waits until it has finished.
func (s *lanScanner) stop() {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (s *lanScanner) setPhase(phase string, total int64) {
	s.mu.Lock()
	s.status.Phase, s.status.Total, s.status.Done = phase, total, 0
	s.mu.Unlock()
}

func (s *lanScanner) run(ctx context.Context, portStart, portEnd int) ([]scanResult, error) {
	hosts, self := s.hosts()
	s.mu.Lock()
	s.status.Hosts = len(hosts)
	s.mu.Unlock()
	found := newDiscovery()
	if s.discover != nil {
		found = s.discover(ctx, hosts)
	}
	live := []netip.Addr{}
	for _, h := range hosts {
		if h.IsLoopback() || self[h] || found.live[h] {
			live = append(live, h)
		}
	}
	ports := s.ports
	if ports == nil {
		for p := portStart; p <= portEnd; p++ {
			ports = append(ports, p)
		}
	}
	s.mu.Lock()
	s.status.Live = len(live)
	s.mu.Unlock()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	s.setPhase("ports", int64(len(live)*len(ports)))
	var quicOpen map[netip.Addr][]int
	var wait sync.WaitGroup
	if s.udpRate > 0 {
		wait.Add(1)
		go func() { defer wait.Done(); quicOpen = quicSweep(ctx, live, ports, s.udpRate) }()
	}
	open := s.tcpSweep(ctx, live, ports)
	wait.Wait()
	// Services announced by broadcast discovery may sit outside the range.
	for _, hint := range found.hints {
		addr, err := netip.ParseAddr(hint.Host)
		known := err == nil && (addr.IsLoopback() || self[addr] || found.live[addr])
		if !known || hint.Proto != "tcp" || containsInt(open[addr], hint.Port) || !s.reachable(ctx, hint.Host, hint.Port) {
			continue
		}
		open[addr] = append(open[addr], hint.Port)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	type target struct {
		host  netip.Addr
		port  int
		proto string
	}
	targets := []target{}
	for h, list := range open {
		for _, p := range list {
			targets = append(targets, target{h, p, "tcp"})
		}
	}
	for h, list := range quicOpen {
		for _, p := range list {
			targets = append(targets, target{h, p, "udp"})
		}
	}
	s.setPhase("identify", int64(len(targets)))
	results := make([]scanResult, len(targets))
	jobs := make(chan int)
	for i := 0; i < 32; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range jobs {
				t := targets[index]
				host := t.host.String()
				if t.proto == "udp" {
					results[index] = probeQUIC(ctx, host, t.port)
				} else {
					results[index] = fingerprintTCP(ctx, host, t.port, found.hints[scanKey(host, "tcp", t.port)])
				}
				s.mu.Lock()
				s.status.Done++
				s.mu.Unlock()
			}
		}()
	}
feed:
	for i := range targets {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wait.Wait()
	return finishResults(results, found, self), nil
}

func (s *lanScanner) reachable(ctx context.Context, host string, port int) bool {
	dialer := net.Dialer{Timeout: s.dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err == nil {
		conn.Close()
	}
	return err == nil
}

// tcpSweep connect-scans every port of every live host. Ports are the outer
// loop so the load is spread across hosts.
func (s *lanScanner) tcpSweep(ctx context.Context, hosts []netip.Addr, ports []int) map[netip.Addr][]int {
	type job struct {
		host netip.Addr
		port int
	}
	jobs := make(chan job, s.workers)
	open := map[netip.Addr][]int{}
	var mu sync.Mutex
	var wait sync.WaitGroup
	for i := 0; i < s.workers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			dialer := net.Dialer{Timeout: s.dialTimeout}
			for j := range jobs {
				address := net.JoinHostPort(j.host.String(), strconv.Itoa(j.port))
				for attempt := 0; attempt < 5 && ctx.Err() == nil; attempt++ {
					conn, err := dialer.DialContext(ctx, "tcp", address)
					if err == nil {
						conn.Close()
						mu.Lock()
						open[j.host] = append(open[j.host], j.port)
						mu.Unlock()
						break
					}
					// Out of descriptors or local ports: back off and retry.
					if !errors.Is(err, syscall.EMFILE) && !errors.Is(err, syscall.ENFILE) && !errors.Is(err, syscall.EADDRNOTAVAIL) && !errors.Is(err, syscall.ENOBUFS) {
						break
					}
					time.Sleep(50 * time.Millisecond)
				}
				s.swept.Add(1)
			}
		}()
	}
feed:
	for _, p := range ports {
		for _, h := range hosts {
			select {
			case jobs <- job{h, p}:
			case <-ctx.Done():
				break feed
			}
		}
	}
	close(jobs)
	wait.Wait()
	return open
}

var scanTypeOrder = map[string]int{"jellyfin": 0, "emby": 1, "plex": 2, "smb": 3, "nfs": 4, "webdav": 5, "dlna": 6, "ftp": 7, "ftps": 8, "sftp": 9, "https": 10, "http": 11, "h3": 12, "other": 20}

// Ports of the Connector itself on this machine are never offered.
var connectorOwnPorts = map[int]bool{9080: true, 9082: true, 1080: true, 1082: true}

func finishResults(results []scanResult, found *discovery, self map[netip.Addr]bool) []scanResult {
	local := map[string]bool{}
	for _, r := range results {
		if r.Host == "127.0.0.1" {
			local[r.Proto+strconv.Itoa(r.Port)] = true
		}
	}
	kept := []scanResult{}
	for _, r := range results {
		addr, err := netip.ParseAddr(r.Host)
		if err != nil || r.Type == "" {
			continue
		}
		// A service on all interfaces shows up on loopback and the LAN
		// address; keep the loopback entry.
		if self[addr] && local[r.Proto+strconv.Itoa(r.Port)] {
			continue
		}
		if (addr.IsLoopback() || self[addr]) && connectorOwnPorts[r.Port] {
			continue
		}
		if r.HostName == "" {
			r.HostName = found.names[addr]
		}
		r.Service = platformServiceType(r)
		kept = append(kept, r)
	}
	sort.Slice(kept, func(i, j int) bool {
		a, b := kept[i], kept[j]
		if scanTypeOrder[a.Type] != scanTypeOrder[b.Type] {
			return scanTypeOrder[a.Type] < scanTypeOrder[b.Type]
		}
		x, _ := netip.ParseAddr(a.Host)
		y, _ := netip.ParseAddr(b.Host)
		if x != y {
			return x.Less(y)
		}
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		return a.Proto < b.Proto
	})
	if len(kept) > scanResultLimit {
		kept = kept[:scanResultLimit]
	}
	return kept
}

// platformServiceType maps a result to a platform service type. FTP needs a
// range of passive data ports and DLNA advertises LAN URLs, so neither works
// through a single TCP mapping; UDP is not forwarded at all.
func platformServiceType(r scanResult) string {
	if r.Proto != "tcp" {
		return ""
	}
	kind := "other_tcp"
	switch r.Type {
	case "smb", "webdav", "jellyfin", "emby":
		kind = r.Type
	case "ftp", "ftps", "dlna":
		return ""
	}
	if validPlatformTarget("scan", kind, r.Host, r.Port) != nil {
		return ""
	}
	return kind
}
