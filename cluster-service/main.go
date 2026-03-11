package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

const version = "0.2.0"

var db *sql.DB

// InboundEmail represents an email from any source (CF Worker or Stalwart)
type InboundEmail struct {
	To         string `json:"to"`
	From       string `json:"from"`
	Subject    string `json:"subject"`
	LocalPart  string `json:"localPart"`
	Raw        string `json:"raw"`
	ReceivedAt string `json:"receivedAt"`
}

// OTPMatch represents an extracted OTP/verification code
type OTPMatch struct {
	Code   string `json:"code"`
	Type   string `json:"type"`
	Source string `json:"source"`
}

// StoredEmail is what we return from queries
type StoredEmail struct {
	ID          string     `json:"id"`
	Recipient   string     `json:"recipient"`
	LocalPart   string     `json:"localPart"`
	Sender      string     `json:"sender"`
	Subject     string     `json:"subject"`
	ReceivedAt  time.Time  `json:"receivedAt"`
	ProcessedAt time.Time  `json:"processedAt"`
	Codes       []OTPMatch `json:"codes,omitempty"`
}

// --- Stalwart Webhook Types ---

// StalwartWebhookPayload is the top-level webhook payload from Stalwart
type StalwartWebhookPayload struct {
	Events []StalwartEvent `json:"events"`
}

// StalwartEvent represents a single event in the webhook payload
type StalwartEvent struct {
	ID        string                 `json:"id"`
	CreatedAt string                 `json:"createdAt"`
	Type      string                 `json:"type"`
	Data      map[string]interface{} `json:"data"`
}

// --- JMAP Types ---

// JMAPRequest is a minimal JMAP request
type JMAPRequest struct {
	Using       []string        `json:"using"`
	MethodCalls [][]interface{} `json:"methodCalls"`
}

// JMAPResponse is a minimal JMAP response
type JMAPResponse struct {
	MethodResponses [][]json.RawMessage `json:"methodResponses"`
}

// JMAPEmailGetResponse is the response from Email/get
type JMAPEmailGetResponse struct {
	AccountID string      `json:"accountId"`
	List      []JMAPEmail `json:"list"`
}

// JMAPEmail represents a single email from JMAP
type JMAPEmail struct {
	ID         string                 `json:"id"`
	From       []JMAPAddress          `json:"from"`
	To         []JMAPAddress          `json:"to"`
	Subject    string                 `json:"subject"`
	TextBody   []JMAPBodyPart         `json:"textBody"`
	BodyValues map[string]JMAPBodyVal `json:"bodyValues"`
}

// JMAPAddress is a JMAP email address
type JMAPAddress struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// JMAPBodyPart references a body value
type JMAPBodyPart struct {
	PartID string `json:"partId"`
	Type   string `json:"type"`
}

// JMAPBodyVal holds the actual body content
type JMAPBodyVal struct {
	Value string `json:"value"`
}

// Common OTP patterns
var otpPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:code|otp|pin|token)[:\s]+(\d{4,8})`),
	regexp.MustCompile(`(?i)(?:verification|confirm)[:\s]+(\d{4,8})`),
	regexp.MustCompile(`(?i)(\d{6})\s+is your (?:verification|confirmation|login) code`),
	regexp.MustCompile(`(?i)your (?:code|otp|pin) is[:\s]+(\d{4,8})`),
	regexp.MustCompile(`(?i)enter (?:the )?(?:code|otp)[:\s]+(\d{4,8})`),
}

var linkPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(https?://[^\s"<]+(?:verify|confirm|activate|auth|token|magic)[^\s"<]*)`),
}

func extractOTPs(body string) []OTPMatch {
	var matches []OTPMatch
	seen := make(map[string]bool)

	for _, pattern := range otpPatterns {
		for _, match := range pattern.FindAllStringSubmatch(body, -1) {
			if len(match) > 1 && !seen[match[1]] {
				seen[match[1]] = true
				matches = append(matches, OTPMatch{Code: match[1], Type: "otp", Source: match[0]})
			}
		}
	}

	for _, pattern := range linkPatterns {
		for _, match := range pattern.FindAllStringSubmatch(body, -1) {
			if len(match) > 0 && !seen[match[0]] {
				seen[match[0]] = true
				matches = append(matches, OTPMatch{Code: match[0], Type: "verification_link", Source: match[0]})
			}
		}
	}

	return matches
}

