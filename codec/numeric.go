package codec

import (
	"fmt"
	"math/big"
)

var (
	twoTo128  = new(big.Int).Lsh(big.NewInt(1), 128)
	twoTo127  = new(big.Int).Lsh(big.NewInt(1), 127)
	maxInt128 = new(big.Int).Sub(new(big.Int).Set(twoTo127), big.NewInt(1))
	minInt128 = new(big.Int).Neg(new(big.Int).Set(twoTo127))
)

func encodeInt128(value *big.Int) ([16]byte, error) {
	var encoded [16]byte
	if value.Cmp(minInt128) < 0 || value.Cmp(maxInt128) > 0 {
		return encoded, fmt.Errorf("codec: decimal coefficient exceeds signed 128-bit range")
	}
	unsigned := new(big.Int).Set(value)
	if unsigned.Sign() < 0 {
		unsigned.Add(unsigned, twoTo128)
	}
	raw := unsigned.Bytes()
	copy(encoded[len(encoded)-len(raw):], raw)
	return encoded, nil
}

func decodeInt128(encoded []byte) (*big.Int, error) {
	if len(encoded) != 16 {
		return nil, fmt.Errorf("codec: signed 128-bit value must contain 16 bytes")
	}
	value := new(big.Int).SetBytes(encoded)
	if encoded[0]&0x80 != 0 {
		value.Sub(value, twoTo128)
	}
	return value, nil
}
