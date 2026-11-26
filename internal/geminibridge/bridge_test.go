package geminibridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func TestGeminiAccountID(t *testing.T) {
	cases := map[string]string{
		"Name@Example.com":       "name-example-com",
		"a.b+c@gmail.com":        "a-b-c-gmail-com",
		"test123@sub.domain.org": "test123-sub-domain-org",
		"--edge--@x.com":         "edge-x-com",
	}
	for email, want := range cases {
		if got := geminiAccountID(email); got != want {
			t.Errorf("geminiAccountID(%q) = %q，期望 %q", email, got, want)
		}
	}
}

func sampleState() aistudio.StorageState {
	return aistudio.StorageState{
		Cookies: []aistudio.StateCookie{
			{Name: "SAPISID", Value: "sapisid", Domain: ".google.com", Path: "/", Expires: 100, Secure: true, SameSite: "None", PartitionKey: "partition"},
			{Name: "__Secure-1PSID", Value: "psid", Domain: ".google.com", Path: "/", Expires: 200, HTTPOnly: true, Secure: true, SameSite: "Lax"},
			{Name: "__Secure-1PSIDTS", Value: "psidts", Domain: ".google.com", Path: "/", Expires: 300, Secure: true},
		},
	}
}

func TestConvertState(t *testing.T) {
	email := "Name@Example.com"
	state := sampleState()
	payload := convertState(email, state, accountOptions{proxy: "http://proxy:8080", locale: "zh-CN", authUser: 2})

	if len(payload.Origins) != 0 {
		t.Fatalf("origins 应为空数组，实际 %v", payload.Origins)
	}
	if len(payload.Cookies) != len(state.Cookies) {
		t.Fatalf("Cookie 数量 = %d，期望 %d", len(payload.Cookies), len(state.Cookies))
	}
	for _, cookie := range payload.Cookies {
		// partitionKey 不出现在 Gemini Cookie 结构中
		if cookie.Name == "SAPISID" && cookie.Value != "sapisid" {
			t.Fatalf("Cookie 值未透传: %+v", cookie)
		}
	}
	if payload.Metadata.ID != "name-example-com" {
		t.Errorf("metadata.ID = %q，期望 name-example-com", payload.Metadata.ID)
	}
	if payload.Metadata.Proxy != "http://proxy:8080" || payload.Metadata.Language != "zh-CN" || payload.Metadata.AuthUser != 2 {
		t.Errorf("metadata 上下文字段错误: %+v", payload.Metadata)
	}
	if payload.Metadata.Source.Browser != "aistudio2api" || payload.Metadata.Source.Profile != email {
		t.Errorf("metadata.source 错误: %+v", payload.Metadata.Source)
	}
	// 序列化结果不含 partitionKey 字段
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "partitionKey") {
		t.Fatal("负载不应包含 partitionKey 字段")
	}
}

func TestConvertStateDefaultLanguage(t *testing.T) {
	payload := convertState("a@x.com", sampleState(), accountOptions{})
	if payload.Metadata.Language != "en-US" {
		t.Errorf("空 locale 应回落 en-US，实际 %q", payload.Metadata.Language)
	}
}

func TestHasCoreCookies(t *testing.T) {
	if !hasCoreCookies(sampleState().Cookies) {
		t.Fatal("包含 SAPISID 与 __Secure-1PSID 应判定为有效")
	}
	missing := []aistudio.StateCookie{{Name: "SAPISID", Value: "x", Domain: ".google.com", Path: "/"}}
	if hasCoreCookies(missing) {
		t.Fatal("缺少 __Secure-1PSID 应判定为无效")
	}
}

func TestCookieFingerprintDedup(t *testing.T) {
	state := sampleState()
	first := cookieFingerprint(state.Cookies)
	second := cookieFingerprint(state.Cookies)
	if first != second {
		t.Fatal("相同 Cookie 罐指纹应一致")
	}
	changed := state
	changed.Cookies[0].Value = "rotated"
	if cookieFingerprint(changed.Cookies) == first {
		t.Fatal("Cookie 值变化后指纹应不同")
	}
}

func TestClientBootstrapAndRenew(t *testing.T) {
	var gotToken string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pairing/accounts" {
			http.NotFound(w, r)
			return
		}
		gotToken = r.Header.Get("Authorization")
		var body pairingRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("请求体解码失败: %v", err)
		}
		if len(body.StorageState.Cookies) == 0 {
			t.Error("请求体应携带 Cookie")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(pairingResult{RenewalToken: "renewal-xyz"})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	token, err := client.Bootstrap(context.Background(), "pairing-abc", convertState("a@x.com", sampleState(), accountOptions{}))
	if err != nil {
		t.Fatalf("Bootstrap 失败: %v", err)
	}
	if token != "renewal-xyz" {
		t.Errorf("续签凭据 = %q，期望 renewal-xyz", token)
	}
	if gotToken != "Bearer pairing-abc" {
		t.Errorf("引导授权头 = %q，期望 Bearer pairing-abc", gotToken)
	}
	if err := client.Renew(context.Background(), token, convertState("a@x.com", sampleState(), accountOptions{})); err != nil {
		t.Fatalf("Renew 失败: %v", err)
	}
}

func TestClientErrorClassification(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantTarget error
	}{
		{"令牌失效", http.StatusUnauthorized, errPairingTokenInvalid},
		{"状态被拒", http.StatusBadRequest, errStateRejected},
		{"服务端错误", http.StatusBadGateway, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "x", "message": "detail"}})
			}))
			defer server.Close()
			err := NewClient(server.URL).Renew(context.Background(), "tok", convertState("a@x.com", sampleState(), accountOptions{}))
			if tc.wantTarget != nil {
				if !errors.Is(err, tc.wantTarget) {
					t.Fatalf("错误 = %v，期望包装 %v", err, tc.wantTarget)
				}
			} else if err == nil || errors.Is(err, errPairingTokenInvalid) || errors.Is(err, errStateRejected) {
				t.Fatalf("5xx 应返回普通错误，实际 %v", err)
			}
		})
	}
}
