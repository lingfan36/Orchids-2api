package client

import (
	"bufio"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/store"
)

const (
	wsBaseURL  = "wss://orchids-server.calmstone-6964e08a.westeurope.azurecontainerapps.io/agent/ws/coding"
	apiVersion = "5"
)

type Client struct {
	config  *config.Config
	account *store.Account
	store   *store.Store
}

type AgentRequest struct {
	ProjectID             string        `json:"projectId"`
	Prompt                string        `json:"prompt"`
	AgentMode             string        `json:"agentMode"`
	Mode                  string        `json:"mode"`
	ChatHistory           []interface{} `json:"chatHistory"`
	ChatSessionID         string        `json:"chatSessionId,omitempty"`
	AttachmentUrls        []string      `json:"attachmentUrls,omitempty"`
	FilesInSession        []string      `json:"filesInSession,omitempty"`
	CurrentPage           interface{}   `json:"currentPage,omitempty"`
	Email                 string        `json:"email,omitempty"`
	IsLocal               bool          `json:"isLocal"`
	LocalWorkingDirectory string        `json:"localWorkingDirectory,omitempty"`
	UserID                string        `json:"userId,omitempty"`
	TemplateID            string        `json:"templateId,omitempty"`
}

type SSEMessage struct {
	Type  string                 `json:"type"`
	Event map[string]interface{} `json:"event,omitempty"`
	Raw   map[string]interface{} `json:"-"`
}

// wsFrame is the WebSocket protocol message sent to upstream
type wsFrame struct {
	Type      string      `json:"type"`
	Data      interface{} `json:"data"`
	RequestID string      `json:"requestId"`
}

func New(cfg *config.Config) *Client {
	return &Client{
		config: cfg,
	}
}

func NewFromAccount(acc *store.Account, s *store.Store) *Client {
	cfg := &config.Config{
		SessionID:    acc.SessionID,
		ClientCookie: acc.ClientCookie,
		ClientUat:    acc.ClientUat,
		ProjectID:    acc.ProjectID,
		UserID:       acc.UserID,
		AgentMode:    acc.AgentMode,
		Email:        acc.Email,
	}
	return &Client{
		config:  cfg,
		account: acc,
		store:   s,
	}
}

