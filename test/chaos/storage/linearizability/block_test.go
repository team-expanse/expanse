package linearizability

import (
	"bytes"
	"testing"
)

func TestBlockRoundTrip(t *testing.T) {
	b := EncodeBlock(5, 42)
	if len(b) != BlockSize {
		t.Fatalf("len %d", len(b))
	}
	tok, err := DecodeBlock(5, b)
	if err != nil || tok != 42 {
		t.Fatalf("decode: tok=%d err=%v", tok, err)
	}
}

func TestBlockZeroIsInitial(t *testing.T) {
	tok, err := DecodeBlock(3, make([]byte, BlockSize))
	if err != nil || tok != 0 {
		t.Fatalf("zero block: tok=%d err=%v", tok, err)
	}
}

func TestBlockRejectsCorruption(t *testing.T) {
	good := EncodeBlock(5, 42)
	torn := append([]byte(nil), good...)
	torn[BlockSize-1] ^= 0xff
	if _, err := DecodeBlock(5, torn); err == nil {
		t.Fatal("torn block accepted")
	}
	if _, err := DecodeBlock(6, good); err == nil {
		t.Fatal("block for another key accepted")
	}
	mixed := append(bytes.Clone(good[:BlockSize/2]), EncodeBlock(5, 43)[BlockSize/2:]...)
	if _, err := DecodeBlock(5, mixed); err == nil {
		t.Fatal("two-write mix accepted")
	}
}
