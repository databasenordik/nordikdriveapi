// Package notificationcontract contains transport contracts, never domain models.
package notificationcontract

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/mail"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
)

var ErrInvalid = errors.New("invalid notification command")

type Attachment struct {
	Bucket      string `json:"bucket"`
	Object      string `json:"object"`
	Generation  int64  `json:"generation"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
}
type Payload struct {
	To          string       `json:"to"`
	Subject     string       `json:"subject"`
	HTML        string       `json:"html"`
	Attachments []Attachment `json:"attachments,omitempty"`
}
type Command struct {
	Version      int       `json:"version"`
	ID           string    `json:"id"`
	Producer     string    `json:"producer"`
	Key          string    `json:"key"`
	Kind         string    `json:"kind"`
	CreatedAt    time.Time `json:"created_at"`
	Deadline     time.Time `json:"deadline"`
	Supersession string    `json:"supersession,omitempty"`
	Sequence     int64     `json:"sequence,omitempty"`
	KeyVersion   string    `json:"key_version"`
	Ciphertext   string    `json:"ciphertext"`
	Fingerprint  string    `json:"fingerprint"`
}
type Codec struct {
	Keys   map[string][]byte
	Active string
}

// NewCodec accepts a version-to-base64-encoded 256-bit-key map.
func NewCodec(encoded, active string) (*Codec, error) {
	var entries map[string]string
	if json.Unmarshal([]byte(encoded), &entries) != nil || active == "" {
		return nil, ErrInvalid
	}
	c := &Codec{Keys: map[string][]byte{}, Active: active}
	for version, value := range entries {
		key, err := base64.StdEncoding.DecodeString(value)
		if err != nil || len(key) != 32 || version == "" {
			return nil, ErrInvalid
		}
		c.Keys[version] = key
	}
	if c.Keys[active] == nil {
		return nil, ErrInvalid
	}
	return c, nil
}
func (p Payload) Validate() error {
	address, err := mail.ParseAddress(p.To)
	if err != nil || address.Address != p.To || strings.ContainsAny(p.To+p.Subject, "\r\n") || len(p.Subject) == 0 || len(p.Subject) > 500 || len(p.HTML) == 0 || len(p.HTML) > 256*1024 || len(p.Attachments) > 1 {
		return ErrInvalid
	}
	for _, a := range p.Attachments {
		if a.Bucket != "nordik-drive-photos" || !strings.HasPrefix(a.Object, "support_requests/") || path.Clean(a.Object) != a.Object || a.Generation <= 0 || a.Size <= 0 || a.Size > 5*1024*1024 || len(a.SHA256) != 64 || a.Filename == "" || path.Base(a.Filename) != a.Filename || strings.ContainsAny(a.Filename, "\r\n\\") {
			return ErrInvalid
		}
		if _, err := hex.DecodeString(a.SHA256); err != nil {
			return ErrInvalid
		}
		if a.ContentType != "image/png" && a.ContentType != "image/jpeg" && a.ContentType != "image/webp" {
			return ErrInvalid
		}
	}
	return nil
}
func (c Command) Validate(now time.Time) error {
	if c.Version != 1 || uuid.Validate(c.ID) != nil || len(c.ID) != 36 || len(c.Key) == 0 || len(c.Key) > 240 || (c.Producer != "identity" && c.Producer != "records" && c.Producer != "support") || (c.Kind != "otp" && c.Kind != "normal") || c.CreatedAt.IsZero() || !c.Deadline.After(c.CreatedAt) || c.CreatedAt.After(now.Add(time.Minute)) || c.Deadline.Sub(c.CreatedAt) > 7*24*time.Hour || len(c.Supersession) > 240 || c.Sequence < 0 {
		return ErrInvalid
	}
	if c.Kind == "otp" && (c.Producer != "identity" || c.Deadline.Sub(c.CreatedAt) > 10*time.Minute) {
		return ErrInvalid
	}
	return nil
}
func (c *Codec) Fingerprint(command Command, p Payload) string {
	key := c.Keys[command.KeyVersion]
	if len(key) != 32 {
		return ""
	}
	b, _ := json.Marshal(p)
	m := hmac.New(sha256.New, key)
	m.Write([]byte("notification-fingerprint-v1\x00" + command.Producer + "\x00" + command.Key + "\x00" + command.Kind + "\x00"))
	m.Write(b)
	return hex.EncodeToString(m.Sum(nil))
}
func aad(command Command) []byte { command.Ciphertext = ""; b, _ := json.Marshal(command); return b }
func (c *Codec) Seal(command Command, p Payload) (Command, error) {
	if p.Validate() != nil || command.Validate(command.CreatedAt) != nil {
		return Command{}, ErrInvalid
	}
	command.KeyVersion = c.Active
	command.Fingerprint = c.Fingerprint(command, p)
	block, err := aes.NewCipher(c.Keys[c.Active])
	if err != nil {
		return Command{}, ErrInvalid
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return Command{}, ErrInvalid
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Command{}, err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return Command{}, err
	}
	command.Ciphertext = base64.StdEncoding.EncodeToString(aead.Seal(nonce, nonce, b, aad(command)))
	return command, nil
}
func (c *Codec) Open(command Command) (Payload, error) {
	var p Payload
	block, err := aes.NewCipher(c.Keys[command.KeyVersion])
	if err != nil {
		return p, ErrInvalid
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return p, ErrInvalid
	}
	b, err := base64.StdEncoding.DecodeString(command.Ciphertext)
	if err != nil || len(b) < aead.NonceSize()+aead.Overhead() {
		return p, ErrInvalid
	}
	plain, err := aead.Open(nil, b[:aead.NonceSize()], b[aead.NonceSize():], aad(command))
	if err != nil {
		return p, ErrInvalid
	}
	if json.Unmarshal(plain, &p) != nil || p.Validate() != nil || !hmac.Equal([]byte(command.Fingerprint), []byte(c.Fingerprint(command, p))) {
		return Payload{}, ErrInvalid
	}
	return p, nil
}
