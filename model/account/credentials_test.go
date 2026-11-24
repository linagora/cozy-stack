package account

import (
	"bytes"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/rand"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncryptDecrytCredentials(t *testing.T) {
	config.UseTestFile(t)

	encryptedCreds1, err := EncryptCredentials("me@mycozy.cloud", "fzEE6HFWsSp8jP")
	require.NoError(t, err)

	encryptedCreds2, err := EncryptCredentials("me@mycozy.cloud", "fzEE6HFWsSp8jP")
	require.NoError(t, err)

	encryptedCreds3, err := EncryptCredentials("", "fzEE6HFWsSp8jP")
	require.NoError(t, err)

	assert.NotEqual(t, encryptedCreds1, encryptedCreds2)

	{
		login, password, err := DecryptCredentials(encryptedCreds1)
		require.NoError(t, err)

		assert.Equal(t, "me@mycozy.cloud", login)
		assert.Equal(t, "fzEE6HFWsSp8jP", password)
	}
	{
		login, password, err := DecryptCredentials(encryptedCreds2)
		require.NoError(t, err)

		assert.Equal(t, "me@mycozy.cloud", login)
		assert.Equal(t, "fzEE6HFWsSp8jP", password)
	}
	{
		login, password, err := DecryptCredentials(encryptedCreds3)
		require.NoError(t, err)

		assert.Equal(t, "", login)
		assert.Equal(t, "fzEE6HFWsSp8jP", password)
	}
}

func TestEncryptDecrytUTF8Credentials(t *testing.T) {
	config.UseTestFile(t)

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := 0; i < 1024; i++ {
		login := string(crypto.GenerateRandomBytes(rng.Intn(256)))
		password := string(crypto.GenerateRandomBytes(rng.Intn(256)))

		encryptedCreds, err := EncryptCredentials(login, password)
		require.NoError(t, err)

		loginDec, passwordDec, err := DecryptCredentials(encryptedCreds)
		require.NoError(t, err)

		assert.Equal(t, loginDec, login)
		assert.Equal(t, passwordDec, password)
	}

	for i := 0; i < 1024; i++ {
		login := utils.RandomString(rng.Intn(256))
		password := utils.RandomString(rng.Intn(256))

		encryptedCreds, err := EncryptCredentials(login, password)
		require.NoError(t, err)

		loginDec, passwordDec, err := DecryptCredentials(encryptedCreds)
		require.NoError(t, err)

		assert.Equal(t, loginDec, login)
		assert.Equal(t, passwordDec, password)
	}
}

func TestDecryptCredentialsRandom(t *testing.T) {
	config.UseTestFile(t)

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := 0; i < 1024; i++ {
		encrypted := base64.StdEncoding.EncodeToString(crypto.GenerateRandomBytes(rng.Intn(256)))
		_, _, err := DecryptCredentials(encrypted)
		assert.Error(t, err)
	}
	for i := 0; i < 1024; i++ {
		encrypted := crypto.GenerateRandomBytes(rng.Intn(256))
		encryptedWithHeader := make([]byte, len(cipherHeader)+len(encrypted))
		copy(encryptedWithHeader[0:], cipherHeader)
		copy(encryptedWithHeader[len(cipherHeader):], encrypted)
		_, _, err := DecryptCredentials(base64.StdEncoding.EncodeToString(encryptedWithHeader))
		assert.Error(t, err)
	}
}

func TestRandomBitFlipsCredentials(t *testing.T) {
	config.UseTestFile(t)

	original, err := EncryptCredentials("toto@titi.com", "X3hVYLJLRiUyCs")
	require.NoError(t, err)

	originalBuffer, err := base64.StdEncoding.DecodeString(original)
	require.NoError(t, err)

	flipped := make([]byte, len(originalBuffer))
	copy(flipped, originalBuffer)
	login, passwd, err := DecryptCredentials(base64.StdEncoding.EncodeToString(flipped))
	require.NoError(t, err)

	assert.Equal(t, "toto@titi.com", login)
	assert.Equal(t, "X3hVYLJLRiUyCs", passwd)

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := 0; i < 1000; i++ {
		copy(flipped, originalBuffer)
		flipsLen := rng.Intn(30) + 1
		flipsSet := make([]int, 0, flipsLen)
		for len(flipsSet) < flipsLen {
			flipValue := rng.Intn(len(originalBuffer) * 8)
			flipFound := false
			for j := 0; j < len(flipsSet); j++ {
				if flipsSet[j] == flipValue {
					flipFound = true
					break
				}
			}
			if !flipFound {
				flipsSet = append(flipsSet, flipValue)
			}
		}
		for _, flipValue := range flipsSet {
			mask := byte(0x1 << uint(flipValue%8))
			flipped[flipValue/8] ^= mask
		}
		_, _, err := DecryptCredentials(base64.StdEncoding.EncodeToString(flipped))
		if !assert.Error(t, err) {
			t.Fatalf("Failed with flips %v", flipsSet)
			return
		}
	}
}

func TestEncryptDecryptData(t *testing.T) {
	config.UseTestFile(t)

	var data interface{}
	err := json.Unmarshal([]byte(`{"foo":"bar","baz":{"quz": "quuz"}}`), &data)
	require.NoError(t, err)

	encBuffer, err := EncryptCredentialsData(data)
	require.NoError(t, err)

	decData, err := DecryptCredentialsData(encBuffer)
	require.NoError(t, err)

	assert.EqualValues(t, data, decData)
}

