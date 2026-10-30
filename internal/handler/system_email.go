package handler

// SMTP email notification support for SystemAdmin flows.
//
// The relay configuration lives in the system_settings registry
// (email.* keys — see service/system_setting.go). Every send re-reads
// the settings so UI edits take effect immediately; there is no
// in-process SMTP client pool to invalidate.
//
// Core contract (per product requirement): sending email is strictly
// best-effort. A missing relay, a failed send, or a nil settings service
// must NEVER fail the business operation that triggered the send —
// user creation returns 201 regardless, and the caller learns the mail
// outcome via CreateSystemUserResponse.email_status.

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
)

// EMail status values reported to the UI (CreateSystemUserResponse.email_status).
const (
	// emailStatusSent: the notification mail was handed to the relay.
	emailStatusSent = "sent"
	// emailStatusFailed: a relay was configured but the send errored.
	// email_error carries the reason.
	emailStatusFailed = "failed"
	// emailStatusDisabled: email.enabled is off — no send attempted.
	emailStatusDisabled = "disabled"
	// emailStatusNotConfigured: email.enabled is on but smtp_host is empty.
	emailStatusNotConfigured = "not_configured"
)

// emailConfig is the resolved SMTP relay configuration for one send.
type emailConfig struct {
	Enabled     bool
	Host        string
	Port        int64
	Username    string
	Password    string
	Encryption  string // starttls | ssl | none
	FromAddress string
	SiteURL     string
}

// resolveEmailConfig reads the email.* system settings (DB > ENV >
// default). A nil svc yields the zero config so partially-wired tests
// and lite deployments degrade to "not configured" instead of panicking.
func resolveEmailConfig(ctx context.Context, svc interfaces.SystemSettingService) emailConfig {
	var cfg emailConfig
	if svc == nil {
		return cfg
	}
	cfg.Enabled = svc.GetBool(ctx, "email.enabled", "WEKNORA_EMAIL_ENABLED", false)
	cfg.Host = strings.TrimSpace(svc.GetString(ctx, "email.smtp_host", "WEKNORA_EMAIL_SMTP_HOST", ""))
	cfg.Port = svc.GetInt(ctx, "email.smtp_port", "WEKNORA_EMAIL_SMTP_PORT", 587)
	cfg.Username = strings.TrimSpace(svc.GetString(ctx, "email.smtp_username", "WEKNORA_EMAIL_SMTP_USERNAME", ""))
	cfg.Password = svc.GetString(ctx, "email.smtp_password", "WEKNORA_EMAIL_SMTP_PASSWORD", "")
	cfg.Encryption = svc.GetString(ctx, "email.smtp_encryption", "WEKNORA_EMAIL_SMTP_ENCRYPTION", "starttls")
	cfg.FromAddress = strings.TrimSpace(svc.GetString(ctx, "email.from_address", "WEKNORA_EMAIL_FROM_ADDRESS", ""))
	cfg.SiteURL = strings.TrimSpace(svc.GetString(ctx, "email.site_url", "WEKNORA_EMAIL_SITE_URL", ""))
	return cfg
}

// sendable reports whether the relay is configured enough to attempt a
// send. Checked separately from Enabled so the UI can distinguish
// "通知未开启" from "未配置邮件服务器".
func (c emailConfig) sendable() bool {
	return c.Enabled && c.Host != ""
}

// effectiveFrom picks the envelope/headers From address: the explicit
// setting, else the SMTP username (the common "sender = login" layout).
func (c emailConfig) effectiveFrom() string {
	if c.FromAddress != "" {
		return c.FromAddress
	}
	return c.Username
}

