package geminibridge

import (
	"context"
	"errors"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 默认同步与退避参数
const (
	defaultInterval = 10 * time.Minute
	backoffStart    = 30 * time.Second
	backoffMax      = 10 * time.Minute
)

// Options 是桥接器的运行参数
type Options struct {
	Origin       string
	Interval     time.Duration
	PairingToken string
	Include      []string
	AuthStates   []string
	GlobalProxy  string
	StorePath    string
}

// LogFunc 与管理服务的日志函数签名一致
type LogFunc func(level, message string, attrs ...any)

// Bridge 周期性把 AIStudio 账户登录态推送到 Gemini 实例
type Bridge struct {
	opts   Options
	client *Client
	store  *Store
	log    LogFunc

	// pairingToken 是一次性引导令牌，只在内存持有，不落盘
	pairingToken string
	// nextDue 记录每个账户下次允许推送的时刻
	nextDue map[string]time.Time
	// backoff 记录每个账户当前的退避时长
	backoff map[string]time.Duration
	// fingerprints 记录上次成功推送的 Cookie 指纹，用于去重
	fingerprints map[string]string
}

// New 创建桥接器并打开令牌存储
func New(opts Options, log LogFunc) (*Bridge, error) {
	if opts.Interval <= 0 {
		opts.Interval = defaultInterval
	}
	store, err := LoadStore(opts.StorePath, opts.Origin)
	if err != nil {
		return nil, err
	}
	return &Bridge{
		opts:         opts,
		client:       NewClient(opts.Origin),
		store:        store,
		log:          log,
		pairingToken: opts.PairingToken,
		nextDue:      make(map[string]time.Time),
		backoff:      make(map[string]time.Duration),
		fingerprints: make(map[string]string),
	}, nil
}

// Run 启动定时同步循环，直到 ctx 结束
func (b *Bridge) Run(ctx context.Context) {
	b.log("INFO", "Gemini Cookie 桥接已启动", "origin", b.opts.Origin, "interval", b.opts.Interval.String())
	// 唤醒粒度为同步间隔的一半，使设备绑定账户能按减半间隔推送
	ticker := time.NewTicker(b.opts.Interval / 2)
	defer ticker.Stop()
	b.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			b.log("INFO", "Gemini Cookie 桥接已停止")
			return
		case <-ticker.C:
			b.tick(ctx)
		}
	}
}

// tick 扫描全部合格账户并推送到期的账户
func (b *Bridge) tick(ctx context.Context) {
	accounts, err := b.loadAccounts()
	if err != nil {
		b.log("WARN", "桥接扫描账户目录失败", "error", err)
		return
	}
	now := time.Now()
	for _, account := range accounts {
		if ctx.Err() != nil {
			return
		}
		if due, ok := b.nextDue[account.ID]; ok && now.Before(due) {
			continue
		}
		b.syncAccount(ctx, account, now)
	}
}

// loadAccounts 复用账户存储的目录展开规则，返回启用且在包含名单内的账户
func (b *Bridge) loadAccounts() ([]*aistudio.Account, error) {
	store := aistudio.NewAccountStore(b.opts.AuthStates...)
	accounts, err := store.Load()
	if err != nil {
		return nil, err
	}
	for _, failure := range store.Failures() {
		b.log("WARN", "桥接跳过无法载入的账户目录", "directory", failure.Directory, "error", failure.Err)
	}
	include := make(map[string]struct{}, len(b.opts.Include))
	for _, email := range b.opts.Include {
		include[email] = struct{}{}
	}
	filtered := make([]*aistudio.Account, 0, len(accounts))
	for _, account := range accounts {
		if !account.Config.Enabled {
			continue
		}
		if len(include) > 0 {
			if _, ok := include[account.ID]; !ok {
				continue
			}
		}
		filtered = append(filtered, account)
	}
	return filtered, nil
}

