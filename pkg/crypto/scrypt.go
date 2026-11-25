package crypto

// Params describes the input parameters to the scrypt
import (
	"bytes"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime/debug"
	"strconv"

	"golang.org/x/crypto/scrypt"
)

// The code below is heavily inspired by https://github.com/elithrar/simple-scrypt

// Scrypt params, this set of parameters is recommended in
// - https://www.tarsnap.com/scrypt/scrypt-slides.pdf
// - https://pkg.go.dev/golang.org/x/crypto/scrypt#Key
// - https://pkg.go.dev/github.com/elithrar/simple-scrypt#DefaultParams
// - https://blog.filippo.io/the-scrypt-parameters/
// - https://github.com/golang/go/issues/22082
const defaultN = 32768
const defaultR = 8
const defaultP = 1

// Upper bounds for hashes we did not generate (e.g. received from RabbitMQ):
// they are run on every login, so an unbounded cost can take the stack down.
const maxN = 1 << 17
const maxR = 16
const maxP = 4

// hash length
const defaultDkLen = 32

// salt length
const defaultSaltLen = 16

// Errors
var (
	ErrInvalidHash                 = errors.New("Invalid hash format")
	ErrMismatchedHashAndPassphrase = errors.New("hash and password are different")
	ErrNoUpdateNeeded              = errors.New("hash already has correct parameters")
)

var sep = []byte("$")

type scryptHash struct {
	n    int
	r    int
	p    int
	salt []byte
	dk   []byte
}

func (h *scryptHash) UnmarshalText(hashbytes []byte) error {
	// Decode existing hash, retrieve params and salt.
	vals := bytes.Split(hashbytes, sep)
	// "scrypt", P, N, R, salt, scrypt derived key
	if len(vals) != 6 {
		return ErrInvalidHash
	}
	if string(vals[0]) != "scrypt" {
		return ErrInvalidHash
	}

	var err error

	h.n, err = strconv.Atoi(string(vals[1]))
	if err != nil {
		return ErrInvalidHash
	}

	h.r, err = strconv.Atoi(string(vals[2]))
	if err != nil {
		return ErrInvalidHash
	}

	h.p, err = strconv.Atoi(string(vals[3]))
	if err != nil {
		return ErrInvalidHash
	}

	if h.n < 2 || h.n&(h.n-1) != 0 || h.n > maxN || h.r < 1 || h.r > maxR || h.p < 1 || h.p > maxP {
		return ErrInvalidHash
	}

	h.salt = make([]byte, hex.DecodedLen(len(vals[4])))
	_, err = hex.Decode(h.salt, vals[4])
	if err != nil {
		return ErrInvalidHash
	}

	h.dk = make([]byte, hex.DecodedLen(len(vals[5])))
	_, err = hex.Decode(h.dk, vals[5])
	if err != nil || len(h.dk) == 0 { // an empty key makes pbkdf2 panic on login
		return ErrInvalidHash
	}

	return nil
}

func (h *scryptHash) MarshalText() ([]byte, error) {
	s := fmt.Sprintf("scrypt$%d$%d$%d$%x$%x", h.n, h.r, h.p, h.salt, h.dk)
	return []byte(s), nil
}

func (h *scryptHash) Compare(passphrase []byte) error {
	// scrypt the cleartext passphrase with the same parameters and salt
	other, err := scrypt.Key(passphrase, h.salt, h.n, h.r, h.p, len(h.dk))
	if err != nil {
		return err
	}

	// Constant time comparison
	if subtle.ConstantTimeCompare(h.dk, other) == 1 {
		return nil
	}

	return ErrMismatchedHashAndPassphrase
}

func (h *scryptHash) NeedUpdate() bool {
	return h.n != defaultN || h.p != defaultP || h.r != defaultR ||
		len(h.salt) != defaultSaltLen || len(h.dk) != defaultDkLen
}

// GenerateFromPassphrase returns the derived key of the passphrase using the
// parameters provided. The parameters are prepended to the derived key and
// separated by the "$" character (0x24).
// If the parameters provided are less than the minimum acceptable values,
// an error will be returned.
func GenerateFromPassphrase(passphrase []byte) ([]byte, error) {
	h := &scryptHash{n: defaultN, r: defaultR, p: defaultP}
	var err error

	h.salt = GenerateRandomBytes(defaultSaltLen)

	// scrypt.Key returns the raw scrypt derived key.
	h.dk, err = scrypt.Key(passphrase, h.salt, h.n, h.r, h.p, defaultDkLen)
	if err != nil {
		return nil, err
	}

	return h.MarshalText()
}

// CompareHashAndPassphrase compares a derived key with the possible cleartext
// equivalent. The parameters used in the provided derived key are used. The
// comparison performed by this function is constant-time.
//
// It returns an error if the derived keys do not match. It also returns a
// needUpdate boolean indicating whether or not the passphrase hash has
// outdated parameters and should be recomputed.
func CompareHashAndPassphrase(hash []byte, passphrase []byte) (needUpdate bool, err error) {
	h := &scryptHash{}
	if err = h.UnmarshalText(hash); err != nil {
		return false, err
	}
	if err = h.Compare(passphrase); err != nil {
		return false, err
	}

	// Force go to release the memory, as scrypt takes a lot of RAM by design
	// and go has some GC heuristics that can keep it for some times.
	// See https://github.com/golang/go/issues/20000
	// and https://groups.google.com/forum/#!topic/golang-nuts/I9R9MKUS9bo
	debug.FreeOSMemory()

	return h.NeedUpdate(), nil
}

// ValidateScryptHashFormat validates that the provided hash is a correctly formatted
// textual scrypt hash (e.g., "scrypt$P$N$R$<hexsalt>$<hexdk>").
// It does not perform any comparison against a passphrase.
func ValidateScryptHashFormat(hash []byte) error {
	h := &scryptHash{}
	return h.UnmarshalText(hash)
}
