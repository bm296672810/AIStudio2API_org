package geminibridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// errPairingTokenInvalid 表示配对或续签令牌被对端拒绝，需要重新引导
var errPairingTokenInvalid = errors.New("配对令牌无效或已过期")

// errStateRejected 表示推送的 storage_state 未通过对端校验
var errStateRejected = errors.New("对端拒绝导入账户状态")

// pairingRequest 是配对上传的请求体
type pairingRequest struct {
	StorageState geminiStorageState `json:"storage_state"`
}

// pairingResult 是首次上传成功的响应，续签凭据用于后续续推
type pairingResult struct {
	RenewalToken string `json:"renewal_token"`
}

// pairingError 是对端返回的错误体
type pairingError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Client 调用 Gemini 配对接口完成账户状态的引导与续推
type Client struct {
	origin string
	http   *http.Client
}

// NewClient 创建指向 Gemini origin 的配对客户端
func NewClient(origin string) *Client {
	return &Client{
		origin: origin,
		http:   &http.Client{Timeout: 2 * time.Minute},
	}
}

// Bootstrap 用一次性配对令牌完成首传，返回对端签发的续签凭据
func (c *Client) Bootstrap(ctx context.Context, token string, state geminiStorageState) (string, error) {
	result, err := c.push(ctx, token, state)
	if err != nil {
		return "", err
	}
	if result.RenewalToken == "" {
		return "", errors.New("对端未返回续签凭据")
	}
	return result.RenewalToken, nil
}

// Renew 用续签凭据续推新的账户状态
func (c *Client) Renew(ctx context.Context, renewalToken string, state geminiStorageState) error {
	_, err := c.push(ctx, renewalToken, state)
	return err
}

// push 发送一次配对上传，按 HTTP 状态码分类错误
func (c *Client) push(ctx context.Context, token string, state geminiStorageState) (pairingResult, error) {
	body, err := json.Marshal(pairingRequest{StorageState: state})
	if err != nil {
		return pairingResult{}, fmt.Errorf("编码账户状态: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.origin+"/api/pairing/accounts", bytes.NewReader(body))
	if err != nil {
		return pairingResult{}, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")

	response, err := c.http.Do(request)
	if err != nil {
		return pairingResult{}, fmt.Errorf("连接 Gemini 实例: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusCreated {
		var result pairingResult
		if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
			return pairingResult{}, fmt.Errorf("解析配对响应: %w", err)
		}
		return result, nil
	}
	if response.StatusCode == http.StatusOK {
		return pairingResult{}, nil
	}

	var failure pairingError
	_ = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&failure)
	switch response.StatusCode {
	case http.StatusUnauthorized:
		return pairingResult{}, fmt.Errorf("%w: %s", errPairingTokenInvalid, failure.Error.Message)
	case http.StatusBadRequest:
		return pairingResult{}, fmt.Errorf("%w: %s", errStateRejected, failure.Error.Message)
	default:
		return pairingResult{}, fmt.Errorf("Gemini 返回 HTTP %d: %s", response.StatusCode, failure.Error.Message)
	}
}
