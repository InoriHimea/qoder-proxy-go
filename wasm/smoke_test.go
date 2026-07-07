package wasm

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
)

func testWasmPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	return filepath.Join(filepath.Dir(thisFile), "qoder_auth_wasm_bg.wasm")
}

func TestNewRuntimeSmoke(t *testing.T) {
	ctx := context.Background()
	rt, err := New(ctx, testWasmPath(t))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer rt.Close()

	userInfo := `{"uid":"test-uid","organization_id":"test-org","organization_tags":[],"data_policy_agreed":true}`
	extra := `{"client_type":"6","business_product":"qoder_work","business_type":"agent","scene":"assistant"}`

	runtimeFields, err := GenerateRuntimeAuthFields(rt, userInfo)
	if err != nil {
		t.Fatalf("GenerateRuntimeAuthFields() failed: %v", err)
	}
	t.Logf("runtimeFields=%s", runtimeFields)

	var rf struct {
		EncryptUserInfo string `json:"encrypt_user_info"`
		Key             string `json:"key"`
	}
	if err := json.Unmarshal([]byte(runtimeFields), &rf); err != nil {
		t.Fatalf("unmarshal runtimeFields: %v", err)
	}

	fullUserInfo := fmt.Sprintf(`{"uid":"test-uid","encrypt_user_info":%q,"key":%q,"organization_id":"test-org","organization_tags":[],"data_policy_agreed":true}`,
		rf.EncryptUserInfo, rf.Key)

	qc, err := NewContext(rt, "test-machine-id", "1.0.0", fullUserInfo, &extra)
	if err != nil {
		t.Fatalf("NewContext() failed: %v", err)
	}
	defer qc.Free()

	rr, err := qc.PrepareInferRequest("https://gateway.qoder.com.cn", `{"hello":"world"}`, nil, nil)
	if err != nil {
		t.Fatalf("PrepareInferRequest() failed: %v", err)
	}
	defer rr.Free()

	url, err := rr.URL()
	if err != nil {
		t.Fatalf("URL() failed: %v", err)
	}
	headers, err := rr.Headers()
	if err != nil {
		t.Fatalf("Headers() failed: %v", err)
	}

	t.Logf("url=%s", url)
	for k, v := range headers {
		t.Logf("header %s=%s", k, v)
	}
}
