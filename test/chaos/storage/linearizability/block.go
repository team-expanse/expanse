package linearizability

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// BlockSize is the checked unit: one 4 KiB block per register.
const BlockSize = 4096

const blockMagic = 0x4c494e424c4b3031 // "LINBLK01"

// EncodeBlock builds the deterministic block body for (key, token): a
// header naming both, then a token-seeded stream, so any torn or
// misdirected block fails DecodeBlock.
func EncodeBlock(key int, token uint64) []byte {
	b := make([]byte, BlockSize)
	binary.LittleEndian.PutUint64(b[0:], blockMagic)
	binary.LittleEndian.PutUint64(b[8:], uint64(key))
	binary.LittleEndian.PutUint64(b[16:], token)
	x := token*0x9e3779b97f4a7c15 + uint64(key) + 1
	for i := 24; i+8 <= BlockSize; i += 8 {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		binary.LittleEndian.PutUint64(b[i:], x)
	}
	return b
}

// DecodeBlock returns the token a block holds (0 for the all-zero
// initial block) or an error if it is not exactly some EncodeBlock(key, t).
func DecodeBlock(key int, b []byte) (uint64, error) {
	if len(b) != BlockSize {
		return 0, fmt.Errorf("block length %d", len(b))
	}
	if bytes.Equal(b, make([]byte, BlockSize)) {
		return 0, nil
	}
	if binary.LittleEndian.Uint64(b[0:]) != blockMagic {
		return 0, fmt.Errorf("bad magic")
	}
	if k := binary.LittleEndian.Uint64(b[8:]); k != uint64(key) {
		return 0, fmt.Errorf("block belongs to key %d, read as key %d", k, key)
	}
	tok := binary.LittleEndian.Uint64(b[16:])
	if !bytes.Equal(b, EncodeBlock(key, tok)) {
		return 0, fmt.Errorf("block body does not match token %d (torn or corrupt)", tok)
	}
	return tok, nil
}
