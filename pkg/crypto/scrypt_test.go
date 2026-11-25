package crypto

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

var pass = []byte("This is secret")

var oldhash = []byte("scrypt$16384$8$1$172721d78c4dc2d6$d74e5214536e31193b5300d6e162e2fdcb9f0c1bffc5be446b93edee65a0570d")
var goodhash = []byte("scrypt$32768$8$1$bc39ced1a16922f626b7036edc2711a9$84a3e30dbde37dcb1b169365a8f7b88c5d0dde057d8b85e3a361fedf8a80d1ef")
var badhash = []byte("scrypt$16384$8$1$3a371fe057cef0063d01fce866acb989$3228bf807f307badbadbadbadbadbadbadbad09fc011f5859ad6a1504de56455")

func TestGenerateFromPassphrase(t *testing.T) {
	val, err := GenerateFromPassphrase(pass)
	assert.NoError(t, err)
	assert.Equal(t, 5, bytes.Count(val, sep), "hash should have 6 parts")
	algo := string(bytes.Split(val, sep)[0])
	assert.Equal(t, "scrypt", algo, "hash should contain algo")
}

func TestCompareGoodHashAndPassphrase(t *testing.T) {
	_, err := CompareHashAndPassphrase(goodhash, pass)
	assert.NoError(t, err)
}

func TestCompareBadHashAndPassphrase(t *testing.T) {
	_, err := CompareHashAndPassphrase(badhash, pass)
	assert.Error(t, err)
}

func TestUpdateHashNoUpdate(t *testing.T) {
	needUpdate, err := CompareHashAndPassphrase(goodhash, pass)
	assert.NoError(t, err)
	assert.False(t, needUpdate)
}

func TestUpdateHashNeedUpdate(t *testing.T) {
	needUpdate, err := CompareHashAndPassphrase(oldhash, pass)
	assert.NoError(t, err)
	assert.True(t, needUpdate)
}

func TestValidateScryptHashFormatRejectsBadParams(t *testing.T) {
	assert.NoError(t, ValidateScryptHashFormat(goodhash))
	for _, h := range []string{
		"scrypt$1099511627776$1$1$bc39ced1a16922f626b7036edc2711a9$84a3e30d",
		"scrypt$32768$1024$1$bc39ced1a16922f626b7036edc2711a9$84a3e30d",
		"scrypt$32768$8$0$bc39ced1a16922f626b7036edc2711a9$84a3e30d",
		"scrypt$1$8$1$bc39ced1a16922f626b7036edc2711a9$84a3e30d",
		"scrypt$1000$8$1$bc39ced1a16922f626b7036edc2711a9$84a3e30d",
		"scrypt$32768$8$1$bc39ced1a16922f626b7036edc2711a9$",
	} {
		assert.ErrorIs(t, ValidateScryptHashFormat([]byte(h)), ErrInvalidHash, h)
		_, err := CompareHashAndPassphrase([]byte(h), pass)
		assert.ErrorIs(t, err, ErrInvalidHash, h)
	}
}
