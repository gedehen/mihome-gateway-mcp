package native

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"testing"
)

// TestECJPAKEInternalConsistency 验证 client 和 server 用同一 passcode 能协商出相同密钥
func TestECJPAKEInternalConsistency(t *testing.T) {
	passcode := "525945"

	client, err := NewECJPAKE("client", passcode)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewECJPAKE("server", passcode)
	if err != nil {
		t.Fatal(err)
	}

	// Round 1: client → server → client
	cR1, err := client.WriteRoundOne()
	if err != nil {
		t.Fatal("client WriteRoundOne:", err)
	}
	if len(cR1) != 330 {
		t.Fatalf("client R1 length = %d, want 330", len(cR1))
	}

	sR1, err := server.WriteRoundOne()
	if err != nil {
		t.Fatal("server WriteRoundOne:", err)
	}
	if len(sR1) != 330 {
		t.Fatalf("server R1 length = %d, want 330", len(sR1))
	}

	if err := server.ReadRoundOne(cR1); err != nil {
		t.Fatal("server ReadRoundOne:", err)
	}
	if err := client.ReadRoundOne(sR1); err != nil {
		t.Fatal("client ReadRoundOne:", err)
	}

	// Round 2
	cR2, err := client.WriteRoundTwo()
	if err != nil {
		t.Fatal("client WriteRoundTwo:", err)
	}
	if len(cR2) != 165 {
		t.Fatalf("client R2 length = %d, want 165", len(cR2))
	}

	sR2, err := server.WriteRoundTwo()
	if err != nil {
		t.Fatal("server WriteRoundTwo:", err)
	}
	if len(sR2) != 168 {
		t.Fatalf("server R2 length = %d, want 168", len(sR2))
	}

	clientKey, err := client.ReadRoundTwo(sR2)
	if err != nil {
		t.Fatal("client ReadRoundTwo:", err)
	}

	serverKey, err := server.ReadRoundTwo(cR2)
	if err != nil {
		t.Fatal("server ReadRoundTwo:", err)
	}

	if !bytes.Equal(clientKey, serverKey) {
		t.Fatalf("shared key mismatch!\nclient: %s\nserver: %s",
			hex.EncodeToString(clientKey), hex.EncodeToString(serverKey))
	}

	t.Logf("✅ shared key: %s", hex.EncodeToString(clientKey))
}

func TestPointEncoding(t *testing.T) {
	_, px, py := genKeyPair()
	enc := encodePoint66(px, py)

	if len(enc) != 66 || enc[0] != 0x41 || enc[1] != 0x04 {
		t.Fatal("bad header")
	}

	dx, dy, err := decodePoint66(enc)
	if err != nil {
		t.Fatal(err)
	}
	if dx.Cmp(px) != 0 || dy.Cmp(py) != 0 {
		t.Fatal("roundtrip mismatch")
	}
}

func TestPasswordEncoding(t *testing.T) {
	// gateway.js: BN(new TextEncoder().encode("525945")) = 0x353235393435
	secret := new(big.Int).SetBytes([]byte("525945"))
	expected, _ := hex.DecodeString("353235393435")
	expectedBN := new(big.Int).SetBytes(expected)

	if secret.Cmp(expectedBN) != 0 {
		t.Fatalf("password encoding mismatch: got %s, want %s", secret.Text(16), expectedBN.Text(16))
	}
}

func TestCipherRoundtrip(t *testing.T) {
	key := make([]byte, 16)
	for i := range key {
		key[i] = byte(i)
	}
	salt := make([]byte, 8)
	for i := range salt {
		salt[i] = byte(i + 16)
	}

	enc, _ := NewCipher(key, salt)
	dec, _ := NewCipher(key, salt)

	pt := []byte("hello mihome gateway!")
	ct, err := enc.Encrypt(pt)
	if err != nil {
		t.Fatal(err)
	}

	got, err := dec.Decrypt(ct)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pt, got) {
		t.Fatal("roundtrip mismatch")
	}
}

func TestCipherReplayProtection(t *testing.T) {
	key := make([]byte, 16)
	salt := make([]byte, 8)

	enc, _ := NewCipher(key, salt)
	dec, _ := NewCipher(key, salt)

	ct, _ := enc.Encrypt([]byte("test"))
	_, err := dec.Decrypt(ct)
	if err != nil {
		t.Fatal("first decrypt should succeed:", err)
	}
	_, err = dec.Decrypt(ct)
	if err == nil {
		t.Fatal("replay should be detected")
	}
}

func TestSHA256(t *testing.T) {
	h := sha256.Sum256([]byte{})
	expected := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if hex.EncodeToString(h[:]) != expected {
		t.Fatal("SHA256 mismatch")
	}
}
