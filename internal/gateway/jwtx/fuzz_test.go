package jwtx

import (
	"testing"
	"time"

	"github.com/gbrlsnchs/jwt/v3"
)

// Verify is the gateway's outermost trust boundary: the token arrives in a
// browser query string. No input may panic, and none may verify under a key it
// was not signed with.
func FuzzVerify(f *testing.F) {
	key := []byte("node-token")
	valid, err := jwt.Sign(
		testClaims{
			Payload:    jwt.Payload{Issuer: "https://panel", ExpirationTime: jwt.NumericDate(time.Now().Add(time.Hour))},
			ServerUUID: "srv", Permissions: []string{"*"}, Scope: "websocket",
		},
		jwt.NewHS256(key),
	)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(valid)
	f.Add([]byte("eyJhbGciOiJub25lIn0.eyJzZXJ2ZXJfdXVpZCI6InNydiJ9."))
	f.Add([]byte("a.b.c"))
	f.Add([]byte(""))
	f.Add([]byte("....."))

	f.Fuzz(func(t *testing.T, token []byte) {
		claims, raw, err := Verify(token, key)
		if err != nil {
			return
		}
		if claims == nil || raw == nil {
			t.Fatalf("Verify returned no error but claims=%v raw=%v", claims, raw)
		}
		// Anything that verified must re-sign, and must not verify under a
		// different key.
		if _, err := Resign(raw, []byte("agent-token")); err != nil {
			t.Fatalf("Resign accepted payload %s: %v", raw, err)
		}
		if _, _, err := Verify(token, []byte("wrong-key")); err == nil {
			t.Fatalf("token %q verified under the wrong key", token)
		}
	})
}
