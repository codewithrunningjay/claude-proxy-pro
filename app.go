package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App struct
type App struct {
	ctx       context.Context
	cfg       *ConfigStore
	proxy     *Proxy
	stats     *StatsManager
	pm        *ProviderManager
	stability *StabilityManager
}

// NewApp creates a new App application struct
func NewApp(cfg *ConfigStore, proxy *Proxy, stats *StatsManager, pm *ProviderManager, stability *StabilityManager) *App {
	return &App{
		cfg:       cfg,
		proxy:     proxy,
		stats:     stats,
		pm:        pm,
		stability: stability,
	}
}

// startup is called when the app starts.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	// Trigger model discovery after startup
	go func() {
		a.pm.DiscoverModels()
	}()
}

// ── Dashboard Methods ─────────────────────────────────────────────────────────

// GetStats returns current statistics.
func (a *App) GetStats() map[string]interface{} {
	return a.stats.GetStats()
}

// GetHealth returns health/stability status.
func (a *App) GetHealth() map[string]interface{} {
	s := a.stability.GetStatus()
	cfg := a.cfg.Get()
	return map[string]interface{}{
		"is_healthy":      s.IsHealthy,
		"uptime":          s.Uptime,
		"start_time":      s.StartTime.Format("2006-01-02T15:04:05Z07:00"),
		"total_requests":  s.TotalRequests,
		"total_errors":    s.TotalErrors,
		"total_retries":   s.TotalRetries,
		"total_failovers": s.TotalFailovers,
		"active_idx":      cfg.ActiveIdx,
		"auto_retry":      cfg.AutoRetry,
		"failover":        cfg.Failover,
		"retry_max":       cfg.RetryMax,
	}
}

// GetUptime returns the server uptime string.
func (a *App) GetUptime() string {
	return a.stability.Uptime().String()
}

// GetConfig returns the current application config.
func (a *App) GetConfig() AppConfig {
	return a.cfg.Get()
}

// ── Provider Methods ──────────────────────────────────────────────────────────

// GetProviders returns all configured providers.
func (a *App) GetProviders() []Provider {
	return a.cfg.Get().Providers
}

// AddProvider adds a new provider.
func (a *App) AddProvider(p Provider) error {
	p.Status = "unknown"
	p.LastCheck = ""
	return a.cfg.AddProvider(p)
}

// UpdateProvider updates a provider at the given index.
func (a *App) UpdateProvider(idx int, p Provider) error {
	return a.cfg.UpdateProvider(idx, p)
}

// RemoveProvider removes a provider at the given index.
func (a *App) RemoveProvider(idx int) error {
	return a.cfg.RemoveProvider(idx)
}

// TestProvider tests a saved provider by index.
func (a *App) TestProvider(idx int) map[string]interface{} {
	status, latency, count, errStr := a.pm.TestConnection(idx)
	return map[string]interface{}{
		"status":      status,
		"latency_ms":  latency,
		"model_count": count,
		"error":       errStr,
	}
}

// TestArbitraryProvider tests an unsaved provider directly from the modal.
func (a *App) TestArbitraryProvider(name, pUrl, key, model string) map[string]interface{} {
	p := Provider{Name: name, URL: pUrl, Key: key, Model: model}
	models, err := a.pm.fetchModels(p, -1)
	status := "online"
	errMsg := ""
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), "HTTP 403") {
			status = "offline"
			errMsg = "Invalid API Key or Unauthorized (Auth Failed)"
		} else if a.pm.pingProvider(p) {
			status = "online"
		} else {
			status = "offline"
			errMsg = err.Error()
		}
	}
	return map[string]interface{}{
		"status":      status,
		"latency_ms":  100, // mock latency for quick test
		"model_count": len(models),
		"error":       errMsg,
	}
}

// SwitchProvider switches the active provider.
func (a *App) SwitchProvider(idx int) error {
	err := a.cfg.SetActiveProvider(idx)
	if err == nil {
		if p, ok := a.cfg.GetProviderByIndex(idx); ok && p.Model != "" {
			updateClaudeSettings(p.Model)
		}
	}
	return err
}

// DiscoverModels triggers model discovery from all providers.
func (a *App) DiscoverModels() string {
	a.pm.DiscoverModels()
	return "discovery completed"
}

