// Package fallback generates static-image video streams used as placeholders
// when a battery/wifi camera is sleeping.
package fallback

import (
	"bufio"
	"io"
)

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

// StreamNALUs reads a continuous Annex-B byte stream from r and sends
// each NAL unit (without start codes) to the returned channel. The
// channel is closed when r returns an error or reaches EOF.
func StreamNALUs(r io.Reader) <-chan []byte {
	ch := make(chan []byte, 16)
	go func() {
		defer close(ch)
		br := bufio.NewReaderSize(r, 64*1024)
		var nalu []byte
		zeroCount := 0
		for {
			b, err := br.ReadByte()
			if err != nil {
				for i := 0; i < zeroCount; i++ {
					nalu = append(nalu, 0)
				}
				if len(nalu) > 0 {
					ch <- nalu
				}
				return
			}
			if b == 0 {
				zeroCount++
				continue
			}
			if b == 1 && zeroCount >= 2 {
				if len(nalu) > 0 {
					ch <- nalu
					nalu = nil
				}
				zeroCount = 0
				continue
			}
			for i := 0; i < zeroCount; i++ {
				nalu = append(nalu, 0)
			}
			zeroCount = 0
			nalu = append(nalu, b)
		}
	}()
	return ch
}

// IsH264VCL reports whether nalu is an H.264 VCL NAL unit (types 1–5).
func IsH264VCL(nalu []byte) bool {
	if len(nalu) == 0 {
		return false
	}
	t := nalu[0] & 0x1F
	return t >= 1 && t <= 5
}

// IsH265VCL reports whether nalu is an H.265 VCL NAL unit (types 0–31).
func IsH265VCL(nalu []byte) bool {
	if len(nalu) < 2 {
		return false
	}
	t := (nalu[0] >> 1) & 0x3F
	return t <= 31
}
