package account

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/keyring"
	"golang.org/x/crypto/nacl/box"
)

const cipherHeader = "nacl"
const nonceLen = 24
const plainPrefixLen = 4

var (
	errCannotDecrypt = errors.New("accounts: cannot decrypt credentials")
	errCannotEncrypt = errors.New("accounts: cannot encrypt credentials")
	// ErrBadCredentials is used when an account credentials cannot be decrypted
	ErrBadCredentials = errors.New("accounts: bad credentials")
)

// EncryptCredentialsWithKey takes a login / password and encrypts their values using
// the vault public key.
func EncryptCredentialsWithKey(encryptorKey *keyring.NACLKey, login, password string) (string, error) {
	if encryptorKey == nil {
		return "", errCannotEncrypt
	}

	loginLen := len(login)

	// make a buffer containing the length of the login in bigendian over 4
	// bytes, followed by the login and password contatenated.
	creds := make([]byte, plainPrefixLen+loginLen+len(password))

	// put the length of login in the first 4 bytes
	binary.BigEndian.PutUint32(creds[0:], uint32(loginLen))

	// copy the concatenation of login + password in the end
	copy(creds[plainPrefixLen:], login)
	copy(creds[plainPrefixLen+loginLen:], password)

	var nonce [nonceLen]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		panic(err)
	}

	encryptedOut := make([]byte, len(cipherHeader)+len(nonce))
	copy(encryptedOut[0:], cipherHeader)
	copy(encryptedOut[len(cipherHeader):], nonce[:])

	encryptedCreds := box.Seal(encryptedOut, creds, &nonce, encryptorKey.PublicKey(), encryptorKey.PrivateKey())
	return base64.StdEncoding.EncodeToString(encryptedCreds), nil
}

// EncryptCredentialsData takes any json encodable data and encode and encrypts
// it using the vault public key.
func EncryptCredentialsData(data interface{}) (string, error) {
	encryptorKey := config.GetKeyring().CredentialsEncryptorKey()
	if encryptorKey == nil {
		return "", errCannotEncrypt
	}
	buf, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	cipher, err := EncryptBufferWithKey(encryptorKey, buf)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(cipher), nil
}

// EncryptBufferWithKey encrypts the given bytee buffer with the specified encryption
// key.
func EncryptBufferWithKey(encryptorKey *keyring.NACLKey, buf []byte) ([]byte, error) {
	var nonce [nonceLen]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		panic(err)
	}

	encryptedOut := make([]byte, len(cipherHeader)+len(nonce))
	copy(encryptedOut[0:], cipherHeader)
	copy(encryptedOut[len(cipherHeader):], nonce[:])

	encryptedCreds := box.Seal(encryptedOut, buf, &nonce, encryptorKey.PublicKey(), encryptorKey.PrivateKey())
	return encryptedCreds, nil
}

// EncryptCredentials encrypts the given credentials with the specified encryption
// key.
func EncryptCredentials(login, password string) (string, error) {
	encryptorKey := config.GetKeyring().CredentialsEncryptorKey()
	if encryptorKey == nil {
		return "", errCannotEncrypt
	}
	return EncryptCredentialsWithKey(encryptorKey, login, password)
}

// DecryptCredentials takes an encrypted credentials, constiting of a login /
// password pair, and decrypts it using the vault private key.
func DecryptCredentials(encryptedData string) (login, password string, err error) {
	decryptorKey := config.GetKeyring().CredentialsDecryptorKey()
	if decryptorKey == nil {
		return "", "", errCannotDecrypt
	}
	encryptedBuffer, err := base64.StdEncoding.DecodeString(encryptedData)
	if err != nil {
		return "", "", errCannotDecrypt
	}
	return DecryptCredentialsWithKey(decryptorKey, encryptedBuffer)
}

// DecryptCredentialsWithKey takes an encrypted credentials, constiting of a
// login / password pair, and decrypts it using the given private key.
func DecryptCredentialsWithKey(decryptorKey *keyring.NACLKey, encryptedCreds []byte) (login, password string, err error) {
	// check the cipher text starts with the cipher header
	if !bytes.HasPrefix(encryptedCreds, []byte(cipherHeader)) {
		return "", "", ErrBadCredentials
	}
	// skip the cipher header
	encryptedCreds = encryptedCreds[len(cipherHeader):]

	// check the encrypted creds contains the space for the nonce as prefix
	if len(encryptedCreds) < nonceLen {
		return "", "", ErrBadCredentials
	}

	// extrct the nonce from the first 24 bytes
	var nonce [nonceLen]byte
	copy(nonce[:], encryptedCreds[:nonceLen])

	// skip the nonce
	encryptedCreds = encryptedCreds[nonceLen:]
	// decrypt the cipher text and check that the plain text is more the 4 bytes
	// long, to contain the login length
	creds, ok := box.Open(nil, encryptedCreds, &nonce, decryptorKey.PublicKey(), decryptorKey.PrivateKey())
	if !ok {
		return "", "", ErrBadCredentials
	}

	// extract login length from 4 first bytes
	loginLen := int(binary.BigEndian.Uint32(creds[0:]))

	// skip login length
	creds = creds[plainPrefixLen:]

	// check credentials contains enough space to contain at least the login
	if len(creds) < loginLen {
		return "", "", ErrBadCredentials
	}

	// split the credentials into login / password
	return string(creds[:loginLen]), string(creds[loginLen:]), nil
}

