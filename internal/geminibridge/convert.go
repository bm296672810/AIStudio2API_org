package geminibridge

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// geminiStorageState 是推送给 Gemini 的 storage_state 负载形状，只保留桥接需要的字段
type geminiStorageState struct {
	Cookies  []geminiCookie    `json:"cookies"`
	Origins  []json.RawMessage `json:"origins"`
	Metadata geminiMetadata    `json:"geminiWeb2api"`
}

// geminiCookie 与 Gemini 的 Cookie 字段对齐，partitionKey 有意丢弃
type geminiCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
	SameSite string  `json:"sameSite,omitempty"`
}

// geminiMetadata 写入 Gemini 侧的 geminiWeb2api 扩展
type geminiMetadata struct {
	ID       string       `json:"id"`
	Proxy    string       `json:"proxy,omitempty"`
	Source   geminiSource `json:"source,omitempty"`
	Language string       `json:"language,omitempty"`
	AuthUser int          `json:"authUser,omitempty"`
}

// geminiSource 标注登录态属主，便于在 Gemini 管理端辨识
type geminiSource struct {
	Browser string `json:"browser,omitempty"`
	Profile string `json:"profile,omitempty"`
}

// accountIDPattern 匹配邮箱中除小写字母与数字外的字符，规整为 Gemini 账号 ID
var accountIDPattern = regexp.MustCompile(`[^a-z0-9]+`)

// geminiAccountID 复刻 Gemini chromeauth.AccountID 的规整规则：小写后非字母数字替换为 -，去掉首尾 -
func geminiAccountID(email string) string {
	id := accountIDPattern.ReplaceAllString(strings.ToLower(strings.TrimSpace(email)), "-")
	return strings.Trim(id, "-")
}

// coreCookieNames 是 Gemini 侧判定登录态有效的必备 Cookie
var coreCookieNames = map[string]bool{
	"SAPISID":        true,
	"__Secure-1PSID": true,
}

// hasCoreCookies 报告 Cookie 罐是否包含 Gemini 要求的核心登录 Cookie
func hasCoreCookies(cookies []aistudio.StateCookie) bool {
	seen := make(map[string]bool, len(cookies))
	for _, cookie := range cookies {
		seen[cookie.Name] = true
	}
	for name := range coreCookieNames {
		if !seen[name] {
			return false
		}
	}
	return true
}

// convertState 把一个 AIStudio 账户的登录态转换为 Gemini storage_state 负载
func convertState(email string, state aistudio.StorageState, cfg accountOptions) geminiStorageState {
	cookies := make([]geminiCookie, 0, len(state.Cookies))
	for _, cookie := range state.Cookies {
		cookies = append(cookies, geminiCookie{
			Name:     cookie.Name,
			Value:    cookie.Value,
			Domain:   cookie.Domain,
			Path:     cookie.Path,
			Expires:  cookie.Expires,
			HTTPOnly: cookie.HTTPOnly,
			Secure:   cookie.Secure,
			SameSite: cookie.SameSite,
		})
	}
	language := cfg.locale
	if language == "" {
		language = "en-US"
	}
	return geminiStorageState{
		Cookies: cookies,
		Origins: []json.RawMessage{},
		Metadata: geminiMetadata{
			ID:       geminiAccountID(email),
			Proxy:    cfg.proxy,
			Language: language,
			AuthUser: cfg.authUser,
			Source: geminiSource{
				Browser: "aistudio2api",
				Profile: email,
			},
		},
	}
}

// cookieFingerprint 取核心字段计算 Cookie 罐指纹，未变化的状态跳过推送
func cookieFingerprint(cookies []aistudio.StateCookie) string {
	var builder strings.Builder
	for _, cookie := range cookies {
		builder.WriteString(cookie.Name)
		builder.WriteByte('=')
		builder.WriteString(cookie.Value)
		builder.WriteByte('@')
		builder.WriteString(cookie.Domain)
		builder.WriteByte('/')
		builder.WriteString(cookie.Path)
		builder.WriteByte(';')
	}
	return builder.String()
}
