package fallback

import (
	"bytes"
	"reflect"
	"testing"
)

func TestParseAnnexBFourByteStartCode(t *testing.T) {
	// Two NAL units with 4-byte start codes.
	data := []byte{
		0x00, 0x00, 0x00, 0x01, 0x67, 0xAA, 0xBB, // NAL 1: SPS
		0x00, 0x00, 0x00, 0x01, 0x68, 0xCC,         // NAL 2: PPS
	}
	got := ParseAnnexB(data)
	want := [][]byte{
		{0x67, 0xAA, 0xBB},
		{0x68, 0xCC},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseAnnexBThreeByteStartCode(t *testing.T) {
	data := []byte{
		0x00, 0x00, 0x01, 0x67, 0xAA,
		0x00, 0x00, 0x01, 0x68, 0xBB, 0xCC,
	}
	got := ParseAnnexB(data)
	want := [][]byte{
		{0x67, 0xAA},
		{0x68, 0xBB, 0xCC},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseAnnexBMixed(t *testing.T) {
	data := []byte{
		0x00, 0x00, 0x00, 0x01, 0x67, 0x11, // 4-byte sc
		0x00, 0x00, 0x01, 0x68, 0x22,        // 3-byte sc
		0x00, 0x00, 0x00, 0x01, 0x65, 0x33, 0x44, // 4-byte sc
	}
	got := ParseAnnexB(data)
	if len(got) != 3 {
		t.Fatalf("got %d NALUs, want 3", len(got))
	}
	if got[0][0] != 0x67 {
		t.Errorf("NALU[0] type = 0x%02x, want 0x67", got[0][0])
	}
	if got[1][0] != 0x68 {
		t.Errorf("NALU[1] type = 0x%02x, want 0x68", got[1][0])
	}
	if got[2][0] != 0x65 {
		t.Errorf("NALU[2] type = 0x%02x, want 0x65", got[2][0])
	}
}

func TestParseAnnexBEmpty(t *testing.T) {
	if got := ParseAnnexB(nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
	if got := ParseAnnexB([]byte{}); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
	if got := ParseAnnexB([]byte{0x00}); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestParseAnnexBSingleNALU(t *testing.T) {
	data := []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0xAA, 0xBB, 0xCC}
	got := ParseAnnexB(data)
	if len(got) != 1 {
		t.Fatalf("got %d NALUs, want 1", len(got))
	}
	want := []byte{0x65, 0xAA, 0xBB, 0xCC}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("got %v, want %v", got[0], want)
	}
}

func TestParseAnnexBIsolation(t *testing.T) {
	// Verify returned slices don't alias the input.
	data := []byte{0x00, 0x00, 0x00, 0x01, 0x67, 0x11}
	got := ParseAnnexB(data)
	got[0][0] = 0xFF
	if data[4] == 0xFF {
		t.Error("returned slice aliases input data")
	}
}

func TestStreamNALUs(t *testing.T) {
	// Same data as TestParseAnnexBFourByteStartCode.
	data := []byte{
		0x00, 0x00, 0x00, 0x01, 0x67, 0xAA, 0xBB, // SPS
		0x00, 0x00, 0x00, 0x01, 0x68, 0xCC,         // PPS
	}
	ch := StreamNALUs(bytes.NewReader(data))
	var nalus [][]byte
	for n := range ch {
		nalus = append(nalus, n)
	}
	if len(nalus) != 2 {
		t.Fatalf("got %d NALUs, want 2", len(nalus))
	}
	if nalus[0][0] != 0x67 {
		t.Errorf("NALU[0] first byte = 0x%02x, want 0x67", nalus[0][0])
	}
	if nalus[1][0] != 0x68 {
		t.Errorf("NALU[1] first byte = 0x%02x, want 0x68", nalus[1][0])
	}
}

func TestStreamNALUsThreeByte(t *testing.T) {
	data := []byte{
		0x00, 0x00, 0x01, 0x67, 0xAA,
		0x00, 0x00, 0x01, 0x65, 0xBB, 0xCC,
	}
	ch := StreamNALUs(bytes.NewReader(data))
	var nalus [][]byte
	for n := range ch {
		nalus = append(nalus, n)
	}
	if len(nalus) != 2 {
		t.Fatalf("got %d NALUs, want 2", len(nalus))
	}
	want0 := []byte{0x67, 0xAA}
	want1 := []byte{0x65, 0xBB, 0xCC}
	if !reflect.DeepEqual(nalus[0], want0) {
		t.Errorf("NALU[0] = %v, want %v", nalus[0], want0)
	}
	if !reflect.DeepEqual(nalus[1], want1) {
		t.Errorf("NALU[1] = %v, want %v", nalus[1], want1)
	}
}

func TestIsH264VCL(t *testing.T) {
	tests := []struct {
		nalu []byte
		want bool
	}{
		{[]byte{0x67}, false},         // SPS (type 7)
		{[]byte{0x68}, false},         // PPS (type 8)
		{[]byte{0x65, 0x00}, true},    // IDR (type 5)
		{[]byte{0x41, 0x00}, true},    // Non-IDR slice (type 1)
		{[]byte{0x06}, false},         // SEI (type 6)
		{nil, false},
	}
	for _, tt := range tests {
		if got := IsH264VCL(tt.nalu); got != tt.want {
			t.Errorf("IsH264VCL(%v) = %v, want %v", tt.nalu, got, tt.want)
		}
	}
}

func TestIsH265VCL(t *testing.T) {
	tests := []struct {
		nalu []byte
		want bool
	}{
		{[]byte{0x40, 0x01}, false},   // VPS (type 32)
		{[]byte{0x42, 0x01}, false},   // SPS (type 33)
		{[]byte{0x44, 0x01}, false},   // PPS (type 34)
		{[]byte{0x26, 0x01}, true},    // IDR_W_RADL (type 19)
		{[]byte{0x02, 0x01}, true},    // TRAIL_R (type 1)
		{nil, false},
		{[]byte{0x01}, false},         // too short
	}
	for _, tt := range tests {
		if got := IsH265VCL(tt.nalu); got != tt.want {
			t.Errorf("IsH265VCL(%v) = %v, want %v", tt.nalu, got, tt.want)
		}
	}
}