// syncAccount 转换并推送单个账户，按结果调度下一次同步
func (b *Bridge) syncAccount(ctx context.Context, account *aistudio.Account, now time.Time) {
	email := account.ID
	state, err := aistudio.LoadStorageState(account.StoragePath)
	if err != nil {
		b.recordFailure(email, now, "读取登录态失败: "+err.Error())
		return
	}
	if !hasCoreCookies(state.Cookies) {
		b.log("WARN", "桥接跳过缺少核心 Cookie 的账户", "account", email)
		b.scheduleNext(email, now, false)
		return
	}
	fingerprint := cookieFingerprint(state.Cookies)
	if b.fingerprints[email] == fingerprint {
		b.scheduleNext(email, now, state.DeviceBoundSession())
		return
	}

	proxy := account.Config.Proxy
	if proxy == "" {
		proxy = b.opts.GlobalProxy
	}
	payload := convertState(email, state, accountOptions{
		proxy: proxy, locale: account.Config.Locale, authUser: account.Config.AuthUser,
	})
	record := b.store.Record(email)
	deviceBound := state.DeviceBoundSession()

	if record.RenewalToken != "" {
		err = b.client.Renew(ctx, record.RenewalToken, payload)
	} else if b.pairingToken != "" {
		var renewalToken string
		renewalToken, err = b.client.Bootstrap(ctx, b.pairingToken, payload)
		if err == nil {
			record.RenewalToken = renewalToken
		}
	} else {
		b.log("WARN", "桥接账户等待配对令牌", "account", email)
		_ = b.store.UpdateRecord(email, func(r accountRecord) accountRecord {
			r.LastStatus = statusPairingRequired
			r.GeminiID = payload.Metadata.ID
			return r
		})
		b.scheduleNext(email, now, deviceBound)
		return
	}

	if err != nil {
		b.handlePushError(email, now, deviceBound, fingerprint, err)
		return
	}
	b.fingerprints[email] = fingerprint
	b.backoff[email] = 0
	_ = b.store.UpdateRecord(email, func(r accountRecord) accountRecord {
		r.RenewalToken = record.RenewalToken
		r.GeminiID = payload.Metadata.ID
		r.DeviceBound = deviceBound
		r.LastPushAt = now
		r.LastStatus = statusOK
		r.LastError = ""
		return r
	})
	b.log("INFO", "桥接账户已推送", "account", email, "gemini_id", payload.Metadata.ID, "device_bound", deviceBound)
	b.scheduleNext(email, now, deviceBound)
}

// handlePushError 按错误类型清理令牌或安排退避
func (b *Bridge) handlePushError(email string, now time.Time, deviceBound bool, fingerprint string, err error) {
	if errors.Is(err, errPairingTokenInvalid) {
		if clearErr := b.store.ClearToken(email); clearErr != nil {
			b.log("WARN", "桥接清除失效令牌失败", "account", email, "error", clearErr)
		}
		b.log("WARN", "桥接令牌失效，等待重新配对", "account", email, "error", err)
		b.scheduleNext(email, now, deviceBound)
		return
	}
	if errors.Is(err, errStateRejected) {
		b.log("WARN", "桥接账户状态被对端拒绝，登录态变化前跳过", "account", email, "error", err)
		// 记录指纹后相同状态会在去重环节跳过，直到 Cookie 变化才再次尝试
		b.fingerprints[email] = fingerprint
		b.scheduleNext(email, now, deviceBound)
		_ = b.store.UpdateRecord(email, func(r accountRecord) accountRecord {
			r.LastStatus = statusRetry
			r.LastError = err.Error()
			return r
		})
		return
	}
	b.recordFailure(email, now, err.Error())
}

// recordFailure 记录退避并更新存储状态
func (b *Bridge) recordFailure(email string, now time.Time, message string) {
	backoff := b.backoff[email]
	if backoff == 0 {
		backoff = backoffStart
	} else {
		backoff *= 2
	}
	if backoff > backoffMax {
		backoff = backoffMax
	}
	b.backoff[email] = backoff
	b.nextDue[email] = now.Add(backoff)
	_ = b.store.UpdateRecord(email, func(r accountRecord) accountRecord {
		r.LastStatus = statusRetry
		r.LastError = message
		return r
	})
	b.log("WARN", "桥接推送失败，安排退避重试", "account", email, "backoff", backoff.String(), "error", message)
}

// scheduleNext 按账户是否设备绑定设置下一次同步时刻
func (b *Bridge) scheduleNext(email string, now time.Time, deviceBound bool) {
	interval := b.opts.Interval
	if deviceBound {
		interval = b.opts.Interval / 2
	}
	b.nextDue[email] = now.Add(interval)
}

// accountOptions 是转换单个账户时的上下文
type accountOptions struct {
	proxy    string
	locale   string
	authUser int
}
