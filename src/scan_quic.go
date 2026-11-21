package main

// HTTP/3 discovery. A QUIC server answers a long-header packet carrying an
// unknown (reserved) version with a Version Negotiation packet, without
// needing a valid handshake. That makes a cheap UDP sweep possible; every
// answering port is then confirmed with a real QUIC handshake offering h3.
import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
)

// quicProbePacket builds a 1200-byte datagram (servers ignore shorter ones)
// with a reserved version of the form 0x?a?a?a?a.
func quicProbePacket(scid []byte) []byte {
	packet := make([]byte, 1200)
	packet[0] = 0xc0
	copy(packet[1:5], []byte{0x1a, 0x2a, 0x3a, 0x4a})
	packet[5] = 8
	rand.Read(packet[6:14]) // destination connection ID
	packet[14] = byte(len(scid))
	copy(packet[15:], scid)
	return packet
}

// isVersionNegotiation checks that a datagram is a Version Negotiation
// packet addressed to our source connection ID.
func isVersionNegotiation(packet, scid []byte) bool {
	if len(packet) < 7 || packet[0]&0x80 == 0 || string(packet[1:5]) != "\x00\x00\x00\x00" {
		return false
	}
	dcidLen := int(packet[5])
	return dcidLen == len(scid) && len(packet) >= 6+dcidLen && string(packet[6:6+dcidLen]) == string(scid)
}

func quicSweep(ctx context.Context, hosts []netip.Addr, ports []int, rate int) map[netip.Addr][]int {
	found := map[netip.Addr][]int{}
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return found
	}
	defer conn.Close()
	scid := make([]byte, 8)
	rand.Read(scid)
	packet := quicProbePacket(scid)
	var mu sync.Mutex
	seen := map[string]bool{}
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			addr, ok := netip.AddrFromSlice(from.IP.To4())
			if !ok || !isVersionNegotiation(buf[:n], scid) {
				continue
			}
			key := from.String()
			mu.Lock()
			if !seen[key] {
				seen[key] = true
				found[addr] = append(found[addr], from.Port)
			}
			mu.Unlock()
		}
	}()
	batch := rate / 100
	if batch < 1 {
		batch = 1
	}
	sent := 0
	tick := time.Now()
send:
	for _, port := range ports {
		for _, host := range hosts {
			if ctx.Err() != nil {
				break send
			}
			target := &net.UDPAddr{IP: net.IP(host.AsSlice()), Port: port}
			for attempt := 0; attempt < 5; attempt++ {
				if _, err := conn.WriteToUDP(packet, target); !errors.Is(err, syscall.ENOBUFS) && !errors.Is(err, syscall.EAGAIN) {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if sent++; sent%batch == 0 {
				if wait := 10*time.Millisecond - time.Since(tick); wait > 0 {
					time.Sleep(wait)
				}
				tick = time.Now()
			}
		}
	}
	select {
	case <-time.After(1500 * time.Millisecond):
	case <-ctx.Done():
	}
	conn.Close()
	<-readerDone
	mu.Lock()
	defer mu.Unlock()
	return found
}

// probeQUIC completes a QUIC handshake offering only h3.
func probeQUIC(ctx context.Context, host string, port int) scanResult {
	r := scanResult{Host: host, Port: port, Proto: "udp", Type: "other", Detail: "QUIC"}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	conn, err := quic.DialAddr(ctx, net.JoinHostPort(host, strconv.Itoa(port)), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h3"}}, &quic.Config{HandshakeIdleTimeout: 3 * time.Second})
	if err != nil {
		if strings.Contains(err.Error(), "no application protocol") || strings.Contains(err.Error(), "0x178") {
			r.Detail = "QUIC（非 HTTP/3）"
		}
		return r
	}
	defer conn.CloseWithError(0, "")
	state := conn.ConnectionState()
	if state.TLS.NegotiatedProtocol == "h3" {
		r.Type, r.Confirmed, r.Detail = "h3", true, "HTTP/3 · "+tls.VersionName(state.TLS.Version)
		if names := state.TLS.PeerCertificates; len(names) > 0 {
			r.Name = cleanLine(names[0].Subject.CommonName)
		}
	}
	return r
}
