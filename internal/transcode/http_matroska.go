package transcode

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

// HTTP Matroska is a stream with an unknown-size Segment. Buffer each complete
// child element before publishing it: if the encoder dies during a Cluster,
// its incomplete bytes cannot be retracted from an HTTP response.
const httpMatroskaElementLimit = 32 << 20

func readHTTPMatroskaElement(r io.Reader) (id uint64, size int64, header []byte, err error) {
	var first [1]byte
	if _, err = io.ReadFull(r, first[:]); err != nil {
		return
	}
	readVint := func(b byte, max int, strip bool) (uint64, []byte, bool, error) {
		mask, width := byte(0x80), 1
		for width <= max && b&mask == 0 {
			mask >>= 1
			width++
		}
		if width > max {
			return 0, nil, false, errors.New("invalid EBML variable integer")
		}
		buf := make([]byte, width)
		buf[0] = b
		if _, e := io.ReadFull(r, buf[1:]); e != nil {
			return 0, nil, false, e
		}
		v := uint64(b)
		if strip {
			v &= uint64(mask - 1)
		}
		for _, b := range buf[1:] {
			v = v<<8 | uint64(b)
		}
		unknown := strip && v == (uint64(1)<<(7*width))-1
		return v, buf, unknown, nil
	}
	var idBytes, sizeBytes []byte
	id, idBytes, _, err = readVint(first[0], 4, false)
	if err != nil {
		return
	}
	if _, err = io.ReadFull(r, first[:]); err != nil {
		return
	}
	var n uint64
	var unknown bool
	n, sizeBytes, unknown, err = readVint(first[0], 8, true)
	if err != nil {
		return
	}
	header = append(idBytes, sizeBytes...)
	if unknown {
		size = -1
	} else {
		size = int64(n)
	}
	return
}

func copyHTTPMatroska(w io.Writer, r io.Reader) error {
	// The first element is the EBML header, followed by the Segment master.
	id, n, header, err := readHTTPMatroskaElement(r)
	if err != nil || id != mkvEBML || n < 0 || n > httpMatroskaElementLimit {
		return errors.New("invalid Matroska header")
	}
	body := make([]byte, int(n))
	if _, err = io.ReadFull(r, body); err != nil {
		return err
	}
	if _, err = w.Write(append(header, body...)); err != nil {
		return err
	}
	id, _, header, err = readHTTPMatroskaElement(r)
	if err != nil || id != mkvSegment {
		return errors.New("missing Matroska Segment")
	}
	if _, err = w.Write(header); err != nil {
		return err
	}
	for {
		id, n, header, err = readHTTPMatroskaElement(r)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if n < 0 || n > httpMatroskaElementLimit {
			return fmt.Errorf("unsupported Matroska element %#x size %d", id, n)
		}
		var element bytes.Buffer
		element.Grow(len(header) + int(n))
		element.Write(header)
		if _, err = io.CopyN(&element, r, n); err != nil {
			return err // Never publish an incomplete element.
		}
		if _, err = element.WriteTo(w); err != nil {
			return err
		}
	}
}
