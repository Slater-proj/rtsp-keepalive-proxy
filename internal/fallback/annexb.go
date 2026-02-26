// Package fallback generates static-image video streams used as placeholders
// when a battery/wifi camera is sleeping.
package fallback

// ParseAnnexB splits an Annex-B byte stream (start-codes 0x000001 or 0x00000001)
// into individual NAL units without start codes.
func ParseAnnexB(data []byte) [][]byte {
	if len(data) < 4 {
		return nil
	}

	// Locate every start-code position.
	var positions []int
	for i := 0; i < len(data)-3; i++ {
		if data[i] == 0x00 && data[i+1] == 0x00 {
			if data[i+2] == 0x01 {
				positions = append(positions, i)
				i += 2
				continue
			}
			if i < len(data)-3 && data[i+2] == 0x00 && data[i+3] == 0x01 {
				positions = append(positions, i)
				i += 3
				continue
			}
		}
	}

	if len(positions) == 0 {
		return nil
	}

	nalus := make([][]byte, 0, len(positions))
	for idx, pos := range positions {
		// Determine start-code length (3 or 4 bytes).
		scLen := 3
		if pos+3 < len(data) && data[pos+2] == 0x00 && data[pos+3] == 0x01 {
			scLen = 4
		}

		start := pos + scLen
		var end int
		if idx+1 < len(positions) {
			end = positions[idx+1]
		} else {
			end = len(data)
		}

		if start < end {
			nalu := make([]byte, end-start)
			copy(nalu, data[start:end])
			nalus = append(nalus, nalu)
		}
	}

	return nalus
}
