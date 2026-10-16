package compass

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// bedrockMagic is the documented Bedrock offline-message magic, independent of this
// implementation: 00 FF FF 00 FE FE FE FE FD FD FD FD 12 34 56 78. The classic RakNet
// magic (all-FF alternating) is silently dropped by BDS 1.26.x — verified live on prod
// 2026-09-18, where the classic ping timed out while this format answered in ~1 ms.
var bedrockMagic = []byte{
	0x00, 0xff, 0xff, 0x00, 0xfe, 0xfe, 0xfe, 0xfe,
	0xfd, 0xfd, 0xfd, 0xfd, 0x12, 0x34, 0x56, 0x78,
}

// The liveness probe is only correct if it sends the format current BDS answers: an
// UnconnectedPing of id 0x01 + time(8) + magic(16) + GUID(8).
func TestPongRequest_IsTheBedrockFormat(t *testing.T) {
	pkt := pongRequest()
	if len(pkt) != 33 {
		t.Fatalf("packet = %d bytes, want 33 (id + time + magic + guid)", len(pkt))
	}
	if pkt[0] != 0x01 {
		t.Errorf("packet id = %#02x, want 0x01 (ID_UNCONNECTED_PING)", pkt[0])
	}
	if !bytes.Equal(pkt[9:25], bedrockMagic) {
		t.Errorf("magic = % x, want the Bedrock value % x", pkt[9:25], bedrockMagic)
	}
	if bytes.Equal(pkt[25:33], make([]byte, 8)) {
		t.Error("client GUID must be a value, not eight zero bytes")
	}
}

// samplePong is the live-captured 1.26 advertisement from research/04.
const samplePong = "MCPE;TestServer;2168;1.26.43;0;15;11741566103384811254;Bedrock level;Survival;1;19132;19133;0;1;0;"

func TestParsePong_CapturedSample(t *testing.T) {
	p, err := ParsePong(samplePong)
	if err != nil {
		t.Fatalf("ParsePong: %v", err)
	}
	if p.Name != "TestServer" {
		t.Errorf("Name = %q", p.Name)
	}
	if p.Version != "1.26.43" {
		t.Errorf("Version = %q", p.Version)
	}
	if p.MOTD != "Bedrock level" {
		t.Errorf("MOTD = %q", p.MOTD)
	}
	if p.MaxPlayers != 15 {
		t.Errorf("MaxPlayers = %d, want 15", p.MaxPlayers)
	}
	if p.Online != 0 {
		t.Errorf("Online = %d, want 0", p.Online)
	}
	if p.GUID != "11741566103384811254" {
		t.Errorf("GUID = %q", p.GUID)
	}
	if p.GamePort != 19132 || p.QueryPort != 19133 {
		t.Errorf("ports = %d/%d", p.GamePort, p.QueryPort)
	}
}

func TestParsePong_NonZeroLiveCount(t *testing.T) {
	// Same shape, but five players online after the two ports.
	s := "MCPE;World;2168;1.26.43;0;20;999;MOTD;Sub;1;19132;19133;5;1;0;"
	p, err := ParsePong(s)
	if err != nil {
		t.Fatalf("ParsePong: %v", err)
	}
	if p.Online != 5 {
		t.Errorf("Online = %d, want 5", p.Online)
	}
	if p.MaxPlayers != 20 {
		t.Errorf("MaxPlayers = %d, want 20", p.MaxPlayers)
	}
}

func TestParsePong_RejectsNonMCPE(t *testing.T) {
	if _, err := ParsePong("java;some;other;layout;here;now;at;all;right;"); err == nil {
		t.Error("non-MCPE payload must be rejected")
	}
	if _, err := ParsePong(""); err == nil {
		t.Error("empty payload must be rejected")
	}
	if _, err := ParsePong("MCPE;only;two"); err == nil {
		t.Error("too-short payload must be rejected")
	}
}

func TestExtractMOTD(t *testing.T) {
	// A plausible UnconnectedPong reply: fixed header bytes, then the
	// null-terminated MOTD, then trailing counters.
	raw := []byte{0x01, 0, 0, 0, 0xAA, 0xBB, 0, 0, 'R', 'a', 'k', 'N', 'e', 't',
		0x88, 0, 0, 0, 0, 0, 0, 0, 10, 0, 0, 0, 130, 75, 0, 75}
	raw = append(raw, []byte(samplePong)...)
	raw = append(raw, 0, 0, 0, 0) // null terminator + trailing
	got, err := extractMOTD(raw)
	if err != nil {
		t.Fatalf("extractMOTD: %v", err)
	}
	if got != strings.TrimSuffix(samplePong, ";") && got != samplePong {
		t.Errorf("MOTD = %q", got)
	}
	if !strings.HasPrefix(got, "MCPE;TestServer") {
		t.Errorf("MOTD should start with MCPE;TestServer, got %q", got)
	}
}

// Current BDS sends the advertisement as a length-prefixed string with no null
// terminator (reply shape live-captured from prod 2026-10-16).
func TestExtractMOTD_LengthDelimitedReply(t *testing.T) {
	ad := "MCPE;TestWorld;2193;1.26.52;1;15;123;Bedrock level;Survival;1;19132;19133;0;1;0;"
	// id 0x1c + time(8) + magic(16) + address field, then the string length (80) and ad.
	head := []byte{0x1c, 0x43, 0xf1, 0xd1, 0x6a, 0, 0, 0, 0, 0x8a, 0x89, 0xf5, 0x59,
		0xf6, 0x5b, 0x0b, 0x97, 0x00, 0xff, 0xff, 0x00, 0xfe, 0xfe, 0xfe, 0xfe,
		0xfd, 0xfd, 0xfd, 0xfd, 0x12, 0x34, 0x56, 0x78, 0x00}
	raw := append(append(head, 80), []byte(ad)...)
	got, err := extractMOTD(raw)
	if err != nil {
		t.Fatalf("extractMOTD on a length-prefixed reply: %v", err)
	}
	if got != ad {
		t.Errorf("MOTD = %q, want the whole advertisement %q", got, ad)
	}
}

func TestExtractMOTD_NoMarker(t *testing.T) {
	if _, err := extractMOTD([]byte("no marker here")); err == nil {
		t.Error("reply without MCPE marker must error")
	}
}

// TestPinger_ParsesReply drives the Pinger through a fake transport that
// returns a canned reply, verifying the send -> extract -> parse path.
func TestPinger_ParsesReply(t *testing.T) {
	ping := &Pinger{
		Addr: "127.0.0.1:19132",
		exchange: func(ctx context.Context, addr string, req []byte) ([]byte, error) {
			if len(req) != 33 || !bytes.Equal(req[9:25], bedrockMagic) {
				t.Errorf("request = % x, want the Bedrock UnconnectedPing (33 bytes)", req)
			}
			raw := make([]byte, 34)
			raw = append(raw, []byte(samplePong)...)
			raw = append(raw, 0)
			return raw, nil
		},
	}
	p, err := ping.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if p.Name != "TestServer" || p.MaxPlayers != 15 || p.Online != 0 {
		t.Errorf("got %+v", p)
	}
}

func TestPinger_TransportError(t *testing.T) {
	ping := &Pinger{
		Addr: "127.0.0.1:1",
		exchange: func(ctx context.Context, addr string, req []byte) ([]byte, error) {
			return nil, context.DeadlineExceeded
		},
	}
	if _, err := ping.Ping(context.Background()); err == nil {
		t.Error("transport error must propagate")
	}
}
