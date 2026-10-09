package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// ErrRCONNoResponse means the command was sent but no reply came back. For "quit"
// that is expected, since the server closes the connection as it exits.
var ErrRCONNoResponse = errors.New("rcon: command sent but no response")

const (
	RCONTypeResponse  = 0
	RCONTypeExec      = 2
	RCONTypeAuthReply = 2
	RCONTypeAuth      = 3
	rconMaxPacket     = 64 << 10
)

// RCONExec authenticates, runs one command and returns its reply.
func RCONExec(ctx context.Context, addr, password, command string) (string, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("rcon: %w", err)
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	conn.SetDeadline(deadline)
	br := bufio.NewReader(conn)

	if err := WriteRCONPacket(conn, 1, RCONTypeAuth, password); err != nil {
		return "", fmt.Errorf("rcon: sending auth: %w", err)
	}
	for {
		id, typ, _, err := ReadRCONPacket(br)
		if err != nil {
			return "", fmt.Errorf("rcon: waiting for auth: %w", err)
		}
		if typ != RCONTypeAuthReply {
			continue // servers send an empty RESPONSE_VALUE before the auth reply
		}
		if id == -1 {
			return "", errors.New("rcon: wrong password")
		}
		break
	}

	if err := WriteRCONPacket(conn, 2, RCONTypeExec, command); err != nil {
		return "", fmt.Errorf("rcon: sending command: %w", err)
	}
	for {
		id, typ, body, err := ReadRCONPacket(br)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrRCONNoResponse, err)
		}
		if id == 2 && typ == RCONTypeResponse {
			return body, nil
		}
	}
}

func WriteRCONPacket(w io.Writer, id, typ int32, body string) error {
	size := len(body) + 10
	buf := make([]byte, 4+size) // body is followed by two NULs, already zero
	binary.LittleEndian.PutUint32(buf[0:], uint32(size))
	binary.LittleEndian.PutUint32(buf[4:], uint32(id))
	binary.LittleEndian.PutUint32(buf[8:], uint32(typ))
	copy(buf[12:], body)
	_, err := w.Write(buf)
	return err
}

func ReadRCONPacket(r io.Reader) (id, typ int32, body string, err error) {
	var hdr [4]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return
	}
	size := int32(binary.LittleEndian.Uint32(hdr[:]))
	if size < 10 || size > rconMaxPacket {
		err = fmt.Errorf("bad packet size %d", size)
		return
	}
	data := make([]byte, size)
	if _, err = io.ReadFull(r, data); err != nil {
		return
	}
	id = int32(binary.LittleEndian.Uint32(data[0:]))
	typ = int32(binary.LittleEndian.Uint32(data[4:]))
	body = string(bytes.TrimRight(data[8:], "\x00"))
	return
}
