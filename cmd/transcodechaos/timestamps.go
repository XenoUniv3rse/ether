package main

import "fmt"

// distortPES rewrites 33-bit PTS/DTS fields in TS PES headers, preserving
// marker bits. It deliberately leaves PCR untouched to stress demux recovery.
func distortPES(data []byte, mode string) ([]byte, error) {
	out := append([]byte(nil), data...)
	changed := 0
	for pos := 0; pos+188 <= len(out); pos += 188 {
		if pos < len(out)/3 || pos > len(out)*2/3 {
			continue
		}
		p := out[pos : pos+188]
		if p[0] != 0x47 || p[1]&0x40 == 0 || p[3]&0x10 == 0 {
			continue
		}
		off := 4
		if p[3]&0x20 != 0 {
			off += 1 + int(p[4])
		}
		if off+14 > len(p) || p[off] != 0 || p[off+1] != 0 || p[off+2] != 1 {
			continue
		}
		flags := p[off+7] >> 6
		if flags < 2 {
			continue
		}
		if mode == "backward-dts" {
			if flags != 3 || off+19 > len(p) || changed > 0 {
				continue
			}
			shiftTimestamp(p[off+14:off+19], -90000*3)
			changed++
			continue
		}
		shiftTimestamp(p[off+9:off+14], 90000*15)
		if flags == 3 && off+19 <= len(p) {
			shiftTimestamp(p[off+14:off+19], 90000*15)
		}
		changed++
	}
	if changed == 0 {
		return nil, fmt.Errorf("fixture has no suitable PES timestamps for %s", mode)
	}
	return out, nil
}
func shiftTimestamp(p []byte, delta int64) {
	v := int64(p[0]&0x0e)<<29 | int64(p[1])<<22 | int64(p[2]&0xfe)<<14 | int64(p[3])<<7 | int64(p[4]>>1)
	v = (v + delta) & ((1 << 33) - 1)
	p[0] = (p[0] & 0xf0) | byte((v>>29)&0x0e) | 1
	p[1] = byte(v >> 22)
	p[2] = byte((v>>14)&0xfe) | 1
	p[3] = byte(v >> 7)
	p[4] = byte(v<<1) | 1
}