// buildMailMessage assembles an RFC 5322 message with UTF-8 headers and a
// base64 body. Base64 sidesteps dot-stuffing and the 998-byte line limit;
// the subject is RFC 2047 B-encoded because raw UTF-8 in headers is only
// valid under RFC 6532 and several relays still reject it. Pure function
// so tests can pin the wire format without a socket.
func buildMailMessage(from string, to []string, subject, body string) []byte {
	var b strings.Builder
	writeHeader := func(k, v string) {
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(v)
		b.WriteString("\r\n")
	}
	writeHeader("From", from)
	writeHeader("To", strings.Join(to, ", "))
	writeHeader("Subject", "=?UTF-8?B?"+base64.StdEncoding.EncodeToString([]byte(subject))+"?=")
	// Date and Message-ID are required by RFC 5322 and several relays /
	// spam filters silently drop messages that lack them — the test mail
	// to oneself may slip through, but a credentials mail to an external
	// recipient often won't. Generate both so deliverability matches the
	// test path regardless of recipient.
	writeHeader("Date", time.Now().Format(time.RFC1123Z))
	writeHeader("Message-ID", buildMessageID(from))
	writeHeader("MIME-Version", "1.0")
	writeHeader("Content-Type", "text/plain; charset=UTF-8")
	writeHeader("Content-Transfer-Encoding", "base64")
	b.WriteString("\r\n")
	b.WriteString(base64.StdEncoding.EncodeToString([]byte(body)))
	b.WriteString("\r\n")
	return []byte(b.String())
}

// buildMessageID builds an RFC 5322 Message-ID, using the sender's domain
// (derived from the From address) as the right-hand side. Falls back to
// "localhost" when From has no '@'.
func buildMessageID(from string) string {
	domain := "localhost"
	if i := strings.LastIndex(from, "@"); i >= 0 && i+1 < len(from) {
		domain = strings.TrimRight(from[i+1:], " \t>")
	}
	return fmt.Sprintf("<%d.%d@%s>", time.Now().UnixNano(), os.Getpid(), domain)
}

// sendMail delivers one message over SMTP. Supports implicit TLS
// ("ssl"), STARTTLS ("starttls", the default) and plaintext ("none").
// The whole conversation is bounded by smtpSendTimeout so a hung relay
// cannot stall the caller indefinitely.
const smtpSendTimeout = 10 * time.Second

