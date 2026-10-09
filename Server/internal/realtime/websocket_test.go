package realtime

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/protocol"
)

func TestShutdownForceClosesUnresponsiveWebSocketTransport(t *testing.T) {
	hub, err := NewHub(context.Background(), HubOptions{Limits: protocol.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	hub.shutdownGrace = 25 * time.Millisecond

	bound := make(chan struct{})
	releaseHandler := make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, acceptErr := Accept(writer, request, "http://"+request.Host)
		if acceptErr != nil {
			t.Errorf("accept websocket: %v", acceptErr)
			return
		}
		reservation, reserveErr := hub.Reserve(request.Context(), principal(testDevice), grant())
		if reserveErr != nil {
			t.Errorf("reserve: %v", reserveErr)
			return
		}
		cleanup, bindErr := reservation.Bind(connection)
		if bindErr != nil {
			t.Errorf("bind: %v", bindErr)
			return
		}
		defer cleanup()
		close(bound)
		<-releaseHandler
	}))
	server.Config.ConnContext = TransportContext
	server.Start()
	defer func() {
		close(releaseHandler)
		server.CloseClientConnections()
		server.Close()
	}()

	peer := dialUnresponsiveWebSocket(t, server.Listener.Addr().String())
	defer peer.Close()
	select {
	case <-bound:
	case <-time.After(time.Second):
		t.Fatal("websocket did not bind")
	}

	shutdownCtx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := hub.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 500*time.Millisecond {
		t.Fatalf("shutdown exceeded bound: %s", elapsed)
	}

	if err := peer.SetReadDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 512)
	for {
		_, err = peer.Read(buffer)
		if err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatal("shutdown left unresponsive websocket transport open")
			}
			break
		}
	}
}

func dialUnresponsiveWebSocket(t *testing.T, address string) net.Conn {
	t.Helper()
	peer, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		peer.Close()
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])
	request := fmt.Sprintf("GET / HTTP/1.1\r\nHost: %s\r\nOrigin: http://%s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Protocol: %s\r\n\r\n", address, address, key, Subprotocol)
	if _, err = peer.Write([]byte(request)); err != nil {
		peer.Close()
		t.Fatal(err)
	}
	reader := bufio.NewReader(peer)
	status, err := reader.ReadString('\n')
	if err != nil {
		peer.Close()
		t.Fatal(err)
	}
	if !strings.Contains(status, " 101 ") {
		peer.Close()
		t.Fatalf("websocket status=%q", strings.TrimSpace(status))
	}
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			peer.Close()
			t.Fatal(readErr)
		}
		if line == "\r\n" {
			return peer
		}
	}
}