// refreshAndGetToken 通过 client_cookie 从 clerk 获取最新的 JWT
func (c *Client) refreshAndGetToken() (string, error) {
	clerkURL := "https://clerk.orchids.app/v1/client?__clerk_api_version=2025-11-10&_clerk_js_version=5.117.0"

	req, err := http.NewRequest("GET", clerkURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create clerk request: %w", err)
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Orchids/0.0.57 Chrome/138.0.7204.251 Electron/37.10.3 Safari/537.36")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "zh-CN")
	req.Header.Set("Cookie", "__client="+c.config.ClientCookie+"; __client_uat="+c.config.ClientUat)

	httpClient := &http.Client{Timeout: 15 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to call clerk: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read clerk response: %w", err)
	}

	// 从 Set-Cookie 更新 rotating __client token
	for _, cookie := range resp.Cookies() {
		switch cookie.Name {
		case "__client":
			if cookie.Value != "" {
				c.config.ClientCookie = cookie.Value
				if c.account != nil && c.account.ID > 0 && c.store != nil {
					if dbErr := c.store.UpdateClientCookie(c.account.ID, cookie.Value); dbErr != nil {
						log.Printf("警告: 更新数据库 client_cookie 失败: %v", dbErr)
					}
				}
			}
		case "__client_uat":
			if cookie.Value != "" {
				c.config.ClientUat = cookie.Value
			}
		}
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("clerk returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var clientResp struct {
		Response struct {
			LastActiveSessionID string `json:"last_active_session_id"`
			Sessions            []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				LastActiveToken struct {
					JWT string `json:"jwt"`
				} `json:"last_active_token"`
			} `json:"sessions"`
		} `json:"response"`
	}

	if err := json.Unmarshal(bodyBytes, &clientResp); err != nil {
		return "", fmt.Errorf("failed to decode clerk response: %w\nbody: %s", err, string(bodyBytes))
	}

	if len(clientResp.Response.Sessions) == 0 {
		return "", fmt.Errorf("no active sessions found. body: %s", string(bodyBytes))
	}

	targetSessionID := clientResp.Response.LastActiveSessionID
	var jwt string
	for _, s := range clientResp.Response.Sessions {
		if s.ID == targetSessionID || targetSessionID == "" {
			jwt = s.LastActiveToken.JWT
			targetSessionID = s.ID
			break
		}
	}
	if jwt == "" {
		jwt = clientResp.Response.Sessions[0].LastActiveToken.JWT
		targetSessionID = clientResp.Response.Sessions[0].ID
	}
	if jwt == "" {
		return "", fmt.Errorf("JWT is empty in clerk response. body: %s", string(bodyBytes))
	}

	log.Printf("成功获取 JWT for %s (session: %s)", c.config.Email, targetSessionID)

	if targetSessionID != c.config.SessionID {
		c.config.SessionID = targetSessionID
		if c.account != nil && c.account.ID > 0 && c.store != nil {
			if dbErr := c.store.UpdateSessionID(c.account.ID, targetSessionID); dbErr != nil {
				log.Printf("警告: 更新数据库 session_id 失败: %v", dbErr)
			}
		}
	}

	return jwt, nil
}

func (c *Client) GetToken() (string, error) {
	return c.refreshAndGetToken()
}

// wsConnect 建立 WebSocket 连接（纯标准库，无外部依赖）
func wsConnect(wsURL string) (net.Conn, *bufio.Reader, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return nil, nil, fmt.Errorf("parse ws url: %w", err)
	}

	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}

	// TLS for wss://
	var conn net.Conn
	if u.Scheme == "wss" {
		conn, err = tlsDial(host, u.Hostname())
		if err != nil {
			return nil, nil, fmt.Errorf("tls dial %s: %w", host, err)
		}
	} else {
		conn, err = net.DialTimeout("tcp", host, 15*time.Second)
		if err != nil {
			return nil, nil, fmt.Errorf("tcp dial %s: %w", host, err)
		}
	}

	// Generate random WebSocket key
	keyBytes := make([]byte, 16)
	if _, err := cryptorand.Read(keyBytes); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("generate ws key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)

	// WebSocket HTTP/1.1 upgrade handshake
	path := u.RequestURI()
	handshake := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + u.Hostname() + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Origin: https://orchids.app\r\n" +
		"User-Agent: Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Orchids/0.0.57 Chrome/138.0.7204.251 Electron/37.10.3 Safari/537.36\r\n" +
		"\r\n"

	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if _, err := conn.Write([]byte(handshake)); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("ws handshake write: %w", err)
	}

	reader := bufio.NewReaderSize(conn, 65536)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("ws handshake read status: %w", err)
	}
	if !strings.Contains(statusLine, "101") {
		var sb strings.Builder
		sb.WriteString(statusLine)
		for {
			line, readErr := reader.ReadString('\n')
			sb.WriteString(line)
			if readErr != nil || strings.TrimSpace(line) == "" {
				break
			}
		}
		conn.Close()
		return nil, nil, fmt.Errorf("ws upgrade failed: %s", sb.String())
	}

	// Verify Sec-WebSocket-Accept and drain remaining headers
	expectedAccept := computeAcceptKey(key)
	var gotAccept string
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil || strings.TrimSpace(line) == "" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "sec-websocket-accept:") {
			gotAccept = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
		}
	}
	if gotAccept != "" && gotAccept != expectedAccept {
		conn.Close()
		return nil, nil, fmt.Errorf("ws accept key mismatch: got %s, want %s", gotAccept, expectedAccept)
	}

	// Clear deadline for subsequent reads/writes
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, reader, nil
}

