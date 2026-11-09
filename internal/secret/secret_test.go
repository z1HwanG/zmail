package secret

import (
	"bytes"
	"errors"
	"testing"
)

func TestEncryptDecryptAndTamper(t *testing.T) {
	box, err := New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := box.EncryptString("mail-password")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := box.DecryptString(ciphertext)
	if err != nil || plain != "mail-password" {
		t.Fatalf("plain=%q err=%v", plain, err)
	}
	ciphertext[len(ciphertext)-1] ^= 1
	if _, err = box.Decrypt(ciphertext); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	if _, err = box.Decrypt([]byte{1}); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("short ciphertext error=%v", err)
	}
}

func TestNewRequiresAES256Key(t *testing.T) {
	if _, err := New(make([]byte, 16)); err == nil {
		t.Fatal("short key accepted")
	}
}
