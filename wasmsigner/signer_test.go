package wasmsigner

import "testing"

func TestSignInferRequestSmoke(t *testing.T) {
	s, err := New("test-machine-id")
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer s.Close()

	u := UserInfo{UID: "test-uid", OrganizationID: "test-org", DataPolicyAgreed: true}

	sr, err := s.SignInferRequest(u, "https://gateway.qoder.com.cn", `{"hello":"world"}`, nil, nil)
	if err != nil {
		t.Fatalf("SignInferRequest() failed: %v", err)
	}
	t.Logf("url=%s", sr.URL)
	if sr.Headers["Authorization"] == "" {
		t.Fatal("missing Authorization header")
	}
	if sr.Headers["Cosy-MachineId"] != "test-machine-id" {
		t.Fatalf("unexpected Cosy-MachineId: %s", sr.Headers["Cosy-MachineId"])
	}

	// second call with same user info must reuse the cached context (no rebuild)
	ctxBefore := s.ctx
	if _, err := s.SignInferRequest(u, "https://gateway.qoder.com.cn", `{"hello":"again"}`, nil, nil); err != nil {
		t.Fatalf("second SignInferRequest() failed: %v", err)
	}
	if s.ctx != ctxBefore {
		t.Fatal("context was rebuilt for identical user info")
	}
}
