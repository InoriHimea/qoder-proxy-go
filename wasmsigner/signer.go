// Package wasmsigner provides a high-level, Config-agnostic API for signing
// Qoder COSY requests via the embedded WASM auth module (wasm.Runtime).
//
// It mirrors the real client's auth-manager pattern: one long-lived
// QoderContext is built from a user identity (uid/org/tags/data-policy) and
// reused across many signed requests, only being rebuilt when that identity
// actually changes (see regenerateRuntimeFields()+createWasmContext() in the
// original JS bundle).
package wasmsigner

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/InoriHimea/qoder-proxy-go/wasm"
)

const (
	defaultCosyVersion = "1.0.0"
	// defaultExtraConfig mirrors the JS client's CLI defaults (Ol()):
	// client_type=6, business_product=qoder_work, business_type=agent,
	// scene=assistant.
	defaultExtraConfig = `{"client_type":"6","business_product":"qoder_work","business_type":"agent","scene":"assistant"}`
)

// UserInfo identifies the account a request is signed on behalf of.
type UserInfo struct {
	UID              string
	OrganizationID   string
	OrganizationTags []string
	DataPolicyAgreed bool
}

func (u UserInfo) cacheKey() string {
	tags, _ := json.Marshal(u.OrganizationTags)
	return fmt.Sprintf("%s|%s|%s|%v", u.UID, u.OrganizationID, tags, u.DataPolicyAgreed)
}

func (u UserInfo) baseJSON() (string, error) {
	payload := struct {
		UID              string   `json:"uid"`
		OrganizationID   string   `json:"organization_id"`
		OrganizationTags []string `json:"organization_tags"`
		DataPolicyAgreed bool     `json:"data_policy_agreed"`
	}{u.UID, u.OrganizationID, tagsOrEmpty(u.OrganizationTags), u.DataPolicyAgreed}
	b, err := json.Marshal(payload)
	return string(b), err
}

func tagsOrEmpty(tags []string) []string {
	if tags == nil {
		return []string{}
	}
	return tags
}

// SignedRequest is a fully-signed (url, headers, body) triple ready to send.
type SignedRequest struct {
	URL     string
	Headers map[string]string
	Body    string
}

// Signer holds one long-lived WASM runtime and QoderContext, recreating the
// context only when the signed-for user identity changes.
type Signer struct {
	mu          sync.Mutex
	rt          *wasm.Runtime
	ctx         *wasm.Context
	machineID   string
	cosyVersion string
	extraConfig string
	lastKey     string
}

// New loads the embedded WASM auth module and returns a Signer that signs
// requests on behalf of machineID (a persisted per-install device UUID).
func New(machineID string) (*Signer, error) {
	rt, err := wasm.NewEmbedded(context.Background())
	if err != nil {
		return nil, fmt.Errorf("wasmsigner: load runtime: %w", err)
	}
	return &Signer{
		rt:          rt,
		machineID:   machineID,
		cosyVersion: defaultCosyVersion,
		extraConfig: defaultExtraConfig,
	}, nil
}

// Invalidate forces the next signing call to rebuild the QoderContext even
// if the user identity is unchanged. Use after a 401/403 in case the
// server-side signed identity (encrypt_user_info) has expired.
func (s *Signer) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastKey = ""
}

// Close releases the underlying WASM context and runtime.
func (s *Signer) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx != nil {
		s.ctx.Free()
		s.ctx = nil
	}
	if s.rt != nil {
		s.rt.Close()
		s.rt = nil
	}
}

// ensureContext builds (or rebuilds) the QoderContext for u if it isn't
// already current. Must be called with s.mu held.
func (s *Signer) ensureContext(u UserInfo) error {
	key := u.cacheKey()
	if s.ctx != nil && s.lastKey == key {
		return nil
	}

	baseJSON, err := u.baseJSON()
	if err != nil {
		return fmt.Errorf("wasmsigner: marshal user info: %w", err)
	}

	runtimeFields, err := wasm.GenerateRuntimeAuthFields(s.rt, baseJSON)
	if err != nil {
		return fmt.Errorf("wasmsigner: generate runtime auth fields: %w", err)
	}
	var rf struct {
		EncryptUserInfo string `json:"encrypt_user_info"`
		Key             string `json:"key"`
	}
	if err := json.Unmarshal([]byte(runtimeFields), &rf); err != nil {
		return fmt.Errorf("wasmsigner: unmarshal runtime auth fields: %w", err)
	}

	fullUserInfo := struct {
		UID              string   `json:"uid"`
		EncryptUserInfo  string   `json:"encrypt_user_info"`
		Key              string   `json:"key"`
		OrganizationID   string   `json:"organization_id"`
		OrganizationTags []string `json:"organization_tags"`
		DataPolicyAgreed bool     `json:"data_policy_agreed"`
	}{u.UID, rf.EncryptUserInfo, rf.Key, u.OrganizationID, tagsOrEmpty(u.OrganizationTags), u.DataPolicyAgreed}
	fullJSON, err := json.Marshal(fullUserInfo)
	if err != nil {
		return fmt.Errorf("wasmsigner: marshal full user info: %w", err)
	}

	newCtx, err := wasm.NewContext(s.rt, s.machineID, s.cosyVersion, string(fullJSON), &s.extraConfig)
	if err != nil {
		return fmt.Errorf("wasmsigner: create context: %w", err)
	}

	if s.ctx != nil {
		s.ctx.Free()
	}
	s.ctx = newCtx
	s.lastKey = key
	return nil
}

func readResult(rr *wasm.RequestResult) (*SignedRequest, error) {
	defer rr.Free()

	url, err := rr.URL()
	if err != nil {
		return nil, fmt.Errorf("wasmsigner: read url: %w", err)
	}
	headers, err := rr.Headers()
	if err != nil {
		return nil, fmt.Errorf("wasmsigner: read headers: %w", err)
	}
	body, err := rr.Body()
	if err != nil {
		return nil, fmt.Errorf("wasmsigner: read body: %w", err)
	}
	return &SignedRequest{URL: url, Headers: headers, Body: body}, nil
}

// SignInferRequest signs an agent_chat_generation-style inference request.
// endpoint must be a bare host URL (e.g. "https://gateway.qoder.com.cn") —
// the WASM module appends the inference path itself. modelKey and source may
// be nil.
func (s *Signer) SignInferRequest(u UserInfo, endpoint, bodyJSON string, modelKey, source *string) (*SignedRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ensureContext(u); err != nil {
		return nil, err
	}

	rr, err := s.ctx.PrepareInferRequest(endpoint, bodyJSON, modelKey, source)
	if err != nil {
		return nil, fmt.Errorf("wasmsigner: prepare infer request: %w", err)
	}
	return readResult(rr)
}

// SignRequest signs a generic (non-inference) request. body and extra may be
// nil.
func (s *Signer) SignRequest(u UserInfo, endpoint, path, method, sigPath string, body, extra *string) (*SignedRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ensureContext(u); err != nil {
		return nil, err
	}

	rr, err := s.ctx.PrepareRequest(endpoint, path, method, sigPath, body, extra)
	if err != nil {
		return nil, fmt.Errorf("wasmsigner: prepare request: %w", err)
	}
	return readResult(rr)
}