// storeEmail persists the email and extracted codes to CockroachDB
func storeEmail(email InboundEmail, otps []OTPMatch) (string, error) {
	if db == nil {
		return "", fmt.Errorf("database not connected")
	}

	receivedAt, err := time.Parse(time.RFC3339, email.ReceivedAt)
	if err != nil {
		receivedAt = time.Now().UTC()
	}

	var emailID string
	err = db.QueryRowContext(context.Background(),
		`INSERT INTO inbound_emails (recipient, local_part, sender, subject, raw_body, received_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id`,
		email.To, email.LocalPart, email.From, email.Subject, email.Raw, receivedAt,
	).Scan(&emailID)
	if err != nil {
		return "", fmt.Errorf("insert email: %w", err)
	}

	for _, otp := range otps {
		_, err := db.ExecContext(context.Background(),
			`INSERT INTO extracted_codes (email_id, code, code_type, source)
			 VALUES ($1, $2, $3, $4)`,
			emailID, otp.Code, otp.Type, otp.Source,
		)
		if err != nil {
			log.Printf("[DB] Failed to insert code for email %s: %v", emailID, err)
		}
	}

	log.Printf("[DB] Stored email %s for %s (%d codes)", emailID, email.LocalPart, len(otps))
	return emailID, nil
}

// Discord bot IDs mapped to their email local parts
var clawDiscordIDs = map[string]string{
	"smokeyclaw": "1479686258560077876",
	"jakeclaw":   "1475254070989295656",
	"shopclaw":   "1475320147098206230",
	"jathyclaw":  "1479742782464852092",
	"pinchy":     "1472394447139377266",
	"pinchyclaw": "1472394447139377266",
	"nimbleclaw": "1480458197423886508",
	"oracleclaw": "1480458226896994325",
}

