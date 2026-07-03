package main

import (
	"context"
 	"crypto/rand"
 	"crypto/sha256"
  "encoding/base64"
 	"encoding/json"
 	"fmt"
 	"net/http"
 	"net/url"
 	"strings"
 	"sync"
 	"time"

 	"github.com/google/uuid"
 	"github.com/valyala/fasthttp"
)

type OAuthClient struct {
 	ClientID        string
 	ClientSecret    string
 	AuthHost        string
 	OpenAPIHost     string
 	ModelServerHost string
 	UserAgent       string
}

type OAuthResult struct {
  Token        string `json:"token"`
  RefreshToken string `json:"refresh_token"`
  UserID       string `json:"user_id"`
  ExpireTime   int64  `json:"expire_time"`
  Error        string `json:"error,omitempty"`
}

type OAuthSession struct {
  CodeVerifier    string
  Nonce           string
  StartTime       time.Time
  Cancel          context.CancelFunc
  ResultChan      chan OAuthResult
  UserID          string
  ExpireTime      int64
  RefreshToken    string
}

type OAuthStore struct {
  mu       sync.RWMutex
  sessions map[string]*OAuthSession
  // Cache by client_id to avoid concurrent sessions for same user
}

var oauthStore = &OAuthStore{sessions: make(map[string]*OAuthSession)}

func (s *OAuthStore) Get(id string) (*OAuthSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.sessions[id]
	return session, ok
}

func (s *OAuthStore) Set(id string, session *OAuthSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = session
}

func (s *OAuthStore) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

func generateOAuthPKCE() (verifier, challenge string, err error) {
  b := make([]byte, 32)
  if _, err = rand.Read(b); err != nil {
    return "", "", err
  }
  verifier = base64.RawURLEncoding.EncodeToString(b)
  h := sha256.Sum256([]byte(verifier))
  challenge = base64.RawURLEncoding.EncodeToString(h[:])
  return verifier, challenge, nil
}

func (c *OAuthClient) GenerateChallenge() (string, string, error) {
  return generateOAuthPKCE()
}

func (c *OAuthClient) AuthorizationURL(verifier, challenge, nonce string) string {
  baseURL := "https://" + c.AuthHost + "/device/selectAccounts"
  params := url.Values{
    "challenge":       {challenge},
    "challenge_method": {"S256"},
    "nonce":           {nonce},
    "client_id":       {c.ClientID},
  }
  return baseURL + "?" + params.Encode()
}

func (c *OAuthClient) PollDeviceToken(ctx context.Context, verifier, nonce, clientID string) (*OAuthResult, error) {
  client := &http.Client{Timeout: 30 * time.Second}
  openAPIHost := c.OpenAPIHost
  if strings.Contains(openAPIHost, "qoder.com.cn") {
    openAPIHost = "openapi.qoder.com.cn"
  }
  pollURL := "https://" + openAPIHost + "/api/v1/deviceToken/poll" +
    "?nonce=" + nonce +
    "&verifier=" + url.QueryEscape(verifier) +
    "&challenge_method=S256"

  ticker := time.NewTicker(5 * time.Second)
  defer ticker.Stop()

  for {
    select {
    case <-ctx.Done():
      return nil, fmt.Errorf("timeout waiting for authorization")
    case <-ticker.C:
      req, err := http.NewRequest("GET", pollURL, nil)
      if err != nil {
        continue
      }
      req.Header.Set("User-Agent", c.UserAgent)
      req.Header.Set("Content-Type", "application/json")
      req.Header.Set("Accept", "application/json")

      resp, err := client.Do(req)
      if err != nil {
        continue
      }

      var body struct {
        Token        string `json:"token"`
        RefreshToken string `json:"refresh_token"`
        Error        string `json:"error"`
      }

      if err := json.NewDecoder(resp.Body).Decode(&body); err == nil {
        if resp.StatusCode == 200 && body.Token != "" {
          resp.Body.Close()
          return &OAuthResult{
            Token:        body.Token,
            RefreshToken: body.RefreshToken,
          }, nil
        }
        resp.Body.Close()
        // 404 = user hasn't approved yet, continue polling
        if resp.StatusCode == 404 {
          continue
        }
      }
      resp.Body.Close()
    }
  }
}

func (c *OAuthClient) RefreshToken(ctx context.Context, refreshToken string) (*OAuthResult, error) {
  client := &http.Client{Timeout: 10 * time.Second}
  openAPIHost := c.OpenAPIHost
  if strings.Contains(openAPIHost, "qoder.com.cn") {
    openAPIHost = "openapi.qoder.com.cn"
  }
  refreshURL := "https://" + openAPIHost + "/api/v1/deviceToken/refresh"

  payload := map[string]string{"refresh_token": refreshToken}
  bodyData, _ := json.Marshal(payload)

  req, err := http.NewRequest("POST", refreshURL, strings.NewReader(string(bodyData)))
  if err != nil {
    return nil, err
  }
  req.Header.Set("User-Agent", c.UserAgent)
  req.Header.Set("Content-Type", "application/json")
  req.Header.Set("Accept", "application/json")

  resp, err := client.Do(req)
  if err != nil {
    return nil, err
  }
  defer resp.Body.Close()

  var result struct {
    DeviceToken   string `json:"device_token"`
    RefreshToken  string `json:"refresh_token"`
    ExpiresAt     string `json:"expires_at"`
    Error         string `json:"error"`
  }

  if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
    return nil, err
  }

  if result.DeviceToken == "" {
    return nil, fmt.Errorf("empty device_token in refresh response")
  }

  return &OAuthResult{
    Token:        result.DeviceToken,
    RefreshToken: result.RefreshToken,
  }, nil
}