func sendMail(ctx context.Context, cfg emailConfig, to []string, subject, body string) error {
	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))
	dialer := &net.Dialer{Timeout: smtpSendTimeout}

	var conn net.Conn
	var err error
	if cfg.Encryption == "ssl" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: cfg.Host})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	// Bound the post-dial conversation too — DialContext's timeout only
	// covers connection establishment.
	_ = conn.SetDeadline(time.Now().Add(smtpSendTimeout))
	defer conn.Close()

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return fmt.Errorf("smtp handshake: %w", err)
	}
	defer client.Close()

	if cfg.Encryption == "starttls" {
		// STARTTLS is the whole point of this mode — if the server doesn't
		// advertise it, proceeding would send credentials in cleartext. Fail
		// with an actionable message instead of letting PlainAuth reject the
		// send later with the cryptic "unencrypted connection".
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fmt.Errorf("服务器在端口 %d 上不支持 STARTTLS；请改用 SSL/TLS（通常 465 端口）或确认端口与加密方式匹配", cfg.Port)
		} else if err := client.StartTLS(&tls.Config{ServerName: cfg.Host}); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}

	if cfg.Username != "" {
		// We secure the connection ourselves (implicit TLS for "ssl",
		// STARTTLS for "starttls"; "none" is the admin's explicit opt-in to
		// plaintext). net/smtp.PlainAuth refuses to run unless smtp.Client
		// believes the connection is TLS — but the client can't see that
		// for an implicit-TLS (465) connection, so it would reject a
		// perfectly secure send with "unencrypted connection". Use our own
		// auth that makes the same security decision at the call site.
		auth := newSMTPAuth(client, cfg.Username, cfg.Password)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}

	if err := client.Mail(cfg.effectiveFrom()); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("smtp RCPT TO %s: %w", rcpt, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	if _, err := w.Write(buildMailMessage(cfg.effectiveFrom(), to, subject, body)); err != nil {
		_ = w.Close()
		return fmt.Errorf("write message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("finish message: %w", err)
	}
	return client.Quit()
}

// smtpAuth implements AUTH PLAIN / LOGIN without net/smtp.PlainAuth's
// "is the connection TLS?" guard. sendMail only constructs it AFTER it has
// secured the connection itself (tls.Dial for "ssl", StartTLS for
// "starttls", or the admin's explicit "none" opt-in), so the stdlib guard
// — which cannot see that wrapping — would wrongly reject auth on a 465
// implicit-TLS relay with "unencrypted connection". We make that call here.
//
// PLAIN is preferred (single round-trip); LOGIN is the fallback for
// servers (notably some Chinese providers) that don't advertise PLAIN.
type smtpAuth struct {
	username string
	password string
	mech     string // "PLAIN" or "LOGIN"
	step     int    // LOGIN challenge counter
}

func newSMTPAuth(client *smtp.Client, username, password string) *smtpAuth {
	mech := "PLAIN"
	if ok, param := client.Extension("AUTH"); ok {
		// RFC 4954: the AUTH extension value is a space-separated mech list.
		mechs := strings.Fields(strings.ReplaceAll(param, ",", " "))
		hasPlain, hasLogin := false, false
		for _, m := range mechs {
			switch strings.ToUpper(m) {
			case "PLAIN":
				hasPlain = true
			case "LOGIN":
				hasLogin = true
			}
		}
		if !hasPlain && hasLogin {
			mech = "LOGIN"
		}
	}
	return &smtpAuth{username: username, password: password, mech: mech}
}

func (a *smtpAuth) Start(_ *smtp.ServerInfo) (string, []byte, error) {
	a.step = 0
	if a.mech == "LOGIN" {
		// Send no initial response; wait for the "Username:" challenge.
		return "LOGIN", nil, nil
	}
	// PLAIN: identity\x00username\x00password in one shot.
	return "PLAIN", []byte("\x00" + a.username + "\x00" + a.password), nil
}

func (a *smtpAuth) Next(_ []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	// LOGIN: first challenge → username, second → password.
	a.step++
	if a.step == 1 {
		return []byte(a.username), nil
	}
	return []byte(a.password), nil
}

// buildNewUserEmail renders the notification sent after a SystemAdmin
// creates a local account. Bilingual-free by design: the platform's
// admin flow is operated in the admin's locale, but the RECIPIENT's
// locale is unknown, so the mail leads with Chinese (the deployment
// language) — operators can localise later by extending this builder.
// The plaintext password is intentional: this mail IS the delivery
// channel for the one-time credential.
func buildNewUserEmail(cfg emailConfig, username, password string) (string, string) {
	subject := "你的 MindBuilder 账号已创建"
	var b strings.Builder
	b.WriteString("你好，\r\n\r\n")
	b.WriteString("管理员刚刚为你创建了 MindBuilder 账号，以下是你的登录信息：\r\n\r\n")
	b.WriteString("  用户名：" + username + "\r\n")
	b.WriteString("  初始密码：" + password + "\r\n\r\n")
	if cfg.SiteURL != "" {
		b.WriteString("登录地址：" + cfg.SiteURL + "\r\n\r\n")
	}
	b.WriteString("为了账号安全，请登录后尽快修改密码。\r\n\r\n")
	b.WriteString("（本邮件由系统自动发送，请勿回复。）\r\n")
	return subject, b.String()
}

// notifyCreatedUser attempts the post-creation credential mail.
// Returns the email_status for the API response plus the failure
// reason (empty unless failed). Never returns an error that callers
// should propagate — user creation has already succeeded.
func (h *SystemHandler) notifyCreatedUser(ctx context.Context, user *types.User, password string) (status, errDetail string) {
	cfg := resolveEmailConfig(ctx, h.systemSettingSvc)
	logger.Infof(ctx, "[email] notifyCreatedUser: user=%s email=%q hasPassword=%v enabled=%v host=%q port=%d encryption=%q from=%q",
		user.ID, user.Email, password != "", cfg.Enabled, cfg.Host, cfg.Port, cfg.Encryption, cfg.effectiveFrom())
	if !cfg.Enabled {
		logger.Warnf(ctx, "[email] notifyCreatedUser skipped for user %s: email.enabled is off", user.ID)
		return emailStatusDisabled, ""
	}
	if !cfg.sendable() {
		logger.Warnf(ctx, "[email] notifyCreatedUser skipped for user %s: smtp_host is empty", user.ID)
		return emailStatusNotConfigured, ""
	}
	if password == "" {
		// No usable credential in hand (e.g. hand-edited request shape);
		// sending a credential mail without the credential would only
		// confuse the recipient.
		logger.Warnf(ctx, "[email] notifyCreatedUser skipped for user %s: no credential in hand", user.ID)
		return emailStatusNotConfigured, ""
	}
	if strings.TrimSpace(user.Email) == "" {
		logger.Warnf(ctx, "[email] notifyCreatedUser skipped for user %s: user has no email address", user.ID)
		return emailStatusNotConfigured, ""
	}
	subject, body := buildNewUserEmail(cfg, user.Username, password)
	if err := sendMail(ctx, cfg, []string{user.Email}, subject, body); err != nil {
		logger.Warnf(ctx, "[email] credential email to %s failed (user %s already created): %v", user.Email, user.ID, err)
		return emailStatusFailed, err.Error()
	}
	logger.Infof(ctx, "[email] credential email sent to %s for user %s", user.Email, user.ID)
	return emailStatusSent, ""
}

// TestEmailRequest is the body for POST /system/admin/email/test.
type TestEmailRequest struct {
	// To is the recipient of the probe mail. The admin's own address is
	// the expected value; any syntactically valid address is accepted.
	To string `json:"to" binding:"required,email"`
}

// TestSystemEmailConfig godoc
// @Summary      Send a test email through the configured SMTP relay
// @Description  Sends a short probe mail to `to` using the saved email.*
//
//	system settings. Use it to verify host/port/credentials
//	before relying on user-creation notifications. Sending
//	is synchronous and bounded by a 10s timeout.
//
// @Tags         System Admin
// @Accept       json
// @Produce      json
// @Param        request body TestEmailRequest true "Test recipient"
// @Success      200 {object} map[string]interface{} "{ok: true}"
// @Failure      400 {object} map[string]interface{} "Invalid request or relay not configured"
// @Failure      502 {object} map[string]interface{} "Relay rejected the send"
// @Router       /system/admin/email/test [post]
func (h *SystemHandler) TestSystemEmailConfig(c *gin.Context) {
	ctx := logger.CloneContext(c.Request.Context())

	var req TestEmailRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "A valid recipient email is required"})
		return
	}

	cfg := resolveEmailConfig(ctx, h.systemSettingSvc)
	if !cfg.Enabled {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Email notification is disabled (email.enabled)"})
		return
	}
	if !cfg.sendable() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "SMTP server is not configured (email.smtp_host)"})
		return
	}

	subject := "MindBuilder 测试邮件"
	body := "这是一封测试邮件。\r\n\r\n如果你收到它，说明邮件服务器配置正确。\r\n\r\n（本邮件由系统自动发送，请勿回复。）\r\n"
	if err := sendMail(ctx, cfg, []string{req.To}, subject, body); err != nil {
		logger.Warnf(ctx, "test email to %s failed: %v", req.To, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	logger.Infof(ctx, "test email sent to %s", req.To)
	c.JSON(http.StatusOK, gin.H{"ok": true, "message": "Test email sent"})
}
