package capacity

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

// targetClients returns the phone client and a separate MCP client for one
// owned proxy address. A desktop MCP client is a different client from the
// phones, so it gets its own transport and connection pool: MCP requests
// never borrow, evict or wait for a phone connection. Both are pinned to
// sync.example.test:443 on the owned proxy and refuse redirects.
func targetClients(tlsConfig *tls.Config, address string) (phones, mcp *http.Client) {
	client := func() *http.Client {
		transport := &http.Transport{TLSClientConfig: tlsConfig.Clone(), DialContext: func(ctx context.Context, network, addressIgnored string) (net.Conn, error) {
			if addressIgnored != "sync.example.test:443" {
				return nil, ErrUnsafeTarget
			}
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}}
		return &http.Client{Timeout: 10 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrUnsafeTarget }}
	}
	return client(), client()
}
