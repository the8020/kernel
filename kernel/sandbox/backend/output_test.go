package backend

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestOutputBufferPreservesBoundedUTF8Ends(t *testing.T) {
	buffer := NewOutputBuffer(33)
	input := []byte("HEAD-" + strings.Repeat("💡", 1000) + "-TAIL")
	for _, value := range input {
		_, _ = buffer.Write([]byte{value})
	}
	output := buffer.Bytes()
	if !buffer.Truncated() || buffer.Total() != uint64(len(input)) || len(output) > 33+64 || !utf8.Valid(output) || !bytes.HasPrefix(output, []byte("HEAD-")) || !bytes.HasSuffix(output, []byte("-TAIL")) || !bytes.Contains(output, []byte("bytes omitted")) {
		t.Fatalf("invalid bounded diagnostic (%d bytes): %q", len(output), output)
	}
}
