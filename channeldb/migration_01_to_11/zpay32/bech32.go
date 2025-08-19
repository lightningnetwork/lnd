package zpay32

import (
	"fmt"
	"strings"
)

const charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

var gen = []int{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}

// NOTE: This method it a slight modification of the method bech32.Decode found
// btcutil, allowing strings to be more than 90 characters.

// decodeBech32 decodes a bech32 encoded string, returning the human-readable
// part and the data part excluding the checksum.
// Note: the data will be base32 encoded, that is each element of the returned
// byte array will encode 5 bits of data. Use the ConvertBits method to convert
// this to 8-bit representation.
func decodeBech32(bech string) (string, []byte, error) {
	// The maximum allowed length for a bech32 string is 90. It must also
	// be at least 8 characters, since it needs a non-empty HRP, a
	// separator, and a 6 character checksum.
	// NB: The 90 character check specified in BIP173 is skipped here, to
	// allow strings longer than 90 characters.
	if len(bech) < 8 {
		return "", nil, fmt.Errorf("invalid bech32 string length %d",
			len(bech))
	}
	// Only	ASCII characters between 33 and 126 are allowed.
	for i := 0; i < len(bech); i++ {
		if bech[i] < 33 || bech[i] > 126 {
			return "", nil, fmt.Errorf("invalid character in "+
				"string: '%c'", bech[i])
		}
	}

	// The characters must be either all lowercase or all uppercase.
	lower := strings.ToLower(bech)
	upper := strings.ToUpper(bech)
	if bech != lower && bech != upper {
		return "", nil, fmt.Errorf("string not all lowercase or all " +
			"uppercase")
	}

	// We'll work with the lowercase string from now on.
	bech = lower

	// The string is invalid if the last '1' is non-existent, it is the
	// first character of the string (no human-readable part) or one of the
	// last 6 characters of the string (since checksum cannot contain '1'),
	// or if the string is more than 90 characters in total.
	one := strings.LastIndexByte(bech, '1')
	if one < 1 || one+7 > len(bech) {
		return "", nil, fmt.Errorf("invalid index of 1")
	}

	// The human-readable part is everything before the last '1'.
	hrp := bech[:one]
	data := bech[one+1:]

	// Each character corresponds to the byte with value of the index in
	// 'charset'.
	decoded, err := toBytes(data)
	if err != nil {
		return "", nil, fmt.Errorf("failed converting data to bytes: "+
			"%v", err)
	}

	if !bech32VerifyChecksum(hrp, decoded) {
		moreInfo := ""
		checksum := bech[len(bech)-6:]
		expected, err := toChars(bech32Checksum(hrp,
			decoded[:len(decoded)-6]))
		if err == nil {
			moreInfo = fmt.Sprintf("Expected %v, got %v.",
				expected, checksum)
		}

		return "", nil, fmt.Errorf("checksum failed. %s", moreInfo)
	}

	// We exclude the last 6 bytes, which is the checksum.
	return hrp, decoded[:len(decoded)-6], nil
}

// toBytes converts each character in the string 'chars' to the value of the
// index of the corresponding character in 'charset'.
func toBytes(chars string) ([]byte, error) {
	decoded := make([]byte, 0, len(chars))
	for i := 0; i < len(chars); i++ {
		index := strings.IndexByte(charset, chars[i])
		if index < 0 {
			return nil, fmt.Errorf("invalid character not part of "+
				"charset: %v", chars[i])
		}
		decoded = append(decoded, byte(index))
	}
	return decoded, nil
}

// toChars converts the byte slice 'data' to a string where each byte in 'data'
// encodes the index of a character in 'charset'.
func toChars(data []byte) (string, error) {
	result := make([]byte, 0, len(data))
	for _, b := range data {
		if int(b) >= len(charset) {
			return "", fmt.Errorf("invalid data byte: %v", b)
		}
		result = append(result, charset[b])
	}
	return string(result), nil
}

// For more details on the checksum calculation, please refer to BIP 173.
func bech32Checksum(hrp string, data []byte) []byte {
	// Convert the bytes to list of integers, as this is needed for the
	// checksum calculation.
	integers := make([]int, len(data))
	for i, b := range data {
		integers[i] = int(b)
	}
	values := append(bech32HrpExpand(hrp), integers...)
	values = append(values, []int{0, 0, 0, 0, 0, 0}...)
	polymod := bech32Polymod(values) ^ 1
	var res []byte
	for i := 0; i < 6; i++ {
		res = append(res, byte((polymod>>uint(5*(5-i)))&31))
	}
	return res
}

// For more details on the polymod calculation, please refer to BIP 173.
func bech32Polymod(values []int) int {
	chk := 1
	for _, v := range values {
		b := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ v
		for i := 0; i < 5; i++ {
			if (b>>uint(i))&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

// For more details on HRP expansion, please refer to BIP 173.
func bech32HrpExpand(hrp string) []int {
	v := make([]int, 0, len(hrp)*2+1)
	for i := 0; i < len(hrp); i++ {
		v = append(v, int(hrp[i]>>5))
	}
	v = append(v, 0)
	for i := 0; i < len(hrp); i++ {
		v = append(v, int(hrp[i]&31))
	}
	return v
}

// For more details on the checksum verification, please refer to BIP 173.
func bech32VerifyChecksum(hrp string, data []byte) bool {
	integers := make([]int, len(data))
	for i, b := range data {
		integers[i] = int(b)
	}
	concat := append(bech32HrpExpand(hrp), integers...)
	return bech32Polymod(concat) == 1
}
