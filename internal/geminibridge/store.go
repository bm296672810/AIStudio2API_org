package geminibridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// 账户推送状态
const (
	statusPairingRequired = "pairing_required"
	statusOK              = "ok"
	statusRetry           = "retry"
)

// accountRecord 是单个账户在 Gemini 侧的桥接状态
type accountRecord struct {
	GeminiID     string    `json:"gemini_id"`
	RenewalToken string    `json:"renewal_token,omitempty"`
	DeviceBound  bool      `json:"device_bound"`
	LastPushAt   time.Time `json:"last_push_at,omitempty"`
	LastStatus   string    `json:"last_status"`
	LastError    string    `json:"last_error,omitempty"`
}

// bridgeData 是令牌存储文件的完整内容
type bridgeData struct {
	Origin   string                   `json:"origin"`
	Accounts map[string]accountRecord `json:"accounts"`
}

// Store 持久化各账户的续签凭据与推送状态，并发安全
type Store struct {
	mu   sync.Mutex
	path string
	data bridgeData
}

// LoadStore 读取令牌存储，文件不存在时返回空存储
func LoadStore(path, origin string) (*Store, error) {
	store := &Store{
		path: path,
		data: bridgeData{Origin: origin, Accounts: make(map[string]accountRecord)},
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取桥接令牌存储: %w", err)
	}
	if err := json.Unmarshal(data, &store.data); err != nil {
		return nil, fmt.Errorf("解析桥接令牌存储: %w", err)
	}
	if store.data.Accounts == nil {
		store.data.Accounts = make(map[string]accountRecord)
	}
	store.data.Origin = origin
	return store, nil
}

// Record 返回账户的桥接状态记录，不存在时返回待配对的零值记录
func (s *Store) Record(email string) accountRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	if record, ok := s.data.Accounts[email]; ok {
		return record
	}
	return accountRecord{LastStatus: statusPairingRequired}
}

// UpdateRecord 在锁内修改账户记录并落盘
func (s *Store) UpdateRecord(email string, update func(accountRecord) accountRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.data.Accounts[email]
	s.data.Accounts[email] = update(record)
	return s.saveLocked()
}

// ClearToken 清除失效的续签凭据并回到待配对状态
func (s *Store) ClearToken(email string) error {
	return s.UpdateRecord(email, func(record accountRecord) accountRecord {
		record.RenewalToken = ""
		record.LastStatus = statusPairingRequired
		return record
	})
}

// saveLocked 以临时文件加 rename 的方式原子写盘，调用方需持有 mu
func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("创建桥接存储目录: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写入桥接令牌存储: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("保存桥接令牌存储: %w", err)
	}
	return nil
}
