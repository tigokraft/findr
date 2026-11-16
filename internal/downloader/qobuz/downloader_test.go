package qobuz

import (
	"crypto/md5"
	"encoding/hex"
	"testing"
)

func TestCalculateSignature(t *testing.T) {
	params := map[string]string{
		"track_id":  "12345",
		"format_id": "6",
		"intent":    "stream",
	}
	ts := int64(1700000000)
	secret := "testsecret"

	// Sorted parameters:
	// format_id6 + intentstream + track_id12345
	// string = "trackgetFileUrlformat_id6intentstreamtrack_id123451700000000testsecret"
	expectedRaw := "trackgetFileUrlformat_id6intentstreamtrack_id123451700000000testsecret"
	h := md5.Sum([]byte(expectedRaw))
	expected := hex.EncodeToString(h[:])

	sig := CalculateSignature(params, ts, secret)
	if sig != expected {
		t.Errorf("CalculateSignature() = %s; expected %s", sig, expected)
	}
}
