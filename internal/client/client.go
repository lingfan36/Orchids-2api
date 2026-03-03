package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/store"
)

const upstreamURL = "https://orchids-server.calmstone-6964e08a.westeurope.azurecontainerapps.io/agent/coding-agent"

type Client struct {
	config     *config.Config
	account    *store.Account
	store      *store.Store
	httpClient *http.Client
}

type TokenResponse struct {
	JWT string `json:"jwt"`
}

type AgentRequest struct {
	Prompt        string        `json:"prompt"`
	ChatHistory   []interface{} `json:"chatHistory"`
	ProjectID     string        `json:"projectId"`
	CurrentPage   interface{}   `json:"currentPage"`
	AgentMode     string        `json:"agentMode"`
	Mode          string        `json:"mode"`
	GitRepoUrl    string        `json:"gitRepoUrl"`
	Email         string        `json:"email"`
	ChatSessionID int           `json:"chatSessionId"`
	UserID        string        `json:"userId"`
	APIVersion    int           `json:"apiVersion"`
	Model         string        `json:"model,omitempty"`
}

type SSEMessage struct {
	Type  string                 `json:"type"`
	Event map[string]interface{} `json:"event,omitempty"`
	Raw   map[string]interface{} `json:"-"`
}

func New(cfg *config.Config) *Client {
	return &Client{
		config:     cfg,
		httpClient: &http.Client{},
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
		config:     cfg,
		account:    acc,
		store:      s,
		httpClient: &http.Client{},
	}
}

// refreshAndGetToken 通过 client_cookie 从 clerk 获取最新的 session、更新 rotating token，
// 并直接从响应里取出 JWT，避免二次请求。
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

	httpClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to call clerk: %w", err)
	}
	defer resp.Body.Close()

	// 先把 body 读出来，防止多次读取
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read clerk response: %w", err)
	}

	// 从 Set-Cookie 更新 rotating __client token 和 __client_uat
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

	// 找到 last_active_session 对应的 session，优先取它的 JWT
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
		// fallback: 取第一个 session 的 JWT
		jwt = clientResp.Response.Sessions[0].LastActiveToken.JWT
		targetSessionID = clientResp.Response.Sessions[0].ID
	}
	if jwt == "" {
		return "", fmt.Errorf("JWT is empty in clerk response. body: %s", string(bodyBytes))
	}

	log.Printf("成功获取 JWT for %s (session: %s)", c.config.Email, targetSessionID)

	// 持久化最新 session_id
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

func (c *Client) SendRequest(ctx context.Context, prompt string, chatHistory []interface{}, model string, onMessage func(SSEMessage), logger *debug.Logger) error {
	token, err := c.GetToken()
	if err != nil {
		return fmt.Errorf("failed to get token: %w", err)
	}

	payload := AgentRequest{
		Prompt:        prompt,
		ChatHistory:   chatHistory,
		ProjectID:     c.config.ProjectID,
		CurrentPage:   map[string]interface{}{},
		AgentMode:     c.config.AgentMode,
		Mode:          "agent",
		GitRepoUrl:    "",
		Email:         c.config.Email,
		ChatSessionID: rand.IntN(90000000) + 10000000,
		UserID:        c.config.UserID,
		APIVersion:    2,
		Model:         model,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", upstreamURL, bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Orchids-Api-Version", "2")

	// 记录上游请求
	if logger != nil {
		headers := map[string]string{
			"Accept":              "text/event-stream",
			"Authorization":       "Bearer [REDACTED]",
			"Content-Type":        "application/json",
			"X-Orchids-Api-Version": "2",
		}
		logger.LogUpstreamRequest(upstreamURL, headers, payload)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upstream request failed with status %d: %s", resp.StatusCode, string(body))
	}

	reader := bufio.NewReader(resp.Body)
	var buffer strings.Builder

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		buffer.WriteString(line)

		if line == "\n" {
			eventData := buffer.String()
			buffer.Reset()

			lines := strings.Split(eventData, "\n")
			for _, l := range lines {
				if !strings.HasPrefix(l, "data: ") {
					continue
				}
				rawData := strings.TrimPrefix(l, "data: ")

				var msg map[string]interface{}
				if err := json.Unmarshal([]byte(rawData), &msg); err != nil {
					continue
				}

				msgType, _ := msg["type"].(string)

				// 记录上游 SSE
				if logger != nil {
					logger.LogUpstreamSSE(msgType, rawData)
				}

				// 非 model 类型打印到日志方便排查
				if msgType != "model" {
					log.Printf("[upstream] type=%s data=%s", msgType, rawData)
					continue
				}

				sseMsg := SSEMessage{
					Type: msgType,
					Raw:  msg,
				}
				if event, ok := msg["event"].(map[string]interface{}); ok {
					sseMsg.Event = event
				}
				onMessage(sseMsg)
			}
		}
	}

	return nil
}