// postToDiscord sends an email notification to the Discord #email channel via webhook
func postToDiscord(email InboundEmail, otps []OTPMatch) {
	webhookURL := os.Getenv("DISCORD_WEBHOOK_URL")
	if webhookURL == "" {
		return
	}

	var sb strings.Builder

	// Tag the claw if we know their Discord ID
	if discordID, ok := clawDiscordIDs[strings.ToLower(email.LocalPart)]; ok {
		sb.WriteString(fmt.Sprintf("<@%s> ", discordID))
	}

	sb.WriteString(fmt.Sprintf("📧 **%s** → `%s`\n", email.From, email.To))
	sb.WriteString(fmt.Sprintf("**Subject:** %s\n", email.Subject))

	if len(otps) > 0 {
		sb.WriteString("\n🔑 ")
		for i, otp := range otps {
			if i > 0 {
				sb.WriteString(" | ")
			}
			if otp.Type == "otp" {
				sb.WriteString(fmt.Sprintf("`%s`", otp.Code))
			} else {
				sb.WriteString(fmt.Sprintf("<%s>", otp.Code))
			}
		}
		sb.WriteString("\n")
	}

	body := extractBody(email.Raw)
	if len(body) > 800 {
		body = body[:800] + "…"
	}
	if body != "" {
		sb.WriteString("\n" + body)
	}

	payload := map[string]interface{}{
		"content":  sb.String(),
		"username": "📧 Email Handler",
	}

	jsonPayload, _ := json.Marshal(payload)
	resp, err := http.Post(webhookURL, "application/json", bytes.NewReader(jsonPayload))
	if err != nil {
		log.Printf("[DISCORD] Failed to post: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		log.Printf("[DISCORD] Webhook returned %d", resp.StatusCode)
	} else {
		log.Printf("[DISCORD] Notification sent for %s", email.To)
	}
}

func extractBody(raw string) string {
	parts := strings.SplitN(raw, "\r\n\r\n", 2)
	if len(parts) == 2 {
		return strings.TrimSpace(parts[1])
	}
	parts = strings.SplitN(raw, "\n\n", 2)
	if len(parts) == 2 {
		return strings.TrimSpace(parts[1])
	}
	return ""
}

// processEmail is the shared pipeline: extract OTPs, store, notify Discord
func processEmail(email InboundEmail, source string) {
	log.Printf("[%s] to=%s from=%s subject=%q localPart=%s",
		source, email.To, email.From, email.Subject, email.LocalPart)

	otps := extractOTPs(email.Raw)
	if len(otps) > 0 {
		log.Printf("[OTP] Found %d codes/links for %s:", len(otps), email.LocalPart)
		for _, otp := range otps {
			log.Printf("  - [%s] %s", otp.Type, otp.Code)
		}
	}

	// Store in CockroachDB
	emailID, err := storeEmail(email, otps)
	if err != nil {
		log.Printf("[DB] Failed to store: %v", err)
	}

	// Post to Discord
	go postToDiscord(email, otps)

	_ = emailID // used in log above
}

// --- Stalwart JMAP Client ---

// fetchEmailViaJMAP fetches the full email content from Stalwart using JMAP
func fetchEmailViaJMAP(accountID, emailID string) (*JMAPEmail, error) {
	jmapURL := os.Getenv("STALWART_JMAP_URL")
	if jmapURL == "" {
		jmapURL = "https://mail.only-claws.net/jmap"
	}

	token := os.Getenv("STALWART_API_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("STALWART_API_TOKEN not set")
	}

	reqBody := JMAPRequest{
		Using: []string{
			"urn:ietf:params:jmap:core",
			"urn:ietf:params:jmap:mail",
		},
		MethodCalls: [][]interface{}{
			{
				"Email/get",
				map[string]interface{}{
					"accountId":          accountID,
					"ids":                []string{emailID},
					"properties":         []string{"from", "to", "subject", "textBody", "bodyValues"},
					"fetchTextBodyValues": true,
				},
				"0",
			},
		},
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal JMAP request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, jmapURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("create JMAP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("JMAP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("JMAP returned %d: %s", resp.StatusCode, string(body))
	}

	var jmapResp JMAPResponse
	if err := json.NewDecoder(resp.Body).Decode(&jmapResp); err != nil {
		return nil, fmt.Errorf("decode JMAP response: %w", err)
	}

	if len(jmapResp.MethodResponses) == 0 || len(jmapResp.MethodResponses[0]) < 2 {
		return nil, fmt.Errorf("empty JMAP response")
	}

	var emailGetResp JMAPEmailGetResponse
	if err := json.Unmarshal(jmapResp.MethodResponses[0][1], &emailGetResp); err != nil {
		return nil, fmt.Errorf("decode Email/get response: %w", err)
	}

	if len(emailGetResp.List) == 0 {
		return nil, fmt.Errorf("email not found: %s", emailID)
	}

	return &emailGetResp.List[0], nil
}

// jmapEmailToInbound converts a JMAP email to our InboundEmail format
func jmapEmailToInbound(email *JMAPEmail) InboundEmail {
	var from, to string
	if len(email.From) > 0 {
		from = email.From[0].Email
	}
	if len(email.To) > 0 {
		to = email.To[0].Email
	}

	localPart := ""
	if idx := strings.Index(to, "@"); idx > 0 {
		localPart = strings.ToLower(to[:idx])
	}

	// Extract text body from bodyValues
	var rawBody string
	for _, part := range email.TextBody {
		if val, ok := email.BodyValues[part.PartID]; ok {
			rawBody = val.Value
			break
		}
	}

	return InboundEmail{
		To:         to,
		From:       from,
		Subject:    email.Subject,
		LocalPart:  localPart,
		Raw:        rawBody,
		ReceivedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

// --- HTTP Handlers ---

// handleRoot returns a simple status page
func handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"service": "email-handler",
		"version": version,
		"status":  "running",
		"routes": []string{
			"GET  /                       — this status page",
			"GET  /health                 — health check",
			"POST /email/inbound          — CF Worker inbound (legacy)",
			"POST /email/stalwart-webhook — Stalwart webhook receiver",
			"GET  /email/query/{name}     — query inbox by local part",
		},
	})
}

// handleInbound handles legacy CF Worker POST (kept as fallback)
func handleInbound(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	secret := os.Getenv("HANDLER_SECRET")
	if secret != "" {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+secret {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	var email InboundEmail
	if err := json.NewDecoder(r.Body).Decode(&email); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	processEmail(email, "CF-INBOUND")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":        true,
		"source":    "cloudflare",
		"recipient": email.LocalPart,
	})
}

// handleStalwartWebhook handles Stalwart webhook events
func handleStalwartWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Verify webhook secret if configured
	webhookSecret := os.Getenv("STALWART_WEBHOOK_SECRET")
	if webhookSecret != "" {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+webhookSecret {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	var payload StalwartWebhookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	log.Printf("[STALWART] Received webhook with %d event(s)", len(payload.Events))

	processed := 0
	for _, event := range payload.Events {
		log.Printf("[STALWART] Event: id=%s type=%s createdAt=%s", event.ID, event.Type, event.CreatedAt)

		// Process message-ingest events (new email received)
		if !strings.HasPrefix(event.Type, "message-ingest") {
			log.Printf("[STALWART] Skipping non-ingest event: %s", event.Type)
			continue
		}

		// Extract metadata from webhook event data
		accountID, _ := event.Data["accountId"].(string)
		messageID, _ := event.Data["messageId"].(string)

		// Extract envelope info from webhook data for immediate processing
		from, _ := event.Data["from"].(string)
		subject, _ := event.Data["subject"].(string)

		// Get recipients — could be string or []interface{}
		var to string
		switch v := event.Data["to"].(type) {
		case string:
			to = v
		case []interface{}:
			if len(v) > 0 {
				to, _ = v[0].(string)
			}
		}

		localPart := ""
		if idx := strings.Index(to, "@"); idx > 0 {
			localPart = strings.ToLower(to[:idx])
		}

		// Try to fetch full email via JMAP for body content + OTP extraction
		var email InboundEmail
		if accountID != "" && messageID != "" {
			jmapEmail, err := fetchEmailViaJMAP(accountID, messageID)
			if err != nil {
				log.Printf("[JMAP] Failed to fetch email %s: %v (using webhook metadata only)", messageID, err)
				// Fall back to webhook metadata only (no body for OTP extraction)
				email = InboundEmail{
					To:         to,
					From:       from,
					Subject:    subject,
					LocalPart:  localPart,
					Raw:        "",
					ReceivedAt: event.CreatedAt,
				}
			} else {
				log.Printf("[JMAP] Fetched email %s via JMAP ✓", messageID)
				email = jmapEmailToInbound(jmapEmail)
				// Prefer webhook createdAt for receivedAt
				if event.CreatedAt != "" {
					email.ReceivedAt = event.CreatedAt
				}
			}
		} else {
			log.Printf("[STALWART] No accountId/messageId in event data, using webhook metadata only")
			email = InboundEmail{
				To:         to,
				From:       from,
				Subject:    subject,
				LocalPart:  localPart,
				Raw:        "",
				ReceivedAt: event.CreatedAt,
			}
		}

		processEmail(email, "STALWART")
		processed++
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":        true,
		"source":    "stalwart",
		"received":  len(payload.Events),
		"processed": processed,
	})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	if db != nil {
		if err := db.Ping(); err != nil {
			status = "degraded (db unreachable)"
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  status,
		"version": version,
	})
}

// handleQuery lets claws check their inbox
func handleQuery(w http.ResponseWriter, r *http.Request) {
	localPart := strings.TrimPrefix(r.URL.Path, "/email/query/")
	if localPart == "" {
		http.Error(w, "missing localPart", http.StatusBadRequest)
		return
	}

	if db == nil {
		http.Error(w, "database not connected", http.StatusServiceUnavailable)
		return
	}

	rows, err := db.QueryContext(context.Background(),
		`SELECT e.id, e.recipient, e.local_part, e.sender, e.subject, e.received_at, e.processed_at
		 FROM inbound_emails e
		 WHERE e.local_part = $1
		 ORDER BY e.received_at DESC
		 LIMIT 10`, localPart)
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var emails []StoredEmail
	for rows.Next() {
		var e StoredEmail
		if err := rows.Scan(&e.ID, &e.Recipient, &e.LocalPart, &e.Sender, &e.Subject, &e.ReceivedAt, &e.ProcessedAt); err != nil {
			continue
		}

		// Fetch codes for this email
		codeRows, err := db.QueryContext(context.Background(),
			`SELECT code, code_type, source FROM extracted_codes WHERE email_id = $1`, e.ID)
		if err == nil {
			for codeRows.Next() {
				var c OTPMatch
				if err := codeRows.Scan(&c.Code, &c.Type, &c.Source); err == nil {
					e.Codes = append(e.Codes, c)
				}
			}
			codeRows.Close()
		}

		emails = append(emails, e)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":        true,
		"recipient": localPart,
		"count":     len(emails),
		"emails":    emails,
	})
}

