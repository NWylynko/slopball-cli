package boxexec

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	msgRun      = 0x01
	msgShutdown = 0x02
	msgStdin    = 0x03
	msgStdinEOF = 0x04

	msgStdout = 0x10
	msgStderr = 0x11
	msgExit   = 0x12
	msgError  = 0x13
)

func writeFrame(w io.Writer, payload []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > 16<<20 {
		return nil, fmt.Errorf("boxexec: frame too large (%d)", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func writeMsg(w io.Writer, typ byte, body []byte) error {
	payload := make([]byte, 1+len(body))
	payload[0] = typ
	copy(payload[1:], body)
	return writeFrame(w, payload)
}

func readMsg(r io.Reader) (byte, []byte, error) {
	frame, err := readFrame(r)
	if err != nil {
		return 0, nil, err
	}
	if len(frame) == 0 {
		return 0, nil, fmt.Errorf("boxexec: empty frame")
	}
	return frame[0], frame[1:], nil
}

func writeArgv(w io.Writer, argv []string) error {
	return writeMsg(w, msgRun, appendArgv(nil, argv))
}

func appendArgv(body []byte, argv []string) []byte {
	body = appendU32(body, uint32(len(argv)))
	for _, a := range argv {
		body = appendString(body, a)
	}
	return body
}

func readArgv(body []byte) ([]string, error) {
	if len(body) < 4 {
		return nil, fmt.Errorf("boxexec: truncated argv")
	}
	n := binary.BigEndian.Uint32(body[:4])
	body = body[4:]
	out := make([]string, 0, n)
	for i := uint32(0); i < n; i++ {
		s, rest, err := readString(body)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
		body = rest
	}
	if len(body) != 0 {
		return nil, fmt.Errorf("boxexec: trailing argv bytes")
	}
	return out, nil
}

func appendU32(b []byte, v uint32) []byte {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], v)
	return append(b, buf[:]...)
}

func appendString(b []byte, s string) []byte {
	b = appendU32(b, uint32(len(s)))
	return append(b, s...)
}

func readString(b []byte) (string, []byte, error) {
	if len(b) < 4 {
		return "", nil, fmt.Errorf("boxexec: truncated string")
	}
	n := binary.BigEndian.Uint32(b[:4])
	b = b[4:]
	if uint32(len(b)) < n {
		return "", nil, fmt.Errorf("boxexec: truncated string body")
	}
	s := string(b[:n])
	return s, b[n:], nil
}

func writeExit(w io.Writer, code int) error {
	var body [4]byte
	binary.BigEndian.PutUint32(body[:], uint32(code))
	return writeMsg(w, msgExit, body[:])
}

func readExit(body []byte) (int, error) {
	if len(body) < 4 {
		return -1, fmt.Errorf("boxexec: truncated exit code")
	}
	return int(binary.BigEndian.Uint32(body[:4])), nil
}
