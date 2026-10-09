package tests

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"strings"
	"sync"
	"testing"
	"time"

	"cs2-maintainer/core"
)

// TestMain lets the test binary double as a fake CS2 server (see runFakeServer),
// so the real launch, health check and stop paths run against a real process.
func TestMain(m *testing.M) {
	if os.Getenv("CS2M_FAKE_SERVER") == "1" {
		os.Exit(runFakeServer(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// a2sInfoReply builds an A2S_INFO response packet.
func a2sInfoReply(players, maxPlayers, bots byte) []byte {
	var b bytes.Buffer
	b.Write([]byte{0xFF, 0xFF, 0xFF, 0xFF, 'I', 17})
	for _, s := range []string{"FLICKED test", "de_mirage", "csgo", "Counter-Strike 2"} {
		b.WriteString(s)
		b.WriteByte(0)
	}
	b.Write([]byte{0xDA, 0x02}) // app id 730
	b.Write([]byte{players, maxPlayers, bots, 'd', 'l', 0, 1})
	b.WriteString("1.40.9.8\x00")
	return b.Bytes()
}

// webhookRecorder is a fake Discord webhook that records embed titles.
type webhookRecorder struct {
	*httptest.Server
	mu     sync.Mutex
	titles []string
}

func newWebhookRecorder(t *testing.T) *webhookRecorder {
	rec := &webhookRecorder{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Embeds []struct {
				Title string `json:"title"`
			} `json:"embeds"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		rec.mu.Lock()
		for _, e := range body.Embeds {
			rec.titles = append(rec.titles, e.Title)
		}
		rec.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(rec.Close)
	return rec
}

func (r *webhookRecorder) has(title string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.titles {
		if t == title {
			return true
		}
	}
	return false
}

func (r *webhookRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.titles...)
}

func argAfter(args []string, name string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

// runFakeServer answers A2S_INFO on UDP and RCON on TCP using the port and
// password from its launch args, and exits on RCON "quit" or SIGINT.
func runFakeServer(args []string) int {
	port := argAfter(args, "-port")
	password := argAfter(args, "+rcon_password")
	quit := make(chan struct{})
	var once sync.Once

	udp, err := net.ListenPacket("udp", "127.0.0.1:"+port)
	if err != nil {
		fmt.Println("listen udp:", err)
		return 1
	}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := udp.ReadFrom(buf)
			if err != nil {
				return
			}
			if strings.Contains(string(buf[:n]), "Source Engine Query") {
				udp.WriteTo(a2sInfoReply(0, 10, 0), addr)
			}
		}
	}()

	tcp, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		fmt.Println("listen tcp:", err)
		return 1
	}
	go func() {
		for {
			conn, err := tcp.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				id, _, body, err := core.ReadRCONPacket(r)
				if err != nil || body != password {
					core.WriteRCONPacket(conn, -1, core.RCONTypeAuthReply, "")
					return
				}
				core.WriteRCONPacket(conn, id, core.RCONTypeAuthReply, "")
				if _, _, cmd, err := core.ReadRCONPacket(r); err == nil && cmd == "quit" {
					once.Do(func() { close(quit) })
				}
			}()
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	fmt.Println("fake server ready on port", port)
	select {
	case <-quit:
	case <-sig:
	}
	fmt.Println("saving and exiting")
	return 0
}
