package jwtx

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gbrlsnchs/jwt/v3"
)

type testClaims struct {
	jwt.Payload
	ServerUUID  string   `json:"server_uuid"`
	UserUUID    string   `json:"user_uuid"`
	Permissions []string `json:"permissions"`
	Scope       string   `json:"scope"`
	UniqueID    string   `json:"unique_id"`
	Extra       string   `json:"file_path"`
}

func TestVerifyAndResign(t *testing.T) {
	node, agent := []byte("node-token"), []byte("agent-token")
	now := time.Now()
	in := testClaims{
		Payload:    jwt.Payload{Issuer: "https://panel", Audience: jwt.Audience{"https://wings"}, JWTID: "jti", IssuedAt: jwt.NumericDate(now), NotBefore: jwt.NumericDate(now.Add(-5 * time.Minute)), ExpirationTime: jwt.NumericDate(now.Add(10 * time.Minute))},
		ServerUUID: "srv", UserUUID: "usr", Permissions: []string{"websocket.connect", "*"}, Scope: "websocket", UniqueID: "u1", Extra: "/x.txt",
	}
	tok, err := jwt.Sign(in, jwt.NewHS256(node))
	if err != nil {
		t.Fatal(err)
	}
	claims, raw, err := Verify(tok, node)
	if err != nil {
		t.Fatal(err)
	}
	if claims.ServerUUID != "srv" || claims.UserUUID != "usr" || claims.Scope != "websocket" || !claims.HasPermission("websocket.connect") {
		t.Fatalf("claims %+v", claims)
	}
	if claims.HasPermission("admin.websocket.errors") {
		t.Fatal("* must not grant admin permissions")
	}
	if !claims.HasPermission("control.start") {
		t.Fatal("* grants non-admin permissions")
	}
	re, err := Resign(raw, agent)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Verify(re, node); err == nil {
		t.Fatal("re-signed token must not verify with the node key")
	}
	var out testClaims
	if _, err := jwt.Verify(re, jwt.NewHS256(agent), &out); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(in)
	b, _ := json.Marshal(out)
	if string(a) != string(b) {
		t.Fatalf("claims changed:\n%s\n%s", a, b)
	}
	// Tampered / wrong key / expired.
	if _, _, err := Verify([]byte(strings.Replace(string(tok), "a", "b", 1)), node); err == nil {
		t.Fatal("tampered token accepted")
	}
	in.ExpirationTime = jwt.NumericDate(now.Add(-time.Minute))
	tok, _ = jwt.Sign(in, jwt.NewHS256(node))
	if _, _, err := Verify(tok, node); err == nil {
		t.Fatal("expired token accepted")
	}
}
