package sftprelay

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pelican/wings/remote"
)

const testUser = "alice.1a2b3c4d"

func testResp() *remote.SftpAuthResponse {
	return &remote.SftpAuthResponse{Server: "1a2b3c4d-0000-4000-8000-000000000001", User: "u-1", Permissions: []string{"file.read", "file.update"}}
}

// A credential issued by one replica verifies on another replica that shares
// the node token: the agent's /sftp/auth call may reach any replica.
func TestSessionsVerifyAcrossReplicas(t *testing.T) {
	a, b := NewSessions("node-token"), NewSessions("node-token")
	cred := a.Issue(testUser, testResp())
	got, ok := b.Lookup(testUser, cred)
	if !ok {
		t.Fatal("credential from another replica rejected")
	}
	if !reflect.DeepEqual(got, testResp()) {
		t.Fatalf("got %+v, want %+v", got, testResp())
	}
	if a.Issue(testUser, testResp()) == cred {
		t.Fatal("credentials must be unique per login")
	}
}

func TestSessionsRejects(t *testing.T) {
	s := NewSessions("node-token")
	cred := s.Issue(testUser, testResp())

	if _, ok := NewSessions("other-token").Lookup(testUser, cred); ok {
		t.Fatal("credential accepted under a different node token")
	}
	if _, ok := s.Lookup("bob.1a2b3c4d", cred); ok {
		t.Fatal("credential accepted for another username")
	}
	for _, bad := range []string{"", "nope", sessionPrefix, sessionPrefix + "x.y", strings.TrimPrefix(cred, sessionPrefix), cred + "x", cred[:len(cred)-2]} {
		if _, ok := s.Lookup(testUser, bad); ok {
			t.Fatalf("malformed credential %q accepted", bad)
		}
	}

	// Changing the sealed claims (here: the permissions) breaks the MAC.
	payload, sig, _ := strings.Cut(strings.TrimPrefix(cred, sessionPrefix), ".")
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	var c sessionClaims
	_ = json.Unmarshal(raw, &c)
	c.Permissions = []string{"*"}
	raw, _ = json.Marshal(c)
	forged := sessionPrefix + base64.RawURLEncoding.EncodeToString(raw) + "." + sig
	if _, ok := s.Lookup(testUser, forged); ok {
		t.Fatal("credential with altered claims accepted")
	}
}

func TestSessionsExpire(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := NewSessions("node-token")
	s.now = func() time.Time { return now }
	cred := s.Issue(testUser, testResp())
	now = now.Add(sessionTTL - time.Second)
	if _, ok := s.Lookup(testUser, cred); !ok {
		t.Fatal("credential rejected before expiry")
	}
	now = now.Add(time.Second)
	if _, ok := s.Lookup(testUser, cred); ok {
		t.Fatal("expired credential accepted")
	}
}
