package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

const bridgeTimeout = 180 * time.Second

var (
	ErrBridgeUnreachable = errors.New("bridge_unreachable")
	errBadBridgeResponse = errors.New("bad_bridge_response")
)

// knownBridgeErrors are the error codes the API returns; anything else is internal.
var knownBridgeErrors = []error{
	ErrUnauthorized, ErrInvalidRequest, ErrNotFound, ErrNotPaired, ErrApprovalRequired, ErrApprovalInvalid,
	ErrApprovalExpired, ErrApprovalUsed, ErrBusy, ErrClosed, ErrSendUncertain, ErrSendFailed,
	ErrMediaNotFound, ErrUnsupportedMedia, ErrSecretRejected,
}

// BridgeClient is the MCP process's connection to the bridge's loopback API.
// It never uses a proxy, never follows redirects and never retries a request.
type BridgeClient struct {
	baseURL string // where to connect; tests point it at a test server
	token   string
	http    *http.Client
}

func NewBridgeClient(token []byte) *BridgeClient {
	return &BridgeClient{
		baseURL: "http://" + apiAddress,
		token:   hex.EncodeToString(token),
		http: &http.Client{
			Timeout:       bridgeTimeout,
			Transport:     &http.Transport{Proxy: nil, MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Send asks the bridge to send an approved message. A connection lost after
// the request was written leaves the outcome uncertain.
func (c *BridgeClient) Send(ctx context.Context, input SendInput, approval *ApprovalProof) (SendReceipt, error) {
	var receipt SendReceipt
	err := c.do(ctx, http.MethodPost, "/api/send", SendRequest{Input: input, Approval: approval}, &receipt)
	if errors.Is(err, errRequestNotDelivered) {
		return receipt, ErrBridgeUnreachable
	}
	if errors.Is(err, errRequestInterrupted) || errors.Is(err, errBadBridgeResponse) {
		return receipt, ErrSendUncertain
	}
	return receipt, err
}

func (c *BridgeClient) Read(ctx context.Context, tool string, request, result any) error {
	if !readToolName.MatchString(tool) {
		return ErrInvalidRequest
	}
	return c.transportError(c.do(ctx, http.MethodPost, "/api/read/"+tool, request, result))
}

func (c *BridgeClient) Download(ctx context.Context, messageID, chatJID string) (DownloadedMedia, error) {
	var media DownloadedMedia
	request := map[string]string{"message_id": messageID, "chat_jid": chatJID}
	return media, c.transportError(c.do(ctx, http.MethodPost, "/api/download", request, &media))
}

var (
	errRequestNotDelivered = errors.New("request not delivered")
	errRequestInterrupted  = errors.New("request interrupted")
)

func (c *BridgeClient) transportError(err error) error {
	if errors.Is(err, errRequestNotDelivered) || errors.Is(err, errRequestInterrupted) {
		return ErrBridgeUnreachable
	}
	return err
}

func (c *BridgeClient) do(ctx context.Context, method, path string, body, result any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return ErrInvalidRequest
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return ErrInvalidRequest
	}
	req.Host = apiAddress // the bridge only answers to its own address
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		var netErr *net.OpError
		if errors.As(err, &netErr) && netErr.Op == "dial" {
			return errRequestNotDelivered
		}
		return errRequestInterrupted
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return errRequestInterrupted
	}
	if len(data) > maxResponseBytes {
		return errBadBridgeResponse
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return bridgeError(data)
	}
	if result != nil && json.Unmarshal(data, result) != nil {
		return errBadBridgeResponse
	}
	return nil
}

func bridgeError(data []byte) error {
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &body) == nil {
		for _, known := range knownBridgeErrors {
			if body.Error == known.Error() {
				return known
			}
		}
	}
	return errBadBridgeResponse
}
