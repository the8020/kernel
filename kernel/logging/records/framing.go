package records

import (
	"encoding/binary"
	"errors"
	"io"
)

func WriteAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if n < 0 || n > len(p) {
			return errors.New("invalid log write count")
		}
		p = p[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func WriteFrame(w io.Writer, payload []byte) error {
	return writeFrame(w, payload, MaxFrame)
}

// Control replies can contain one bounded query page. This larger bound is never
// used for producer log frames or unauthenticated handshakes.
const MaxControlFrame = MaxQueryBytes + 8192

func WriteControlFrame(w io.Writer, payload []byte) error {
	return writeFrame(w, payload, MaxControlFrame)
}

func writeFrame(w io.Writer, payload []byte, limit int) error {
	if len(payload) == 0 || len(payload) > limit {
		return errors.New("invalid log frame size")
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(payload)))
	if err := WriteAll(w, size[:]); err != nil {
		return err
	}
	return WriteAll(w, payload)
}

func ReadFrame(r io.Reader) ([]byte, error) {
	return readFrame(r, MaxFrame)
}

func ReadControlFrame(r io.Reader) ([]byte, error) { return readFrame(r, MaxControlFrame) }

func readFrame(r io.Reader, limit uint32) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > limit {
		return nil, errors.New("invalid log frame size")
	}
	payload := make([]byte, int(size))
	_, err := io.ReadFull(r, payload)
	return payload, err
}
