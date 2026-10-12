package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func testKey(t *testing.T) (*rsa.PrivateKey, *rsa.PublicKey) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return priv, &priv.PublicKey
}

func TestHashCompare(t *testing.T) {
	hash, err := Hash("s3cret-p@ss")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !Compare(hash, "s3cret-p@ss") {
		t.Fatal("Compare: correct password rejected")
	}
	if Compare(hash, "wrong-password") {
		t.Fatal("Compare: wrong password accepted")
	}
}

func TestIssueVerifyRoundTrip(t *testing.T) {
	priv, pub := testKey(t)
	claims := AuthClaims{Subject: "user-1", OrgID: "org-1", Roles: []string{"owner", "admin"}, TTL: 15 * time.Minute}

	token, err := claims.Issue(priv)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	got, err := Verify(token, pub)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got["sub"] != "user-1" {
		t.Fatalf("claims mismatch: %#v", got)
	}
	orgClaims, ok := got[hasuraClaimsKey].(map[string]any)
	if !ok {
		t.Fatalf("missing Hasura claims block")
	}
	if orgClaims["x-hasura-org-id"] != "org-1" || orgClaims["x-hasura-default-role"] != "owner" {
		t.Fatalf("Hasura claims mismatch: %#v", orgClaims)
	}
}

func TestVerifyRejectsTamperAndWrongKey(t *testing.T) {
	priv, pub := testKey(t)
	token, err := AuthClaims{Subject: "u", OrgID: "o", Roles: []string{"owner"}, TTL: time.Hour}.Issue(priv)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Tamper with the payload (middle segment).
	bad := token[:len(token)-6] + "AAAAAA"
	if _, err := Verify(bad, pub); err == nil {
		t.Fatal("Verify accepted a tampered token")
	}

	// Wrong key must not validate.
	_, otherPub := testKey(t)
	if _, err := Verify(token, otherPub); err == nil {
		t.Fatal("Verify accepted a token signed by another key")
	}
}

func TestLoadPrivateKeyPKCS8PEM(t *testing.T) {
	priv, _ := testKey(t)

	blockBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal PKCS#8: %v", err)
	}
	block := &pem.Block{Type: "PRIVATE KEY", Bytes: blockBytes}
	data := pem.EncodeToMemory(block)
	if data == nil {
		t.Fatal("failed to encode PKCS#8 PEM")
	}
	loaded, err := loadPrivateKeyBytes(data)
	if err != nil {
		t.Fatalf("loadPrivateKey: %v", err)
	}
	if loaded.N.Cmp(priv.N) != 0 {
		t.Fatal("loaded key modulus does not match original")
	}
}
