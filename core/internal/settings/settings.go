// Package settings loads and persists user configuration.
package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"orbsleep/internal/model"
	"orbsleep/internal/state"
)

const storeKey = "settings"

// Manager owns the persisted settings blob.
type Manager struct {
	store   *state.Store
	dataDir string
}

// New returns a settings manager for the given data directory.
func New(store *state.Store, dataDir string) *Manager {
	return &Manager{store: store, dataDir: dataDir}
}

// DataDir returns the application support directory used by OrbSleep.
func DataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", "OrbSleep"), nil
}

// SocketPath is the default Unix socket path.
func SocketPath(dataDir string) string { return filepath.Join(dataDir, "daemon.sock") }

// Defaults builds default settings for a data directory.
func Defaults(dataDir string) model.Settings {
	return model.Settings{
		IdleTimeoutSecs:     1800,
		StartTimeoutSecs:    60,
		ProxyPortStart:      20000,
		ProxyPortEnd:        20099,
		AdminPort:           47811,
		WebPort:             47810,
		AutoScanOnLaunch:    false,
		EnableLowPortHelper: false,
		LogRetentionDays:    14,
		BackupDir:           filepath.Join(dataDir, "backups"),
		DataDir:             dataDir,
		SocketPath:          SocketPath(dataDir),
		ProxyDialMode:       "published",
		ExcludeImagePatterns: []string{
			"mysql", "mariadb", "postgres", "postgresql", "redis", "mongo",
			"rabbitmq", "kafka", "elasticsearch", "clickhouse", "influxdb",
			"memcached", "etcd", "zookeeper", "cockroach", "sqlserver", "oracle",
		},
	}
}

// Get returns the effective settings (defaults merged with the stored blob).
func (m *Manager) Get() model.Settings {
	s := Defaults(m.dataDir)
	raw, ok := m.store.GetSetting(storeKey)
	if !ok || raw == "" {
		return s
	}
	var stored model.Settings
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return s
	}
	merge(&s, stored)
	s.DataDir = m.dataDir
	s.SocketPath = SocketPath(m.dataDir)
	if !filepath.IsAbs(s.BackupDir) {
		s.BackupDir = filepath.Join(m.dataDir, s.BackupDir)
	}
	return s
}

// MigrateLegacy normalises persisted settings that enabled features which are
// no longer implemented (or are no longer supported).
func (m *Manager) MigrateLegacy() (bool, error) {
	s := m.Get()
	changed := false
	if s.EnableLowPortHelper {
		s.EnableLowPortHelper = false
		changed = true
	}
	if !changed {
		return false, nil
	}
	return true, m.Save(s)
}

// Save persists the given settings, keeping derived fields out of the database.
func (m *Manager) Save(s model.Settings) error {
	if s.ProxyPortStart < 1 || s.ProxyPortEnd > 65535 || s.ProxyPortEnd <= s.ProxyPortStart {
		return fmt.Errorf("代理端口范围无效: %d-%d", s.ProxyPortStart, s.ProxyPortEnd)
	}
	if s.ProxyPortEnd-s.ProxyPortStart+1 > 1000 {
		return fmt.Errorf("代理端口范围过大（最多 1000 个端口）")
	}
	if s.IdleTimeoutSecs < 0 {
		return fmt.Errorf("空闲超时不能为负数")
	}
	if s.StartTimeoutSecs < 1 {
		return fmt.Errorf("启动等待时间必须大于 0")
	}
	if s.ProxyDialMode != "published" && s.ProxyDialMode != "container_ip" {
		return fmt.Errorf("未知的代理连接模式: %s", s.ProxyDialMode)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return m.store.SetSetting(storeKey, string(raw))
}

func merge(dst *model.Settings, src model.Settings) {
	if src.IdleTimeoutSecs != 0 {
		dst.IdleTimeoutSecs = src.IdleTimeoutSecs
	}
	if src.StartTimeoutSecs != 0 {
		dst.StartTimeoutSecs = src.StartTimeoutSecs
	}
	if src.ProxyPortStart != 0 {
		dst.ProxyPortStart = src.ProxyPortStart
	}
	if src.ProxyPortEnd != 0 {
		dst.ProxyPortEnd = src.ProxyPortEnd
	}
	if src.AdminPort != 0 {
		dst.AdminPort = src.AdminPort
	}
	if src.WebPort != 0 {
		dst.WebPort = src.WebPort
	}
	if src.LogRetentionDays != 0 {
		dst.LogRetentionDays = src.LogRetentionDays
	}
	if src.BackupDir != "" {
		dst.BackupDir = src.BackupDir
	}
	if src.ProxyDialMode != "" {
		dst.ProxyDialMode = src.ProxyDialMode
	}
	if src.ExcludeImagePatterns != nil {
		dst.ExcludeImagePatterns = src.ExcludeImagePatterns
	}
	// Booleans need to follow the stored value even when false; a JSON round
	// trip through the stored blob means they are only applied when present,
	// which the merge cannot distinguish. We therefore copy them directly from
	// the stored blob via the caller path below.
	dst.AutoScanOnLaunch = src.AutoScanOnLaunch
	dst.EnableLowPortHelper = src.EnableLowPortHelper
}
