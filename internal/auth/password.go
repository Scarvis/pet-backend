// Package auth implements registration, login and refresh-token rotation (§8.1),
// plus the RS256 JWTs that carry Hasura claims for org/user filtering.
package auth

import "golang.org/x/crypto/bcrypt"

// bcryptCost is the cost factor used when hashing passwords (≥ 12 per §8.1 / §2).
const bcryptCost = 12

// Hash returns a bcrypt hash of password using a cost factor of at least 12.
func Hash(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Compare reports whether password matches the stored hash in constant time.
// bcrypt.CompareHashAndPassword is already timing-safe internally.
func Compare(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}
