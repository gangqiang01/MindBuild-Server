package handler

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

// emailTestSettings is a minimal SystemSettingService stand-in that
// returns a fixed email.* configuration. It implements the whole
// interface (embedding nothing) so it can stand in for the real
// service in handler tests that only exercise email resolution.
type emailTestSettings struct {
	enabled    bool
	host       string
	port       int64
	username   string
	password   string
	encryption string
	from       string
	siteURL    string
}

func (s emailTestSettings) GetInt(_ context.Context, key, _ string, def int64) int64 {
	if key == "email.smtp_port" {
		if s.port != 0 {
			return s.port
		}
		return def
	}
	return def
}
func (s emailTestSettings) GetString(_ context.Context, key, _ string, def string) string {
	switch key {
	case "email.smtp_host":
		if s.host != "" {
			return s.host
		}
		return def
	case "email.smtp_username":
		return s.username
	case "email.smtp_password":
		return s.password
	case "email.smtp_encryption":
		if s.encryption != "" {
			return s.encryption
		}
		return def
	case "email.from_address":
		return s.from
	case "email.site_url":
		return s.siteURL
	}
	return def
}
func (s emailTestSettings) GetBool(_ context.Context, key, _ string, def bool) bool {
	if key == "email.enabled" {
		return s.enabled
	}
	return def
}
func (s emailTestSettings) GetStringList(_ context.Context, _ string, _ string, def []string) []string {
	return def
}
func (s emailTestSettings) List(context.Context) ([]*types.SystemSetting, error) { return nil, nil }
func (s emailTestSettings) Get(context.Context, string) (*types.SystemSetting, error) {
	return nil, nil
}
func (s emailTestSettings) Update(context.Context, string, any) (*types.SystemSetting, error) {
	return nil, nil
}
func (s emailTestSettings) Reset(context.Context, string) error  { return nil }
func (s emailTestSettings) SubscribeRedis(context.Context) error { return nil }

func TestBuildMailMessageEncodesSubjectAndBody(t *testing.T) {
	msg := buildMailMessage("bot@example.com", []string{"alice@example.com"}, "你的账号", "密码: P@ssw0rd")
	s := string(msg)
	if !strings.HasPrefix(s, "From: bot@example.com\r\n") {
		t.Fatalf("missing From header: %q", s)
	}
	if !strings.Contains(s, "To: alice@example.com\r\n") {
		t.Fatalf("missing To header: %q", s)
	}
	// Subject is RFC 2047 B-encoded — the raw UTF-8 must NOT appear.
	if !strings.Contains(s, "Subject: =?UTF-8?B?") {
		t.Fatalf("subject not RFC2047-encoded: %q", s)
	}
	if strings.Contains(s, "你的账号") {
		t.Fatalf("raw UTF-8 subject leaked into headers: %q", s)
	}
	if !strings.Contains(s, "Content-Transfer-Encoding: base64") {
		t.Fatalf("expected base64 transfer encoding: %q", s)
	}
	wantBody := base64.StdEncoding.EncodeToString([]byte("密码: P@ssw0rd"))
	if !strings.Contains(s, wantBody) {
		t.Fatalf("base64 body missing/wrong: %q (want %s)", s, wantBody)
	}
}

func TestResolveEmailConfigReadsAllKeys(t *testing.T) {
	cfg := resolveEmailConfig(context.Background(), emailTestSettings{
		enabled: true, host: "smtp.example.com", port: 465, username: "bot",
		password: "secret", encryption: "ssl", from: "noreply@example.com", siteURL: "https://app.example.com",
	})
	if !cfg.Enabled || cfg.Host != "smtp.example.com" || cfg.Port != 465 {
		t.Fatalf("unexpected cfg: %+v", cfg)
	}
	if cfg.Username != "bot" || cfg.Password != "secret" || cfg.Encryption != "ssl" {
		t.Fatalf("unexpected cfg: %+v", cfg)
	}
	if cfg.FromAddress != "noreply@example.com" || cfg.SiteURL != "https://app.example.com" {
		t.Fatalf("unexpected cfg: %+v", cfg)
	}
	if !cfg.sendable() {
		t.Fatalf("enabled+host must be sendable")
	}
	if cfg.effectiveFrom() != "noreply@example.com" {
		t.Fatalf("effectiveFrom should honour from_address")
	}
}

func TestResolveEmailConfigNilSvcYieldsDisabled(t *testing.T) {
	cfg := resolveEmailConfig(context.Background(), nil)
	if cfg.Enabled || cfg.sendable() {
		t.Fatalf("nil svc must yield a non-sendable zero config: %+v", cfg)
	}
}

func TestNotifyCreatedUserStatuses(t *testing.T) {
	user := &types.User{ID: "u1", Username: "alice", Email: "alice@example.com"}
	ctx := context.Background()

	h := &SystemHandler{}

	// No settings service wired → disabled (the unit-test / lite-deploy path).
	status, errDetail := h.notifyCreatedUser(ctx, user, "PlainPass9")
	if status != emailStatusDisabled || errDetail != "" {
		t.Fatalf("nil svc: status=%q err=%q, want disabled", status, errDetail)
	}

	// Enabled but no host → not_configured.
	h.systemSettingSvc = emailTestSettings{enabled: true}
	status, _ = h.notifyCreatedUser(ctx, user, "PlainPass9")
	if status != emailStatusNotConfigured {
		t.Fatalf("enabled-without-host: status=%q, want not_configured", status)
	}

	// Enabled with host but the relay is unreachable → failed (no password
	// leak in the surfaced error detail is asserted separately by the
	// audit-leak tests; here we only assert the status). Dialing a refused
	// port returns immediately, so this stays fast.
	h.systemSettingSvc = emailTestSettings{enabled: true, host: "127.0.0.1", port: 1, encryption: "none"}
	status, errDetail = h.notifyCreatedUser(ctx, user, "PlainPass9")
	if status != emailStatusFailed || errDetail == "" {
		t.Fatalf("unreachable relay: status=%q err=%q, want failed+reason", status, errDetail)
	}

	// No credential in hand → not_configured (avoids sending a credential
	// mail without the credential).
	h.systemSettingSvc = emailTestSettings{enabled: true, host: "smtp.example.com"}
	status, _ = h.notifyCreatedUser(ctx, user, "")
	if status != emailStatusNotConfigured {
		t.Fatalf("empty password: status=%q, want not_configured", status)
	}
}

func TestEffectiveFromFallsBackToUsername(t *testing.T) {
	cfg := emailConfig{Username: "bot@example.com"}
	if cfg.effectiveFrom() != "bot@example.com" {
		t.Fatalf("effectiveFrom should fall back to username, got %q", cfg.effectiveFrom())
	}
}
