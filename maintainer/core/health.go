package core

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

// A2SInfo is the subset of the A2S_INFO reply the maintainer uses.
type A2SInfo struct {
	Name       string
	Map        string
	Folder     string
	Game       string
	Version    string
	Players    int
	MaxPlayers int
	Bots       int
}

// Humans is the player count without bots. Source counts bots as players.
func (i A2SInfo) Humans() int { return max(i.Players-i.Bots, 0) }

var (
	ErrA2SShort = errors.New("a2s: truncated response")
	ErrA2SSplit = errors.New("a2s: split responses are not supported")
)

func A2SInfoRequest(challenge []byte) []byte {
	return append([]byte("\xFF\xFF\xFF\xFFTSource Engine Query\x00"), challenge...)
}

// QueryA2SInfo sends A2S_INFO, answering the challenge Valve added in 2020 if the
// server asks for one. The context deadline bounds the whole query.
func QueryA2SInfo(ctx context.Context, addr string) (A2SInfo, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		return A2SInfo{}, err
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(3 * time.Second)
	}
	conn.SetDeadline(deadline)

	buf := make([]byte, 4096)
	var challenge []byte
	for range 3 {
		if _, err := conn.Write(A2SInfoRequest(challenge)); err != nil {
			return A2SInfo{}, err
		}
		n, err := conn.Read(buf)
		if err != nil {
			return A2SInfo{}, err
		}
		resp := buf[:n]
		if n < 5 {
			return A2SInfo{}, ErrA2SShort
		}
		switch int32(binary.LittleEndian.Uint32(resp)) {
		case -1:
		case -2:
			return A2SInfo{}, ErrA2SSplit
		default:
			return A2SInfo{}, errors.New("a2s: bad packet header")
		}
		switch resp[4] {
		case 'A': // S2C_CHALLENGE
			if n < 9 {
				return A2SInfo{}, ErrA2SShort
			}
			challenge = append(challenge[:0], resp[5:9]...)
		case 'I':
			return ParseA2SInfo(resp[5:])
		default:
			return A2SInfo{}, fmt.Errorf("a2s: unexpected response type 0x%02x", resp[4])
		}
	}
	return A2SInfo{}, errors.New("a2s: server kept answering with challenges")
}

func ParseA2SInfo(b []byte) (A2SInfo, error) {
	r := byteReader{b: b}
	var info A2SInfo
	r.u8() // protocol
	info.Name = r.str()
	info.Map = r.str()
	info.Folder = r.str()
	info.Game = r.str()
	r.u16() // app id
	info.Players = int(r.u8())
	info.MaxPlayers = int(r.u8())
	info.Bots = int(r.u8())
	r.u8() // server type
	r.u8() // environment
	r.u8() // visibility
	r.u8() // VAC
	info.Version = r.str()
	if r.err != nil {
		return A2SInfo{}, r.err
	}
	return info, nil
}

type byteReader struct {
	b   []byte
	err error
}

func (r *byteReader) u8() byte {
	if r.err != nil || len(r.b) < 1 {
		r.err = ErrA2SShort
		return 0
	}
	v := r.b[0]
	r.b = r.b[1:]
	return v
}

func (r *byteReader) u16() uint16 {
	if r.err != nil || len(r.b) < 2 {
		r.err = ErrA2SShort
		return 0
	}
	v := binary.LittleEndian.Uint16(r.b)
	r.b = r.b[2:]
	return v
}

func (r *byteReader) str() string {
	if r.err != nil {
		return ""
	}
	i := bytes.IndexByte(r.b, 0)
	if i < 0 {
		r.err = ErrA2SShort
		return ""
	}
	s := string(r.b[:i])
	r.b = r.b[i+1:]
	return s
}
