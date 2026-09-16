// Package jwtx verifies Panel-issued JWTs with the node token and re-signs
// their claims unchanged with an agent token (ARCHITECTURE.md 5.4).
package jwtx

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gbrlsnchs/jwt/v3"
)

// Claims are the fields the gateway itself needs to look at.
type Claims struct {
	jwt.Payload
	ServerUUID  string   `json:"server_uuid"`
	UserUUID    string   `json:"user_uuid"`
	Permissions []string `json:"permissions"`
	Scope       string   `json:"scope"`
	UniqueID    string   `json:"unique_id"`
}

// ErrExpired is returned for expired tokens.
var ErrExpired = errors.New("jwt: token expired")

// Verify checks the signature with key and the expiry, and returns the claims
// together with the raw payload for re-signing.
func Verify(token []byte, key []byte) (*Claims, json.RawMessage, error) {
	var raw json.RawMessage
	_, err := jwt.Verify(token, jwt.NewHS256(key), &raw)
	if err != nil {
		return nil, nil, fmt.Errorf("jwt: %w", err)
	}
	var c Claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, nil, fmt.Errorf("jwt: payload: %w", err)
	}
	if c.ExpirationTime != nil && time.Now().After(c.ExpirationTime.Time) {
		return nil, nil, ErrExpired
	}
	return &c, raw, nil
}

// Resign signs the raw payload with key, preserving every claim.
func Resign(raw json.RawMessage, key []byte) ([]byte, error) {
	return jwt.Sign(raw, jwt.NewHS256(key))
}

// HasPermission mirrors Wings' permission check ("*" grants non-admin permissions).
func (c *Claims) HasPermission(p string) bool {
	for _, k := range c.Permissions {
		if k == p || (k == "*" && !strings.HasPrefix(p, "admin.")) {
			return true
		}
	}
	return false
}
