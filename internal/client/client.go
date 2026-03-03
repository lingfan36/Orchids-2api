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

// refreshSessionID 通过 client_cookie 从 clerk 获取最新的 session_id
func (c *Client) refreshSessionID() error {
	url := "https://clerk.orchids.app/v1/client?__clerk_api_version=2025-11-10&_clerk_js_version=5.117.0"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create refresh request: %w", err)
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Orchids/0.0.57 Chrome/138.0.7204.251 Electron/37.10.3 Safari/537.36")
	req.Header.Set("Accept-Language", "zh-CN")
	req.AddCookie(&http.Cookie{Name: "__client", Value: c.config.ClientCookie})

	httpClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to refresh session: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("refresh session failed with status %d: %s", resp.StatusCode, string(body))
	}

	var clientResp struct {
		Response struct {
			LastActiveSessionID string `json:"last_active_session_id"`
			Sessions            []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				User   struct {
					ID string `json:"id"`
				} `json:"user"`
			} `json:"sessions"`
		} `json:"response"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&clientResp); err != nil {
		return fmt.Errorf("failed to decode refresh response: %w", err)
	}

	if len(clientResp.Response.Sessions) == 0 {
		return fmt.Errorf("no active sessions found after refresh")
	}

	newSessionID := clientResp.Response.LastActiveSessionID
	if newSessionID == "" {
		newSessionID = clientResp.Response.Sessions[0].ID
	}

	if newSessionID != c.config.SessionID {
		log.Printf("session 已刷新: %s -> %s", c.config.SessionID, newSessionID)
		c.config.SessionID = newSessionID
		// 持久化到数据库，避免下次重建 Client 时又读到旧 session_id
		if c.account != nil && c.account.ID > 0 {
			if err := c.store.UpdateSessionID(c.account.ID, newSessionID); err != nil {
				log.Printf("警告: 更新数据库 session_id 失败: %v", err)
			}
		}
	}

	return nil
}

func (c *Client) GetToken() (string, error) {
	// 每次请求前先用 client_cookie 刷新 session_id，防止 session 过期
	if err := c.refreshSessionID(); err != nil {
		log.Printf("警告: 刷新 session 失败: %v，尝试使用已有 session_id", err)
	}

	url := fmt.Sprintf("https://clerk.orchids.app/v1/client/sessions/%s/tokens?__clerk_api_version=2025-11-10&_clerk_js_version=5.117.0", c.config.SessionID)

	req, err := http.NewRequest("POST", url, strings.NewReader("organization_id="))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", c.config.GetCookies())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", err
	}

	return tokenResp.JWT, nil
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
