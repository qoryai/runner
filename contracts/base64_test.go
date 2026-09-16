package contracts_test

import "encoding/base64"

// base64Decode is the standard decoding a run.log chunk uses.
func base64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
