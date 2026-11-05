package transcode

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"testing/iotest"
)

// A stopped encoder may leave only part of a Cluster in its pipe. Publishing
// any of those bytes corrupts the response; reads may split at any byte.
func TestHTTPMatroskaPublishesOnlyCompleteElements(t *testing.T) {
	header := []byte{0x1a, 0x45, 0xdf, 0xa3, 0x80, 0x18, 0x53, 0x80, 0x67, 0xff}
	cluster := []byte{0x1f, 0x43, 0xb6, 0x75, 0x83, 1, 2, 3}
	partial := []byte{0x1f, 0x43, 0xb6, 0x75, 0x84, 4, 5}
	full := append(bytes.Clone(header), cluster...)
	input := append(bytes.Clone(full), partial...)
	var output bytes.Buffer
	err := copyHTTPMatroska(&output, iotest.OneByteReader(bytes.NewReader(input)))
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("partial cluster returned %v", err)
	}
	if !bytes.Equal(output.Bytes(), full) {
		t.Fatalf("published incomplete element: %x", output.Bytes())
	}
	output.Reset()
	if err := copyHTTPMatroska(&output, bytes.NewReader(full)); err != nil || !bytes.Equal(output.Bytes(), full) {
		t.Fatalf("complete stream: %v", err)
	}
}
