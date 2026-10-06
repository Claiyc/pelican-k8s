package wsproxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// runPump runs fn in a goroutine and returns a function that waits for it to
// return, failing the test when it does not.
func runPump(t *testing.T, fn func()) (wait func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	return func() {
		t.Helper()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("pump did not stop")
		}
	}
}

func expectClose(t *testing.T, c *websocket.Conn, code int) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err := c.ReadMessage()
	var ce *websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("read error %v, want close code %d", err, code)
	}
}

func TestPumpClientToAgentFrames(t *testing.T) {
	s, browser, agent := newTestSession(t)
	wait := runPump(t, func() { s.pumpClientToAgent(context.Background()) })

	// Binary frames are dropped; unparseable text goes through untouched.
	if err := browser.WriteMessage(websocket.BinaryMessage, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if err := browser.WriteMessage(websocket.TextMessage, []byte("not json")); err != nil {
		t.Fatal(err)
	}
	_ = agent.SetReadDeadline(time.Now().Add(2 * time.Second))
	if mt, data, err := agent.ReadMessage(); err != nil || mt != websocket.TextMessage || string(data) != "not json" {
		t.Fatalf("agent got %d %q, %v; want the unparseable text frame", mt, data, err)
	}

	// A power request before any auth is refused by setState, not forwarded.
	if err := browser.WriteJSON(Message{Event: "set state", Args: []string{"start"}}); err != nil {
		t.Fatal(err)
	}
	if m := readMessage(t, browser); m.Event != "jwt error" || len(m.Args) != 1 || m.Args[0] != "jwt: no jwt present" {
		t.Fatalf("browser got %+v", m)
	}

	// The browser going away closes the agent side normally. The close frame
	// is the next thing the agent sees, so neither the dropped binary frame nor
	// the refused power request was forwarded. (A read timeout would poison
	// the connection, so this cannot use expectNothing.)
	_ = browser.Close()
	expectClose(t, agent, websocket.CloseNormalClosure)
	wait()
}

func TestPumpClientToAgentStopsOnAgentWriteError(t *testing.T) {
	t.Run("passthrough", func(t *testing.T) {
		s, browser, _ := newTestSession(t)
		_ = s.agent.Close()
		wait := runPump(t, func() { s.pumpClientToAgent(context.Background()) })
		if err := browser.WriteJSON(Message{Event: "send command", Args: []string{"say hi"}}); err != nil {
			t.Fatal(err)
		}
		wait()
	})
	t.Run("auth", func(t *testing.T) {
		s, browser, _ := newTestSession(t)
		_ = s.agent.Close()
		wait := runPump(t, func() { s.pumpClientToAgent(context.Background()) })
		tok := signToken(t, testNodeToken, testUUID, time.Now().Add(10*time.Minute))
		if err := browser.WriteJSON(Message{Event: "auth", Args: []string{tok}}); err != nil {
			t.Fatal(err)
		}
		wait()
	})
}

func TestPumpAgentToClient(t *testing.T) {
	s, browser, agent := newTestSession(t)
	wait := runPump(t, s.pumpAgentToClient)

	if err := agent.WriteMessage(websocket.TextMessage, []byte(`{"event":"console output","args":["hi"]}`)); err != nil {
		t.Fatal(err)
	}
	if m := readMessage(t, browser); m.Event != "console output" || m.Args[0] != "hi" {
		t.Fatalf("browser got %+v", m)
	}
	if err := agent.WriteMessage(websocket.BinaryMessage, []byte{9, 8}); err != nil {
		t.Fatal(err)
	}
	_ = browser.SetReadDeadline(time.Now().Add(2 * time.Second))
	if mt, data, err := browser.ReadMessage(); err != nil || mt != websocket.BinaryMessage || len(data) != 2 {
		t.Fatalf("browser got %d %v, %v; want the binary frame", mt, data, err)
	}

	// The agent's close code and reason reach the browser.
	err := agent.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4000, "bye"), time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	expectClose(t, browser, 4000)
	wait()
}

func TestPumpAgentToClientStopsWhenAgentDropsAbruptly(t *testing.T) {
	s, _, agent := newTestSession(t)
	wait := runPump(t, s.pumpAgentToClient)
	_ = agent.UnderlyingConn().Close()
	wait()
}

func TestPumpAgentToClientStopsOnClientWriteError(t *testing.T) {
	s, _, agent := newTestSession(t)
	_ = s.client.Close()
	wait := runPump(t, s.pumpAgentToClient)
	if err := agent.WriteMessage(websocket.TextMessage, []byte("x")); err != nil {
		t.Fatal(err)
	}
	wait()
}

// run relays both directions and returns as soon as either one ends.
func TestRunRelaysUntilEitherSideCloses(t *testing.T) {
	s, browser, agent := newTestSession(t)
	wait := runPump(t, func() { s.run(context.Background()) })

	const cmd = `{"event":"send command","args":["list"]}`
	if err := browser.WriteMessage(websocket.TextMessage, []byte(cmd)); err != nil {
		t.Fatal(err)
	}
	_ = agent.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, data, err := agent.ReadMessage(); err != nil || string(data) != cmd {
		t.Fatalf("agent got %q, %v", data, err)
	}
	if err := agent.WriteJSON(Message{Event: "console output", Args: []string{"ok"}}); err != nil {
		t.Fatal(err)
	}
	if m := readMessage(t, browser); m.Event != "console output" {
		t.Fatalf("browser got %+v", m)
	}

	_ = browser.Close()
	wait()
}
