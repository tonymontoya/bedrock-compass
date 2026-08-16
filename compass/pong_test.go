package compass

import (
	"context"
	"strings"
	"testing"
)

// samplePong is the live-captured 1.26 advertisement from research/04.
const samplePong = "MCPE;MontoyaWorld;2168;1.26.43;0;15;11741566103384811254;Bedrock level;Survival;1;19132;19133;0;1;0;"

func TestParsePong_CapturedSample(t *testing.T) {
	p, err := ParsePong(samplePong)
	if err != nil {
		t.Fatalf("ParsePong: %v", err)
	}
	if p.Name != "MontoyaWorld" {
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
	if !strings.HasPrefix(got, "MCPE;MontoyaWorld") {
		t.Errorf("MOTD should start with MCPE;MontoyaWorld, got %q", got)
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
			if len(req) != 12 {
				t.Errorf("request length = %d, want 12", len(req))
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
	if p.Name != "MontoyaWorld" || p.MaxPlayers != 15 || p.Online != 0 {
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
