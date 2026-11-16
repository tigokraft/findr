package navidrome

import (
	"crypto/md5"
	"encoding/hex"
	"testing"
)

func TestHashToken(t *testing.T) {
	password := "secret123"
	salt := "randomsalt"

	expectedHash := md5.Sum([]byte(password + salt))
	expected := hex.EncodeToString(expectedHash[:])

	actual := hashToken(password, salt)
	if actual != expected {
		t.Errorf("hashToken() = %s; expected %s", actual, expected)
	}
}
