package tests

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"

	"cs2-maintainer/core"
)

// fakeA2SServer answers like a post-2020 Source server: a challenge first, then
// the info reply only for requests that echo the challenge.
func fakeA2SServer(t *testing.T, reply []byte) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	challenge := []byte{0x11, 0x22, 0x33, 0x44}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			req := buf[:n]
			if !bytes.HasPrefix(req, core.A2SInfoRequest(nil)) {
				continue
			}
			if bytes.Equal(req[len(req)-4:], challenge) {
				conn.WriteTo(reply, addr)
			} else {
				conn.WriteTo(append([]byte{0xFF, 0xFF, 0xFF, 0xFF, 'A'}, challenge...), addr)
			}
		}
	}()
	return conn.LocalAddr().String()
}

func TestQueryA2SInfoWithChallenge(t *testing.T) {
	addr := fakeA2SServer(t, a2sInfoReply(5, 12, 2))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	info, err := core.QueryA2SInfo(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "FLICKED test" || info.Map != "de_mirage" || info.MaxPlayers != 12 || info.Version != "1.40.9.8" {
		t.Errorf("unexpected info %+v", info)
	}
	if info.Humans() != 3 {
		t.Errorf("Humans() = %d, want 3 (5 players minus 2 bots)", info.Humans())
	}
}

func TestQueryA2SInfoTimesOut(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0") // never answers
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := core.QueryA2SInfo(ctx, conn.LocalAddr().String()); err == nil {
		t.Fatal("expected a timeout")
	}
	if time.Since(start) > time.Second {
		t.Error("query ignored the context deadline")
	}
}

func TestParseA2SInfoTruncated(t *testing.T) {
	full := a2sInfoReply(1, 10, 0)[5:]
	if _, err := core.ParseA2SInfo(full[:10]); !errors.Is(err, core.ErrA2SShort) {
		t.Errorf("err = %v, want ErrA2SShort", err)
	}
}

// fakeRCONServer checks the password and reports each command it receives.
func fakeRCONServer(t *testing.T, password string, got chan<- string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				id, _, body, err := core.ReadRCONPacket(r)
				if err != nil {
					return
				}
				core.WriteRCONPacket(conn, id, core.RCONTypeResponse, "")
				if body != password {
					core.WriteRCONPacket(conn, -1, core.RCONTypeAuthReply, "")
					return
				}
				core.WriteRCONPacket(conn, id, core.RCONTypeAuthReply, "")
				id, _, cmd, err := core.ReadRCONPacket(r)
				if err != nil {
					return
				}
				got <- cmd
				if cmd == "quit" {
					return // the real server just goes away
				}
				core.WriteRCONPacket(conn, id, core.RCONTypeResponse, "ok: "+cmd)
			}()
		}
	}()
	return ln.Addr().String()
}

func TestRCONExec(t *testing.T) {
	got := make(chan string, 4)
	addr := fakeRCONServer(t, "secret", got)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := core.RCONExec(ctx, addr, "secret", "say hello")
	if err != nil || resp != "ok: say hello" {
		t.Fatalf("resp=%q err=%v", resp, err)
	}
	if cmd := <-got; cmd != "say hello" {
		t.Errorf("server got %q", cmd)
	}
	if _, err := core.RCONExec(ctx, addr, "nope", "status"); err == nil || err.Error() != "rcon: wrong password" {
		t.Errorf("wrong password: err = %v", err)
	}
	if _, err := core.RCONExec(ctx, addr, "secret", "quit"); !errors.Is(err, core.ErrRCONNoResponse) {
		t.Errorf("quit: err = %v, want ErrRCONNoResponse", err)
	}
}

func TestRCONPacketRoundTrip(t *testing.T) {
	var b bytes.Buffer
	if err := core.WriteRCONPacket(&b, 7, core.RCONTypeExec, "status"); err != nil {
		t.Fatal(err)
	}
	if size := binary.LittleEndian.Uint32(b.Bytes()); size != uint32(len("status")+10) {
		t.Errorf("size field = %d", size)
	}
	id, typ, body, err := core.ReadRCONPacket(&b)
	if err != nil || id != 7 || typ != core.RCONTypeExec || body != "status" {
		t.Errorf("got id=%d typ=%d body=%q err=%v", id, typ, body, err)
	}
}