// ── Model Methods ─────────────────────────────────────────────────────────────

// GetModels returns all discovered models.
func (a *App) GetModels() []ModelInfo {
	return a.cfg.GetModels()
}

// ActivateModel switches to a specific model via its provider.
func (a *App) ActivateModel(modelID, providerName string) error {
	cfg := a.cfg.Get()
	for i, p := range cfg.Providers {
		if p.Name == providerName {
			p.Model = modelID
			if err := a.cfg.UpdateProvider(i, p); err != nil {
				return err
			}
			err := a.cfg.SetActiveProvider(i)
			if err == nil {
				// Dynamically sync to Claude Code's settings.json immediately
				updateClaudeSettings(modelID)
			}
			return err
		}
	}
	return fmt.Errorf("provider %s not found", providerName)
}

// RestoreNativeClaude removes all proxy settings and cleans the terminal integration
func (a *App) RestoreNativeClaude() string {
	clearClaudeSettings()
	RemoveTerminalIntegration()
	return "Successfully restored native Claude Code and removed terminal integration."
}

// ── Settings Methods ──────────────────────────────────────────────────────────

// UpdateSetting updates a single setting value.
func (a *App) UpdateSetting(key string, value interface{}) error {
	cfg := a.cfg.Get()
	switch key {
	case "auto_retry":
		if v, ok := value.(bool); ok {
			cfg.AutoRetry = v
		}
	case "failover":
		if v, ok := value.(bool); ok {
			cfg.Failover = v
		}
	case "retry_max":
		if v, ok := value.(float64); ok {
			cfg.RetryMax = int(v)
		} else if v, ok := value.(int); ok {
			cfg.RetryMax = v
		}
	case "check_interval_seconds":
		if v, ok := value.(float64); ok {
			cfg.CheckInterval = int(v)
		} else if v, ok := value.(int); ok {
			cfg.CheckInterval = v
		}
	default:
		return fmt.Errorf("unknown setting: %s", key)
	}

	// Write updated config
	a.cfg.mu.Lock()
	a.cfg.cfg = cfg
	a.cfg.mu.Unlock()
	return a.cfg.save()
}

// ExportConfig returns the full config as a JSON string.
func (a *App) ExportConfig() (string, error) {
	data, err := a.cfg.ConfigJSON()
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ExportLogs returns the request logs as a JSON string.
func (a *App) ExportLogs() (string, error) {
	data, err := a.stats.LogsJSON()
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ── Log Methods ───────────────────────────────────────────────────────────────

// GetLogs returns recent request logs.
func (a *App) GetLogs(limit int) []RequestLog {
	logs := a.stats.GetLogs()
	if limit > 0 && len(logs) > limit {
		logs = logs[:limit]
	}
	return logs
}

// ── Dialog Methods ────────────────────────────────────────────────────────────

// ConfirmDelete shows a native confirmation dialog.
func (a *App) ConfirmDelete(title, message string) bool {
	resp, err := runtime.MessageDialog(a.ctx, runtime.MessageDialogOptions{
		Type:          runtime.QuestionDialog,
		Title:         title,
		Message:       message,
		Buttons:       []string{"Yes", "No"},
		DefaultButton: "No",
		CancelButton:  "No",
	})
	if err != nil {
		return false
	}
	return resp == "Yes"
}

// ShowAlert shows a native alert dialog.
func (a *App) ShowAlert(title, message string) {
	runtime.MessageDialog(a.ctx, runtime.MessageDialogOptions{
		Type:          runtime.InfoDialog,
		Title:         title,
		Message:       message,
		Buttons:       []string{"OK"},
		DefaultButton: "OK",
	})
}

// ── Window Methods ────────────────────────────────────────────────────────────

// MinimizeWindow minimizes the app window.
func (a *App) MinimizeWindow() {
	runtime.WindowMinimise(a.ctx)
}

// MaximizeWindow toggles the window between maximized and normal.
func (a *App) MaximizeWindow() {
	runtime.WindowToggleMaximise(a.ctx)
}

// CloseWindow closes the app.
func (a *App) CloseWindow() {
	runtime.Quit(a.ctx)
}