// DecryptCredentialsData takes an encryted buffer and decrypts and decode its
// content.
func DecryptCredentialsData(encryptedData string) (interface{}, error) {
	decryptorKey := config.GetKeyring().CredentialsDecryptorKey()
	if decryptorKey == nil {
		return nil, errCannotDecrypt
	}
	encryptedBuffer, err := base64.StdEncoding.DecodeString(encryptedData)
	if err != nil {
		return nil, errCannotDecrypt
	}
	plainBuffer, err := DecryptBufferWithKey(decryptorKey, encryptedBuffer)
	if err != nil {
		return nil, err
	}
	var data interface{}
	if err = json.Unmarshal(plainBuffer, &data); err != nil {
		return nil, err
	}
	return data, nil
}

// DecryptBufferWithKey takes an encrypted buffer and decrypts it using the
// given private key.
func DecryptBufferWithKey(decryptorKey *keyring.NACLKey, encryptedBuffer []byte) ([]byte, error) {
	// check the cipher text starts with the cipher header
	if !bytes.HasPrefix(encryptedBuffer, []byte(cipherHeader)) {
		return nil, ErrBadCredentials
	}

	// skip the cipher header
	encryptedBuffer = encryptedBuffer[len(cipherHeader):]

	// check the encrypted creds contains the space for the nonce as prefix
	if len(encryptedBuffer) < nonceLen {
		return nil, ErrBadCredentials
	}

	// extrct the nonce from the first 24 bytes
	var nonce [nonceLen]byte
	copy(nonce[:], encryptedBuffer[:nonceLen])

	// skip the nonce
	encryptedBuffer = encryptedBuffer[nonceLen:]

	// decrypt the cipher text and check that the plain text is more the 4 bytes
	// long, to contain the login length
	plainBuffer, ok := box.Open(nil, encryptedBuffer, &nonce, decryptorKey.PublicKey(), decryptorKey.PrivateKey())
	if !ok {
		return nil, ErrBadCredentials
	}

	return plainBuffer, nil
}

// Encrypt encrypts sensitive fields inside the account. The document is
// modified in place.
func Encrypt(doc couchdb.JSONDoc) bool {
	if config.GetKeyring().CredentialsEncryptorKey() != nil {
		return encryptMap(doc.M)
	}
	return false
}

// Decrypt decrypts sensitive fields inside the account. The document is
// modified in place.
func Decrypt(doc couchdb.JSONDoc) bool {
	if config.GetKeyring().CredentialsDecryptorKey() != nil {
		return decryptMap(doc.M)
	}
	return false
}

// DropForeignEncrypted removes the *_encrypted fields of a client document
// that are not already stored in old (nil for a new account). Encrypt keeps
// them as they are and Decrypt opens any box, so the stack would otherwise
// decrypt a box taken from another instance.
func DropForeignEncrypted(doc, old map[string]interface{}) {
	auth, _ := doc["auth"].(map[string]interface{})
	oldAuth, _ := old["auth"].(map[string]interface{})
	for k, v := range auth {
		// Decrypt skips non-string values, and comparing maps would panic.
		if s, ok := v.(string); ok && strings.HasSuffix(k, "_encrypted") && oldAuth[k] != s {
			delete(auth, k)
		}
	}
	if data, ok := doc["data"].(map[string]interface{}); ok {
		oldData, _ := old["data"].(map[string]interface{})
		DropForeignEncrypted(data, oldData)
	}
}

func encryptMap(m map[string]interface{}) (encrypted bool) {
	auth, ok := m["auth"].(map[string]interface{})
	if !ok {
		return
	}
	login, _ := auth["login"].(string)
	cloned := make(map[string]interface{}, len(auth))
	var encKeys []string
	for k, v := range auth {
		var err error
		switch k {
		case "password":
			password, _ := v.(string)
			cloned["credentials_encrypted"], err = EncryptCredentials(login, password)
			if err == nil {
				encrypted = true
			}
		case "secret", "dob", "code", "answer", "access_token", "refresh_token", "appSecret", "session":
			cloned[k+"_encrypted"], err = EncryptCredentialsData(v)
			if err == nil {
				encrypted = true
			}
		default:
			if strings.HasSuffix(k, "_encrypted") {
				encKeys = append(encKeys, k)
			} else {
				cloned[k] = v
			}
		}
	}
	for _, key := range encKeys {
		if _, ok := cloned[key]; !ok {
			cloned[key] = auth[key]
		}
	}
	m["auth"] = cloned
	if data, ok := m["data"].(map[string]interface{}); ok {
		if encryptMap(data) && !encrypted {
			encrypted = true
		}
	}
	return
}

func decryptMap(m map[string]interface{}) (decrypted bool) {
	auth, ok := m["auth"].(map[string]interface{})
	if !ok {
		return
	}
	cloned := make(map[string]interface{}, len(auth))
	for k, v := range auth {
		if !strings.HasSuffix(k, "_encrypted") {
			cloned[k] = v
			continue
		}
		k = strings.TrimSuffix(k, "_encrypted")
		var str string
		str, ok = v.(string)
		if !ok {
			cloned[k] = v
			continue
		}
		var err error
		if k == "credentials" {
			cloned["login"], cloned["password"], err = DecryptCredentials(str)
		} else {
			cloned[k], err = DecryptCredentialsData(str)
		}
		if !decrypted {
			decrypted = err == nil
		}
	}
	m["auth"] = cloned
	if data, ok := m["data"].(map[string]interface{}); ok {
		if decryptMap(data) && !decrypted {
			decrypted = true
		}
	}
	return
}
