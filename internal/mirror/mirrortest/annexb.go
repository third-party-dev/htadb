package mirrortest

// SplitAnnexB splits an H.264 Annex B stream into the codec config (SPS and
// PPS, as the mirroring server sends them in its config packet) and access
// units. A new access unit starts at an AUD or at a slice whose
// first_mb_in_slice is 0; SEI and other non-VCL units attach to the next
// access unit. IDR slices mark keyframes.
func SplitAnnexB(b []byte) (config []byte, frames []Frame) {
	type nal struct {
		data []byte // with start code
		typ  byte
	}
	var nals []nal
	start := -1
	for i := 0; i+3 <= len(b); {
		if b[i] == 0 && b[i+1] == 0 && (b[i+2] == 1 || (i+3 < len(b) && b[i+2] == 0 && b[i+3] == 1)) {
			if start >= 0 {
				nals = append(nals, nal{data: b[start:i]})
			}
			start = i
			if b[i+2] == 1 {
				i += 3
			} else {
				i += 4
			}
			continue
		}
		i++
	}
	if start >= 0 {
		nals = append(nals, nal{data: b[start:]})
	}
	var cur []byte
	curKey, curHasSlice := false, false
	flush := func() {
		if curHasSlice {
			frames = append(frames, Frame{Data: cur, Key: curKey})
		}
		cur, curKey, curHasSlice = nil, false, false
	}
	seenConfig := map[byte]bool{}
	for i := range nals {
		d := nals[i].data
		hdr := 3
		if d[2] == 0 {
			hdr = 4
		}
		if len(d) <= hdr {
			continue
		}
		typ := d[hdr] & 0x1f
		switch {
		case typ == 7 || typ == 8:
			if !seenConfig[typ] {
				config = append(config, d...)
				seenConfig[typ] = true
			}
		case typ == 9:
			flush()
		case typ == 1 || typ == 5:
			firstMB0 := len(d) > hdr+1 && d[hdr+1]&0x80 != 0
			if firstMB0 && curHasSlice {
				flush()
			}
			cur = append(cur, d...)
			curHasSlice = true
			curKey = curKey || typ == 5
		default: // SEI etc.
			if curHasSlice {
				flush()
			}
			cur = append(cur, d...)
		}
	}
	flush()
	return config, frames
}