func computeAcceptKey(key string) string {
	h := sha1.New()
	h.Write([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// wsSendText sends a masked text frame (masking is required client→server per RFC 6455)
func wsSendText(conn net.Conn, data []byte) error {
	mask := make([]byte, 4)
	if _, err := cryptorand.Read(mask); err != nil {
		return fmt.Errorf("generate mask: %w", err)
	}

	masked := make([]byte, len(data))
	for i, b := range data {
		masked[i] = b ^ mask[i%4]
	}

	var frame []byte
	frame = append(frame, 0x81) // FIN=1, opcode=1 (text)

	n := len(masked)
	switch {
	case n <= 125:
		frame = append(frame, byte(0x80|n))
	case n <= 65535:
		frame = append(frame, 0x80|126)
		frame = append(frame, byte(n>>8), byte(n))
	default:
		frame = append(frame, 0x80|127)
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, uint64(n))
		frame = append(frame, b...)
	}

	frame = append(frame, mask...)
	frame = append(frame, masked...)

	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	defer conn.SetDeadline(time.Time{})
	_, err := conn.Write(frame)
	return err
}

// wsReadFrame reads one WebSocket frame; returns (opcode, payload, err)
func wsReadFrame(reader *bufio.Reader) (byte, []byte, error) {
	b0, err := reader.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	b1, err := reader.ReadByte()
	if err != nil {
		return 0, nil, err
	}

	opcode := b0 & 0x0F
	isMasked := (b1 & 0x80) != 0
	payloadLen := int64(b1 & 0x7F)

	switch payloadLen {
	case 126:
		var l uint16
		if err := binary.Read(reader, binary.BigEndian, &l); err != nil {
			return 0, nil, err
		}
		payloadLen = int64(l)
	case 127:
		if err := binary.Read(reader, binary.BigEndian, &payloadLen); err != nil {
			return 0, nil, err
		}
	}

	var maskBytes [4]byte
	if isMasked {
		if _, err := io.ReadFull(reader, maskBytes[:]); err != nil {
			return 0, nil, err
		}
	}

	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}

	if isMasked {
		for i := range payload {
			payload[i] ^= maskBytes[i%4]
		}
	}

	return opcode, payload, nil
}

// newRequestID generates a UUID v4
func newRequestID() string {
	b := make([]byte, 16)
	cryptorand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (c *Client) SendRequest(ctx context.Context, promptText string, chatHistory []interface{}, model string, onMessage func(SSEMessage), logger *debug.Logger) error {
	token, err := c.GetToken()
	if err != nil {
		return fmt.Errorf("failed to get token: %w", err)
	}

	wsURL := wsBaseURL + "?token=" + url.QueryEscape(token) + "&orchids_api_version=" + apiVersion
	log.Printf("连接 WebSocket: %s", wsBaseURL)

	conn, reader, err := wsConnect(wsURL)
	if err != nil {
		return fmt.Errorf("ws connect failed: %w", err)
	}
	defer conn.Close()

	chatSessionID := newRequestID()
	// agentMode 是模型选择器（如 "claude-opus-4.6"），优先使用传入的 model 参数
	agentMode := model
	if agentMode == "" {
		agentMode = c.config.AgentMode
	}

	data := AgentRequest{
		ProjectID:     c.config.ProjectID,
		Prompt:        promptText,
		AgentMode:     agentMode,
		Mode:          "agent",
		ChatHistory:   chatHistory,
		ChatSessionID: chatSessionID,
		Email:         c.config.Email,
		IsLocal:       false,
		UserID:        c.config.UserID,
	}

	requestID := newRequestID()
	msg := wsFrame{
		Type:      "user_request",
		Data:      data,
		RequestID: requestID,
	}

	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal ws frame: %w", err)
	}

	if logger != nil {
		logger.LogUpstreamRequest(wsBaseURL, map[string]string{"type": "websocket"}, msg)
	}

	if err := wsSendText(conn, msgBytes); err != nil {
		return fmt.Errorf("ws send failed: %w", err)
	}
	log.Printf("已发送 user_request (requestId=%s)", requestID)

	// Read loop
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := conn.SetDeadline(time.Now().Add(120 * time.Second)); err != nil {
			return err
		}
		opcode, frameData, err := wsReadFrame(reader)
		conn.SetDeadline(time.Time{})

		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("ws read: %w", err)
		}

		switch opcode {
		case 0x8: // close
			log.Printf("WebSocket close frame received")
			return nil
		case 0x9: // ping → pong
			pongFrame := []byte{0x8A, 0x80, 0x00, 0x00, 0x00, 0x00}
			conn.Write(pongFrame)
			continue
		case 0xA: // pong
			continue
		case 0x0, 0x1, 0x2: // continuation, text, binary
			// fall through to parse
		default:
			continue
		}

		var raw map[string]interface{}
		if err := json.Unmarshal(frameData, &raw); err != nil {
			log.Printf("ws frame JSON parse error: %v, raw: %.200s", err, string(frameData))
			continue
		}

		msgType, _ := raw["type"].(string)

		if logger != nil {
			logger.LogUpstreamSSE(msgType, string(frameData))
		}
		log.Printf("[upstream ws] type=%s", msgType)

		switch msgType {
		case "complete":
			return nil

		case "error", "coding_agent.error":
			if d, ok := raw["data"].(map[string]interface{}); ok {
				if e, ok := d["error"].(string); ok {
					return fmt.Errorf("upstream error: %s", e)
				}
				if e, ok := d["message"].(string); ok {
					return fmt.Errorf("upstream error: %s", e)
				}
			}
			return fmt.Errorf("upstream error: %.500s", string(frameData))

		case "request_ack":
			log.Printf("request_ack received for %s", requestID)
			continue

		case "heartbeat":
			continue

		case "model":
			sseMsg := SSEMessage{
				Type: msgType,
				Raw:  raw,
			}
			if event, ok := raw["event"].(map[string]interface{}); ok {
				sseMsg.Event = event
			} else if eventData, ok := raw["data"].(map[string]interface{}); ok {
				sseMsg.Event = eventData
			}
			onMessage(sseMsg)

		default:
			// forward unknown event types so handler can decide
			sseMsg := SSEMessage{Type: msgType, Raw: raw}
			if event, ok := raw["event"].(map[string]interface{}); ok {
				sseMsg.Event = event
			}
			onMessage(sseMsg)
		}
	}
}
