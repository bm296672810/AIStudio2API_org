package app

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/config"
	"github.com/Mag1cFall/AIStudio2API/internal/geminibridge"
)

// geminiBridgeStore 是续签凭据与推送状态的持久化文件
var geminiBridgeStore = filepath.Join("runtime", "gemini-bridge.json")

// startGeminiBridge 在启用时启动进程级 Cookie 桥接，把 AIStudio 登录态单向推送到 Gemini 实例
func startGeminiBridge(ctx context.Context, cfg config.Config, log func(string, string, ...any)) {
	if !cfg.GeminiBridgeEnabled {
		return
	}
	include := splitCSV(cfg.GeminiBridgeInclude)
	bridge, err := geminibridge.New(geminibridge.Options{
		Origin:       cfg.GeminiBridgeOrigin,
		Interval:     cfg.GeminiBridgeInterval,
		PairingToken: cfg.GeminiBridgePairingToken,
		Include:      include,
		AuthStates:   strings.Split(cfg.AuthStates, ","),
		GlobalProxy:  cfg.Proxy,
		StorePath:    geminiBridgeStore,
	}, log)
	if err != nil {
		log("WARN", "Gemini Cookie 桥接启动失败", "error", err)
		return
	}
	go bridge.Run(ctx)
}

// splitCSV 解析逗号分隔名单，去掉空白与空项
func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		if item := strings.TrimSpace(part); item != "" {
			items = append(items, item)
		}
	}
	return items
}