func initDB() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgresql://root@cockroachdb-public.cockroachdb.svc.cluster.local:26257/email?sslmode=disable"
	}

	var err error
	db, err = sql.Open("postgres", dsn)
	if err != nil {
		log.Printf("[DB] Failed to open: %v", err)
		return
	}

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		log.Printf("[DB] Failed to ping: %v", err)
		db = nil
		return
	}

	log.Printf("[DB] Connected to CockroachDB ✓")
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	initDB()

	webhookURL := os.Getenv("DISCORD_WEBHOOK_URL")
	if webhookURL != "" {
		log.Printf("Discord webhook configured ✓")
	}

	stalwartToken := os.Getenv("STALWART_API_TOKEN")
	if stalwartToken != "" {
		jmapURL := os.Getenv("STALWART_JMAP_URL")
		if jmapURL == "" {
			jmapURL = "https://mail.only-claws.net/jmap"
		}
		log.Printf("Stalwart JMAP configured ✓ (%s)", jmapURL)
	} else {
		log.Printf("Stalwart JMAP not configured (STALWART_API_TOKEN missing)")
	}

	http.HandleFunc("/", handleRoot)
	http.HandleFunc("/health", handleHealth)
	http.HandleFunc("/email/inbound", handleInbound)
	http.HandleFunc("/email/stalwart-webhook", handleStalwartWebhook)
	http.HandleFunc("/email/query/", handleQuery)

	addr := fmt.Sprintf(":%s", port)
	log.Printf("email-handler v%s listening on %s", version, addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
