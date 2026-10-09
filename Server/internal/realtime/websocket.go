package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"

	"github.com/coder/websocket"
)

const Subprotocol = "baby-tracker.v1"

type transportContextKey struct{}

type WebSocketConnection struct {
	connection *websocket.Conn
	transport  net.Conn
}

// TransportContext retains the underlying HTTP transport so a hijacked
// websocket can be terminated even while its graceful close handshake is
// waiting on an unresponsive peer.
func TransportContext(ctx context.Context, transport net.Conn) context.Context {
	return context.WithValue(ctx, transportContextKey{}, transport)
}

func Accept(w http.ResponseWriter, request *http.Request, canonicalOrigin string) (*WebSocketConnection, error) {
	origin, err := url.Parse(canonicalOrigin)
	if err != nil || origin.Scheme == "" || origin.Host == "" {
		return nil, errors.New("invalid canonical websocket origin")
	}
	connection, err := websocket.Accept(w, request, &websocket.AcceptOptions{
		Subprotocols: []string{Subprotocol}, OriginPatterns: []string{origin.Host}, CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, err
	}
	connection.SetReadLimit(1024)
	transport, _ := request.Context().Value(transportContextKey{}).(net.Conn)
	return &WebSocketConnection{connection: connection, transport: transport}, nil
}

func (connection *WebSocketConnection) WriteHint(ctx context.Context, hint Hint) error {
	raw, err := json.Marshal(hint)
	if err != nil {
		return err
	}
	return connection.connection.Write(ctx, websocket.MessageText, raw)
}

func (connection *WebSocketConnection) Ping(ctx context.Context) error {
	return connection.connection.Ping(ctx)
}

func (connection *WebSocketConnection) Close(code int, reason string) error {
	return connection.connection.Close(websocket.StatusCode(code), reason)
}

func (connection *WebSocketConnection) Abort() error {
	if connection.transport != nil {
		_ = connection.transport.Close()
	}
	return connection.connection.CloseNow()
}

func (connection *WebSocketConnection) ReadDone(ctx context.Context) context.Context {
	return connection.connection.CloseRead(ctx)
}
