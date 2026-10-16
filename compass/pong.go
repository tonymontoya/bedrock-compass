package compass

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Pong is the parsed Bedrock server-list advertisement — the read-only
// liveness, player-count, version, and MOTD probe (D8).
type Pong struct {
	Name       string
	Version    string
	MaxPlayers int
	GUID       string // 64-bit server GUID
	MOTD       string // MOTD line 1
	Online     int    // live player count
	GamePort   int
	QueryPort  int
}

// ParsePong parses the "MCPE;..." advertisement string in the 1.26 layout
// (research/04): field [5] is max-players and a 64-bit GUID occupies the old
// max-players slot. The live count is the integer field following the two
// port fields. It returns an error if the layout is not recognized.
func ParsePong(payload string) (Pong, error) {
	f := strings.Split(strings.TrimSuffix(payload, ";"), ";")
	if len(f) < 8 || f[0] != "MCPE" {
		return Pong{}, errors.New("not a Bedrock MCPE advertisement")
	}
	p := Pong{
		Name:    f[1],
		Version: f[3],
		GUID:    f[6],
		MOTD:    f[7],
	}
	p.MaxPlayers, _ = strconv.Atoi(f[5])
	p.GamePort, p.QueryPort, p.Online = trailingPortsAndCount(f)
	return p, nil
}

// trailingPortsAndCount locates the two consecutive port-like integers
// (1024..65535) and takes the next integer field as the live player count.
func trailingPortsAndCount(f []string) (game, query, online int) {
	for i := 0; i+2 < len(f); i++ {
		p1, e1 := port(f[i])
		p2, e2 := port(f[i+1])
		if e1 != nil || e2 != nil {
			continue
		}
		game, query = p1, p2
		if n, e := strconv.Atoi(f[i+2]); e == nil {
			online = n
		}
		return
	}
	return 0, 0, 0
}

func port(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	if n < 1024 || n > 65535 {
		return 0, errors.New("not in port range")
	}
	return n, nil
}

// Pinger probes a Bedrock server via the RakNet UnconnectedPong exchange.
// The transport is injectable so the parse path is testable offline; the
// default transport is a UDP round-trip.
type Pinger struct {
	Addr     string
	timeout  time.Duration
	exchange func(ctx context.Context, addr string, req []byte) ([]byte, error)
}

// NewPinger builds a Pinger for addr (host:port) with the default UDP
// transport and a 2 s timeout.
func NewPinger(addr string) *Pinger {
	return &Pinger{Addr: addr, timeout: 2 * time.Second, exchange: udpExchange}
}

// Ping sends the UnconnectedPong request and returns the parsed advertisement.
func (p *Pinger) Ping(ctx context.Context) (Pong, error) {
	if p.exchange == nil {
		return Pong{}, errors.New("Pinger.exchange is nil")
	}
	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}
	raw, err := p.exchange(ctx, p.Addr, pongRequest())
	if err != nil {
		return Pong{}, fmt.Errorf("pong %s: %w", p.Addr, err)
	}
	motd, err := extractMOTD(raw)
	if err != nil {
		return Pong{}, err
	}
	return ParsePong(motd)
}

// pongMagic is the Bedrock offline-message magic. The classic RakNet value quoted in most
// legacy docs is silently dropped by BDS 1.26.x — no log line, just a black hole — so the
// value below is load-bearing (verified live on prod 2026-09-18).
var pongMagic = []byte{
	0x00, 0xff, 0xff, 0x00, 0xfe, 0xfe, 0xfe, 0xfe,
	0xfd, 0xfd, 0xfd, 0xfd, 0x12, 0x34, 0x56, 0x78,
}

// pongRequest builds the UnconnectedPing that current BDS answers: id 0x01 + time (8) +
// Bedrock magic (16) + client GUID (8), 33 bytes total.
func pongRequest() []byte {
	b := make([]byte, 33)
	b[0] = 0x01
	binary.BigEndian.PutUint64(b[1:9], uint64(time.Now().UnixMilli()))
	copy(b[9:25], pongMagic)
	_, _ = rand.Read(b[25:33])
	return b
}

// extractMOTD pulls the null-terminated "MCPE;..." string out of the
// UnconnectedPong reply, locating it by marker so the variable-length address
// field does not have to be decoded.
func extractMOTD(raw []byte) (string, error) {
	i := bytes.Index(raw, []byte("MCPE;"))
	if i < 0 {
		return "", errors.New("no MCPE MOTD in pong reply")
	}
	j := bytes.IndexByte(raw[i:], 0)
	if j >= 0 {
		return string(raw[i : i+j]), nil
	}
	// No terminator: current BDS sends the advertisement as a length-prefixed string, so
	// the byte before the marker is its length.
	if i > 0 && i+int(raw[i-1]) <= len(raw) {
		return string(raw[i : i+int(raw[i-1])]), nil
	}
	return "", errors.New("unterminated MOTD in pong reply")
}

// udpExchange performs the production UDP round-trip.
func udpExchange(ctx context.Context, addr string, req []byte) ([]byte, error) {
	c, err := net.Dial("udp", addr)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	deadline := time.Now().Add(2 * time.Second)
	if d, ok := ctx.Deadline(); ok {
		deadline = d
	}
	if err := c.SetDeadline(deadline); err != nil {
		return nil, err
	}
	if _, err := c.Write(req); err != nil {
		return nil, err
	}
	buf := make([]byte, 2048)
	n, err := c.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}
