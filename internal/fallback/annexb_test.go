package fallback

import (
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
