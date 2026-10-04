package proxy

// G.711 helpers: some cameras send mu-law (PCMU) while our output track is
// fixed to A-law (PCMA). The conversion goes through 16-bit linear PCM, as in
// the reference ITU-T G.711 implementation.

var ulawToAlawTable [256]byte

func init() {
	for i := 0; i < 256; i++ {
		ulawToAlawTable[i] = linearToAlaw(ulawToLinear(byte(i)))
	}
}

// ulawToLinear decodes one mu-law sample to 16-bit linear PCM.
func ulawToLinear(u byte) int16 {
	u = ^u
	t := (int(u&0x0F) << 3) + 0x84
	t <<= (u & 0x70) >> 4
	if u&0x80 != 0 {
		return int16(0x84 - t)
	}
	return int16(t - 0x84)
}

// linearToAlaw encodes one 16-bit linear PCM sample as A-law.
func linearToAlaw(pcm int16) byte {
	segEnd := [8]int{0x1F, 0x3F, 0x7F, 0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF}

	v := int(pcm) >> 3
	var mask byte
	if v >= 0 {
		mask = 0xD5
	} else {
		mask = 0x55
		v = -v - 1
	}

	seg := 8
	for i, e := range segEnd {
		if v <= e {
			seg = i
			break
		}
	}
	if seg >= 8 {
		return 0x7F ^ mask
	}

	a := byte(seg << 4)
	if seg < 2 {
		a |= byte(v>>1) & 0x0F
	} else {
		a |= byte(v>>seg) & 0x0F
	}
	return a ^ mask
}

// ulawToAlaw converts a mu-law payload to A-law. It returns a new slice.
func ulawToAlaw(in []byte) []byte {
	out := make([]byte, len(in))
	for i, b := range in {
		out[i] = ulawToAlawTable[b]
	}
	return out
}