func TestRandomBitFlipsBuffer(t *testing.T) {
	config.UseTestFile(t)

	plainBuffer := make([]byte, 256)
	_, err := io.ReadFull(cryptorand.Reader, plainBuffer)
	require.NoError(t, err)

	original, err := EncryptBufferWithKey(config.GetKeyring().CredentialsEncryptorKey(), plainBuffer)
	require.NoError(t, err)

	flipped := make([]byte, len(original))
	copy(flipped, original)
	testBuffer, err := DecryptBufferWithKey(config.GetKeyring().CredentialsDecryptorKey(), flipped)
	require.NoError(t, err)

	assert.True(t, bytes.Equal(plainBuffer, testBuffer))

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := 0; i < 1000; i++ {
		copy(flipped, original)
		flipsLen := rng.Intn(30) + 1
		flipsSet := make([]int, 0, flipsLen)
		for len(flipsSet) < flipsLen {
			flipValue := rng.Intn(len(original) * 8)
			flipFound := false
			for j := 0; j < len(flipsSet); j++ {
				if flipsSet[j] == flipValue {
					flipFound = true
					break
				}
			}
			if !flipFound {
				flipsSet = append(flipsSet, flipValue)
			}
		}
		for _, flipValue := range flipsSet {
			mask := byte(0x1 << uint(flipValue%8))
			flipped[flipValue/8] ^= mask
		}
		_, err := DecryptBufferWithKey(config.GetKeyring().CredentialsDecryptorKey(), flipped)
		if !assert.Error(t, err) {
			t.Fatalf("Failed with flips %v", flipsSet)
			return
		}
	}
}

func TestAccountsEncryptDecrypt(t *testing.T) {
	config.UseTestFile(t)

	v := []byte(`
{
    "_id": "d01aa821781612dce542a13d6989e6d0",
    "_rev": "5-c8fc2169ff3226165688865e7cb609ef",
    "_type": "io.cozy.accounts",
    "account_type": "labanquepostale44",
    "auth": {
        "accountName": "Perso",
        "identifier": "WHATYOUWANT",
        "secret": "YOUWANTTOREADMYSECRET"
    },
    "data": {
        "account_type": "linxo",
        "auth": {
            "login": "linxo.SOMEID@cozy.rocks",
            "password": "SOMEPASSWORD"
        },
        "status": "connected",
        "token": "4D757B74AD",
        "uuid": "f6bb19cf-1c03-4d80-92e9-af66c18c4aa4"
    },
    "type": "io.cozy.accounts"
}
`)

	var encrypted, decrypted bool
	var m1 map[string]interface{} // original
	var m2 map[string]interface{} // encrypted
	var m3 map[string]interface{} // decrypted
	assert.NoError(t, json.Unmarshal(v, &m1))
	assert.NoError(t, json.Unmarshal(v, &m2))
	assert.NoError(t, json.Unmarshal(v, &m3))

	encrypted = encryptMap(m2)
	assert.True(t, encrypted)

	{
		auth1 := m2["auth"].(map[string]interface{})
		auth2 := m2["data"].(map[string]interface{})["auth"].(map[string]interface{})
		{
			_, ok1 := auth1["secret"]
			_, ok2 := auth1["secret_encrypted"]
			assert.False(t, ok1)
			assert.True(t, ok2)
		}
		{
			_, ok1 := auth2["password"]
			_, ok2 := auth2["credentials_encrypted"]
			assert.False(t, ok1)
			assert.True(t, ok2)
		}
	}

	encrypted = encryptMap(m3)
	decrypted = decryptMap(m3)
	assert.True(t, encrypted)
	assert.True(t, decrypted)
	assert.EqualValues(t, m1, m3)

	{
		auth1 := m3["auth"].(map[string]interface{})
		auth2 := m3["data"].(map[string]interface{})["auth"].(map[string]interface{})
		{
			_, ok1 := auth1["secret"]
			_, ok2 := auth1["secret_encrypted"]
			assert.True(t, ok1)
			assert.False(t, ok2)
		}
		{
			_, ok1 := auth2["password"]
			_, ok2 := auth2["credentials_encrypted"]
			assert.True(t, ok1)
			assert.False(t, ok2)
		}
	}
}

func TestDropForeignEncrypted(t *testing.T) {
	old := map[string]interface{}{
		"auth": map[string]interface{}{"credentials_encrypted": "stored"},
		"data": map[string]interface{}{"auth": map[string]interface{}{"secret_encrypted": "stored"}},
	}
	doc := map[string]interface{}{
		"auth": map[string]interface{}{
			"login":                 "me",
			"credentials_encrypted": "stored",
			"secret_encrypted":      "foreign",
		},
		"data": map[string]interface{}{"auth": map[string]interface{}{"secret_encrypted": "foreign"}},
	}
	DropForeignEncrypted(doc, old)
	assert.Equal(t, map[string]interface{}{"login": "me", "credentials_encrypted": "stored"}, doc["auth"])
	assert.Empty(t, doc["data"].(map[string]interface{})["auth"])

	created := map[string]interface{}{"auth": map[string]interface{}{"credentials_encrypted": "foreign"}}
	DropForeignEncrypted(created, nil)
	assert.Empty(t, created["auth"])
}
