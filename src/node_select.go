package main

// Relay node selection is automatic: the Connector measures TCP connect time
// from this machine to every enabled node and uses the fastest reachable one.
// The choice is sticky while the node stays listed and reachable, so a healthy
// tunnel is never torn down to chase a few milliseconds. Devices follow the
// node the Connector reports to the platform instead of choosing their own.
import (
	"context"
	"log"
	"net"
	"strconv"
	"sync"
	"time"
)

const (
	nodeProbeInterval  = 30 * time.Second
	nodeProbeTimeout   = 3 * time.Second
	nodeProbeSamples   = 3
	nodeFailureLimit   = 3 // consecutive failed probes of the current node before switching
	nodeReportInterval = 10 * time.Minute
)

type nodeHealth struct {
	RTT      time.Duration
	OK       bool
	Failures int
	At       time.Time
}

// probeTCP returns the fastest of a few TCP connects to host:port.
func probeTCP(ctx context.Context, host string, port int) (time.Duration, error) {
	address := net.JoinHostPort(host, strconv.Itoa(port))
	var best time.Duration
	var last error
	dialer := net.Dialer{Timeout: nodeProbeTimeout}
	for i := 0; i < nodeProbeSamples; i++ {
		start := time.Now()
		connection, err := dialer.DialContext(ctx, "tcp", address)
		if err != nil {
			last = err
			continue
		}
		elapsed := time.Since(start)
		connection.Close()
		if best == 0 || elapsed < best {
			best = elapsed
		}
	}
	if best == 0 {
		return 0, last
	}
	return best, nil
}

func nodeAddress(node map[string]any) (string, int, bool) {
	host := stringValue(node["host"])
	number, ok := intValue(node["port"])
	return host, int(number), ok && host != "" && number > 0 && number < 65536
}

// Caller holds p.mu. Records one probe result per node.
func (p *connectorPlatform) probeNodes(ctx context.Context, nodes []map[string]any) {
	type result struct {
		id  string
		rtt time.Duration
		err error
	}
	results := make(chan result, len(nodes))
	var wait sync.WaitGroup
	for _, node := range nodes {
		id := stringValue(node["nodeId"])
		host, port, ok := nodeAddress(node)
		if !ok {
			continue
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			rtt, err := p.probe(ctx, host, port)
			results <- result{id, rtt, err}
		}()
	}
	wait.Wait()
	close(results)
	if p.health == nil {
		p.health = map[string]*nodeHealth{}
	}
	now := time.Now()
	for r := range results {
		h := p.health[r.id]
		if h == nil {
			h = &nodeHealth{}
			p.health[r.id] = h
		}
		h.At = now
		if r.err != nil {
			h.OK = false
			h.Failures++
			continue
		}
		h.OK, h.RTT, h.Failures = true, r.rtt, 0
	}
}

func profileNodes(profile obj) []map[string]any {
	items, _ := profile["nodes"].([]any)
	nodes := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if node, ok := item.(map[string]any); ok && stringValue(node["nodeId"]) != "" {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

// Caller holds p.mu. Keeps the current node while it is listed and has not
// failed nodeFailureLimit probes in a row; otherwise switches to the reachable
// node with the lowest connect time. With nothing reachable it keeps a listed
// current node, or falls back to the first listed node so the tunnel comes up
// as soon as that node does. Returns true when the selection changed.
func (p *connectorPlatform) ensureNode(ctx context.Context, profile obj) (bool, error) {
	nodes := profileNodes(profile)
	if len(nodes) == 0 {
		return false, nil
	}
	listed := false
	for _, node := range nodes {
		if stringValue(node["nodeId"]) == p.state.NodeID {
			listed = true
		}
	}
	due := time.Since(p.lastProbe) >= nodeProbeInterval
	if listed && !due {
		return false, nil
	}
	if listed {
		// Routine health check: only the current node.
		for _, node := range nodes {
			if stringValue(node["nodeId"]) == p.state.NodeID {
				p.probeNodes(ctx, []map[string]any{node})
			}
		}
		p.lastProbe = time.Now()
		if h := p.health[p.state.NodeID]; h == nil || h.Failures < nodeFailureLimit {
			return false, nil
		}
	}
	p.probeNodes(ctx, nodes)
	p.lastProbe = time.Now()
	best := ""
	var bestRTT time.Duration
	for _, node := range nodes {
		id := stringValue(node["nodeId"])
		if h := p.health[id]; h != nil && h.OK && (best == "" || h.RTT < bestRTT) {
			best, bestRTT = id, h.RTT
		}
	}
	if best == "" {
		if listed {
			return false, nil
		}
		best = stringValue(nodes[0]["nodeId"])
	}
	if best == p.state.NodeID {
		return false, nil
	}
	previous := p.state.NodeID
	p.state.NodeID = best
	if err := p.save(); err != nil {
		p.state.NodeID = previous
		return false, err
	}
	if previous == "" {
		log.Printf("relay node selected: %s (%v)", best, bestRTT.Round(time.Millisecond))
	} else {
		log.Printf("relay node switched: %s -> %s (%v)", previous, best, bestRTT.Round(time.Millisecond))
	}
	return true, nil
}

// Caller holds p.mu. Tells the platform which node carries this Connector's
// services so devices can follow it. Failures are retried on the next sync and
// never stop the running tunnel.
func (p *connectorPlatform) reportNode(ctx context.Context) {
	if p.state.NodeID == "" || p.planHash == "" {
		return
	}
	if p.reportedNode == p.state.NodeID && time.Since(p.reportedAt) < nodeReportInterval {
		return
	}
	body := obj{"nodeId": p.state.NodeID}
	if h := p.health[p.state.NodeID]; h != nil && h.OK {
		body["rttMs"] = h.RTT.Milliseconds()
	}
	if _, _, err := p.requestAs(ctx, "PUT", "/connector/node", p.state.MachineToken, body, "", ""); err != nil {
		if p.reportFault != err.Error() {
			log.Printf("relay node report failed: %v", err)
		}
		p.reportFault = err.Error()
		return
	}
	p.reportedNode, p.reportedAt, p.reportFault = p.state.NodeID, time.Now(), ""
}

// Caller holds p.mu. Public, secret-free latency view for the manager page.
func (p *connectorPlatform) nodeLatencies() obj {
	result := obj{}
	for id, h := range p.health {
		entry := obj{"ok": h.OK, "failures": h.Failures, "checkedAt": h.At}
		if h.OK {
			entry["rttMs"] = h.RTT.Milliseconds()
		}
		result[id] = entry
	}
	return result
}
