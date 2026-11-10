package notificationcontract

import (
	"bytes"
	"encoding/base64"
	"github.com/google/uuid"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testCodec(t *testing.T) *Codec {
	t.Helper()
	c, err := NewCodec(`{"1":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))+`"}`, "1")
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func testCommand() Command {
	now := time.Now().UTC()
	return Command{Version: 1, ID: uuid.NewString(), Producer: "identity", Key: "signup-otp:synthetic", Kind: "otp", CreatedAt: now, Deadline: now.Add(10 * time.Minute)}
}
func TestPayloadEncryptionAndMetadataBinding(t *testing.T) {
	c := testCodec(t)
	p := Payload{To: "person@example.test", Subject: "Verification", HTML: "<strong>012345</strong>"}
	sealed, err := c.Seal(testCommand(), p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed.Ciphertext, "012345") {
		t.Fatal("plaintext code in ciphertext")
	}
	opened, err := c.Open(sealed)
	if err != nil || !reflect.DeepEqual(opened, p) {
		t.Fatal("payload did not round trip")
	}
	for _, mutate := range []func(*Command){func(v *Command) { v.ID = uuid.NewString() }, func(v *Command) { v.Producer = "support" }, func(v *Command) { v.Key = "different" }, func(v *Command) { v.Deadline = v.Deadline.Add(time.Minute) }, func(v *Command) { v.Sequence++ }, func(v *Command) { v.Fingerprint = "bad" }, func(v *Command) { v.KeyVersion = "missing" }, func(v *Command) { v.Ciphertext = "invalid" }} {
		copy := sealed
		mutate(&copy)
		if _, err := c.Open(copy); err == nil {
			t.Fatal("tampered command was accepted")
		}
	}
	other := testCodec(t)
	other.Keys["1"] = bytes.Repeat([]byte{8}, 32)
	if _, err := other.Open(sealed); err == nil {
		t.Fatal("wrong key accepted")
	}
}
func TestCommandAndPayloadValidation(t *testing.T) {
	valid := testCommand()
	p := Payload{To: "person@example.test", Subject: "hello", HTML: "body"}
	for _, mutate := range []func(*Command){func(v *Command) { v.Version = 2 }, func(v *Command) { v.ID = "bad" }, func(v *Command) { v.Key = "" }, func(v *Command) { v.Producer = "unknown" }, func(v *Command) { v.Kind = "sms" }, func(v *Command) { v.Deadline = v.CreatedAt }, func(v *Command) { v.CreatedAt = time.Now().Add(2 * time.Minute) }, func(v *Command) { v.Deadline = v.CreatedAt.Add(11 * time.Minute) }, func(v *Command) { v.Sequence = -1 }} {
		v := valid
		mutate(&v)
		if v.Validate(time.Now()) == nil {
			t.Fatal("invalid command accepted")
		}
	}
	for _, bad := range []Payload{{To: "bad", Subject: "s", HTML: "b"}, {To: p.To, Subject: "a\r\nBcc: attacker", HTML: "b"}, {To: p.To, Subject: "s"}, {To: p.To, HTML: "b"}, {To: p.To, Subject: "s", HTML: strings.Repeat("x", 256*1024+1)}} {
		if bad.Validate() == nil {
			t.Fatal("invalid payload accepted")
		}
	}
	ref := Attachment{Bucket: "nordik-drive-photos", Object: "support_requests/1/image.png", Generation: 1, Size: 10, SHA256: strings.Repeat("a", 64), Filename: "image.png", ContentType: "image/png"}
	p.Attachments = []Attachment{ref}
	if p.Validate() != nil {
		t.Fatal("valid reference rejected")
	}
	for _, mutate := range []func(*Attachment){func(v *Attachment) { v.Bucket = "other" }, func(v *Attachment) { v.Object = "support_requests/../secret" }, func(v *Attachment) { v.Generation = 0 }, func(v *Attachment) { v.Size = 6 * 1024 * 1024 }, func(v *Attachment) { v.SHA256 = strings.Repeat("z", 64) }, func(v *Attachment) { v.Filename = "../image.png" }, func(v *Attachment) { v.ContentType = "text/html" }} {
		bad := ref
		mutate(&bad)
		p.Attachments = []Attachment{bad}
		if p.Validate() == nil {
			t.Fatal("unsafe attachment accepted")
		}
	}
	for _, encoded := range []string{"not-json", `{}`, `{"1":"bad"}`, `{"1":""}`} {
		if _, err := NewCodec(encoded, "1"); err == nil {
			t.Fatal("invalid key ring accepted")
		}
	}
}
