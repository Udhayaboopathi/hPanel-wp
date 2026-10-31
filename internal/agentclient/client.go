// Package agentclient is the panel-side client for the provisioning agent.
// It speaks HTTP over the agent's Unix socket and sends a bearer token read
// from a 0600 token file. The panel process never opens the Docker socket.
package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Udhayaboopathi/hPanel-wp/internal/agentproto"
)

type Client struct {
	SocketPath string
	Token      string
	http       *http.Client
}

func New(socketPath, tokenFile string) (*Client, error) {
	token := ""
	if tokenFile != "" {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			return nil, fmt.Errorf("reading agent token: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{SocketPath: socketPath, Token: token, http: &http.Client{Transport: tr, Timeout: 10 * time.Minute}}, nil
}

func (c *Client) Available() bool {
	_, err := os.Stat(c.SocketPath)
	return err == nil
}

func (c *Client) Call(ctx context.Context, req agentproto.Request) (*agentproto.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", "http://agent/execute", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("provisioning agent unreachable at %s: %w", c.SocketPath, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	var out agentproto.Response
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("agent returned invalid response (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode >= 300 && out.Error == "" {
		out.Error = fmt.Sprintf("agent HTTP %d", resp.StatusCode)
	}
	return &out, nil
}
