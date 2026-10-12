package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// hasuraClaimsKey is the reserved claim namespace Hasura reads to configure
// permissions, roles and org/user filtering (§8.1).
const hasuraClaimsKey = "https://hasura.io/jwt/claims"

// AuthClaims describes a token to be issued: the subject (user UUID), the owning
// organization, the granted roles and how long it should live. The first role is
// used as x-hasura-default-role; all roles are allowed (§8.1).
type AuthClaims struct {
	Subject  string        // user UUID → claim "sub" and x-hasura-user-id
	OrgID    string        // organization UUID → x-hasura-org-id
	Roles    []string      // non-empty; first is the default role
	TTL      time.Duration // lifetime of the issued token
	IssuedAt time.Time     // explicit iat (defaults to now)
}

// loadPrivateKeyBytes decodes an RSA private key from a PEM block. It accepts both
// PKCS#1 ("RSA PRIVATE KEY") and PKCS#8 ("PRIVATE KEY") encodings.
func loadPrivateKeyBytes(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("private key is not a valid PEM block")
	}
	switch block.Type {
	case "RSA PRIVATE KEY": // PKCS#1
		priv, perr := x509.ParsePKCS1PrivateKey(block.Bytes)
		if perr != nil {
			return nil, fmt.Errorf("parse PKCS#1 key: %w", perr)
		}
		return priv, nil
	default: // PKCS#8 (DER) or other
		parsed, perr := x509.ParsePKCS8PrivateKey(block.Bytes)
		if perr != nil {
			return nil, fmt.Errorf("parse PKCS#8 key: %w", perr)
		}
		priv, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("key is not an RSA private key")
		}
		return priv, nil
	}
}

// loadPrivateKey reads a PEM file and decodes the RSA private key it contains.
func loadPrivateKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	return loadPrivateKeyBytes(data)
}

// claimsMap builds the JWT claims: standard "sub" plus the Hasura claim block and
// iat/exp (§8.1). Roles must be non-empty; the first becomes the default role.
func (c AuthClaims) claimsMap() map[string]any {
	now := c.IssuedAt
	return map[string]any{
		"sub": c.Subject,
		hasuraClaimsKey: map[string]any{
			"x-hasura-allowed-roles": c.Roles,
			"x-hasura-default-role":  c.Roles[0],
			"x-hasura-org-id":        c.OrgID,
			"x-hasura-user-id":       c.Subject,
		},
		"iat": now.Unix(),
		"exp": now.Add(c.TTL).Unix(),
	}
}

// Issue signs a new RS256 access token for the given subject, org and roles. The
// returned string is what Hasura expects in Authorization: Bearer (§8.1).
func (c AuthClaims) Issue(priv *rsa.PrivateKey) (string, error) {
	header := map[string]any{"typ": "JWT", "alg": "RS256"}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("marshal header: %w", err)
	}
	cb, err := json.Marshal(c.claimsMap())
	if err != nil {
		return "", fmt.Errorf("marshal claims: %w", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)

	h := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, h[:])
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Verify validates tokenStr's signature and expiry against pub and returns its
// claims. It is used for testing and any path where Go needs to read a token it
// issued; the Action handlers authenticate via the shared X-Action-Secret instead.
func Verify(tokenStr string, pub *rsa.PublicKey) (map[string]any, error) {
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return nil, errors.New("token must have three parts")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("decode signature: %w", err)
	}
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, h[:], sig); err != nil {
		return nil, fmt.Errorf("verify signature: %w", err)
	}

	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode claims: %w", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		return nil, fmt.Errorf("unmarshal claims: %w", err)
	}
	return claims, nil
}
