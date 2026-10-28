package auth

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// MinKeyLen is the shortest auth key accepted, in bytes of the env value.
const MinKeyLen = 32

// sealer encrypts people's forge tokens at rest with AES-256-GCM, and
// MACs the other records (sign). Each ciphertext names the key that made it,
// so a rotation can keep the previous key for decrypting while everything is
// re-sealed under the new one.
type sealer struct {
	current  sealKey
	previous *sealKey
}

type sealKey struct {
	id   string
	aead cipher.AEAD
	mac  []byte
}

func newSealKey(secret []byte) (sealKey, error) {
	if len(secret) < MinKeyLen {
		return sealKey{}, fmt.Errorf("auth key is %d bytes; want at least %d (openssl rand -base64 32)", len(secret), MinKeyLen)
	}
	k, err := hkdf.Key(sha256.New, secret, nil, "devbot forge tokens v1", 32)
	if err != nil {
		return sealKey{}, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return sealKey{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return sealKey{}, err
	}
	mac, err := hkdf.Key(sha256.New, secret, nil, "devbot auth records v1", 32)
	if err != nil {
		return sealKey{}, err
	}
	// The ID identifies the key without revealing anything usable about it.
	id := sha256.Sum256(append([]byte("devbot key id\x00"), k...))
	return sealKey{id: hex.EncodeToString(id[:4]), aead: aead, mac: mac}, nil
}

func newSealer(key, previous []byte) (*sealer, error) {
	cur, err := newSealKey(key)
	if err != nil {
		return nil, err
	}
	s := &sealer{current: cur}
	if len(previous) > 0 {
		p, err := newSealKey(previous)
		if err != nil {
			return nil, fmt.Errorf("previous %w", err)
		}
		s.previous = &p
	}
	return s, nil
}

// seal encrypts plain, bound to aad (the grant ID) so a ciphertext can't be
// moved to another grant.
func (s *sealer) seal(plain []byte, aad string) string {
	nonce := make([]byte, s.current.aead.NonceSize())
	rand.Read(nonce)
	ct := s.current.aead.Seal(nonce, nonce, plain, []byte(aad))
	return "v1." + s.current.id + "." + base64.RawStdEncoding.EncodeToString(ct)
}

// open decrypts, and reports whether the ciphertext should be re-sealed
// because it was made with the previous key.
func (s *sealer) open(sealed, aad string) (plain []byte, stale bool, err error) {
	parts := strings.SplitN(sealed, ".", 3)
	if len(parts) != 3 || parts[0] != "v1" {
		return nil, false, errors.New("auth: malformed sealed value")
	}
	k := s.current
	switch {
	case parts[1] == s.current.id:
	case s.previous != nil && parts[1] == s.previous.id:
		k, stale = *s.previous, true
	default:
		return nil, false, errors.New("auth: sealed with a key this install no longer has")
	}
	ct, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(ct) < k.aead.NonceSize() {
		return nil, false, errors.New("auth: malformed sealed value")
	}
	plain, err = k.aead.Open(nil, ct[:k.aead.NonceSize()], ct[k.aead.NonceSize():], []byte(aad))
	if err != nil {
		return nil, false, errors.New("auth: sealed value doesn't authenticate")
	}
	return plain, stale, nil
}

// sign MACs a record, bound to the Redis key it's stored under, so one
// written or edited by anyone without the auth key, or moved to another
// key, doesn't verify. More than serve can reach Redis; the MAC is what
// makes a record serve's.
func (s *sealer) sign(key string, record []byte) []byte {
	tag := recordMAC(s.current.mac, key, record)
	return append([]byte("m1."+base64.RawURLEncoding.EncodeToString(tag)+"."), record...)
}

// verify returns the record a signed value carries, if its MAC holds under
// the current or the previous key, and whether it was the previous one, so
// the record should be signed again.
func (s *sealer) verify(key string, signed []byte) (record []byte, stale, ok bool) {
	rest, ok := bytes.CutPrefix(signed, []byte("m1."))
	if !ok {
		return nil, false, false
	}
	enc, record, ok := bytes.Cut(rest, []byte("."))
	if !ok {
		return nil, false, false
	}
	tag, err := base64.RawURLEncoding.DecodeString(string(enc))
	if err != nil {
		return nil, false, false
	}
	if hmac.Equal(tag, recordMAC(s.current.mac, key, record)) {
		return record, false, true
	}
	if s.previous != nil && hmac.Equal(tag, recordMAC(s.previous.mac, key, record)) {
		return record, true, true
	}
	return nil, false, false
}

func recordMAC(key []byte, name string, record []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write(record)
	return h.Sum(nil)
}