func (c *OAuthClient) GetUserInfo(ctx context.Context, token string) (*OAuthResult, error) {
  client := &http.Client{Timeout: 10 * time.Second}
  openAPIHost := c.OpenAPIHost
  if strings.Contains(openAPIHost, "qoder.com.cn") {
    openAPIHost = "openapi.qoder.com.cn"
  }
  userInfoURL := "https://" + openAPIHost + "/api/v1/userinfo"

  req, err := http.NewRequest("GET", userInfoURL, nil)
  if err != nil {
    return nil, err
  }
  req.Header.Set("User-Agent", c.UserAgent)
  req.Header.Set("Authorization", "Bearer " + token)
  req.Header.Set("Accept", "application/json")

  resp, err := client.Do(req)
  if err != nil {
    return nil, err
  }
  defer resp.Body.Close()

  var result struct {
    ID         string `json:"id"`
    Name       string `json:"name"`
    Email      string `json:"email"`
    ExpireTime int64  `json:"expire_time"`
  }

  if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
    return nil, err
  }

  return &OAuthResult{
    UserID:     result.ID,
    ExpireTime: result.ExpireTime,
  }, nil
}

func (c *OAuthClient) CreateLoginSession() (string, string, error) {
  sessionID := uuid.New().String()
  codeVerifier, challenge, err := c.GenerateChallenge()
  if err != nil {
    return "", "", err
  }

  nonce := uuid.New().String()
  authURL := c.AuthorizationURL(codeVerifier, challenge, nonce)

  ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
  session := &OAuthSession{
    CodeVerifier: codeVerifier,
    Nonce:        nonce,
    StartTime:    time.Now(),
    Cancel:       cancel,
    ResultChan:   make(chan OAuthResult, 1),
  }

  oauthStore.Set(sessionID, session)

  // Start polling in background
  go func() {
    result, err := c.PollDeviceToken(ctx, codeVerifier, nonce, c.ClientID)
    if err != nil {
      session.ResultChan <- OAuthResult{Error: err.Error()}
      return
    }
    if result != nil {
      // Get user info
      if userInfo, err := c.GetUserInfo(ctx, result.Token); err == nil {
        result.UserID = userInfo.UserID
        result.ExpireTime = userInfo.ExpireTime
      }
      session.ResultChan <- *result
    }
  }()

  return sessionID, authURL, nil
}

func (c *OAuthClient) GetSessionResult(sessionID string, timeout int) (*OAuthResult, error) {
  session, ok := oauthStore.Get(sessionID)
  if !ok {
    return nil, fmt.Errorf("session %s not found", sessionID)
  }

  select {
  case result := <-session.ResultChan:
    oauthStore.Delete(sessionID)
    return &result, nil
  case <-time.After(time.Duration(timeout) * time.Second):
    session.Cancel()
    oauthStore.Delete(sessionID)
    return nil, fmt.Errorf("timeout waiting for OAuth result")
  }
}

func (c *OAuthClient) CleanupExpiredSessions() {
  now := time.Now()
  for id, session := range oauthStore.sessions {
    if now.Sub(session.StartTime) > 10*time.Minute {
      session.Cancel()
      oauthStore.Delete(id)
    }
  }
}

func NewOAuthClient(backend string) *OAuthClient {
  if strings.ToLower(backend) == "cn" {
    return &OAuthClient{
      ClientID:        "e93fe488-5778-4c35-a6fc-0f54ed7b3139",
      AuthHost:        "qoder.com.cn",
      OpenAPIHost:     "openapi.qoder.com.cn",
      ModelServerHost: "api2-v2.qoder.com.cn",
      UserAgent:       "qoder/1.0.22",
    }
  }
  return &OAuthClient{
    ClientID:        "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb",
    AuthHost:        "qoder.sh",
    OpenAPIHost:     "openapi.qoder.sh",
    ModelServerHost: "api2-v2.qoder.sh",
    UserAgent:       "qoder/1.0.22",
  }
}

func (c *OAuthClient) HandleLogin(ctx *fasthttp.RequestCtx) {
  sessionID, authURL, err := c.CreateLoginSession()
  if err != nil {
    ctx.SetStatusCode(500)
    json.NewEncoder(ctx).Encode(map[string]interface{}{
      "error": fmt.Sprintf("failed to create OAuth session: %v", err),
    })
    return
  }

  json.NewEncoder(ctx).Encode(map[string]interface{}{
    "session_id": sessionID,
    "auth_url":   authURL,
    "message":    "Open the auth URL in your browser to login. Then poll /login/<session_id> for the result.",
  })
}

func (c *OAuthClient) HandleLoginResult(ctx *fasthttp.RequestCtx, sessionID string) {
  result, err := c.GetSessionResult(sessionID, 5*60)
  if err != nil {
    ctx.SetStatusCode(500)
    json.NewEncoder(ctx).Encode(map[string]interface{}{
      "error": err.Error(),
    })
    return
  }

  json.NewEncoder(ctx).Encode(result)
}