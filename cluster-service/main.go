package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

var db *sql.DB

// InboundEmail represents an email forwarded from the Cloudflare Worker
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
	"smokeyclaw":  "1479686258560077876",
	"jakeclaw":    "1475254070989295656",
	"shopclaw":    "1475320147098206230",
	"jathyclaw":   "1479742782464852092",
	"pinchy":      "1472394447139377266",
	"pinchyclaw":  "1472394447139377266",
	"nimbleclaw":  "1480458197423886508",
	"oracleclaw":  "1480458226896994325",
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

	sb.WriteString(fmt.Sprintf("📧 **New email for `%s`**\n", email.To))
	sb.WriteString(fmt.Sprintf("**From:** %s\n", email.From))
	sb.WriteString(fmt.Sprintf("**Subject:** %s\n", email.Subject))

	if len(otps) > 0 {
		sb.WriteString("\n🔑 **Extracted codes/links:**\n")
		for _, otp := range otps {
			if otp.Type == "otp" {
				sb.WriteString(fmt.Sprintf("- **OTP:** `%s`\n", otp.Code))
			} else {
				sb.WriteString(fmt.Sprintf("- **Link:** <%s>\n", otp.Code))
			}
		}
	}

	body := extractBody(email.Raw)
	if len(body) > 500 {
		body = body[:500] + "..."
	}
	if body != "" {
		sb.WriteString(fmt.Sprintf("\n```\n%s\n```", body))
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

	log.Printf("[INBOUND] to=%s from=%s subject=%q localPart=%s",
		email.To, email.From, email.Subject, email.LocalPart)

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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":        true,
		"id":        emailID,
		"recipient": email.LocalPart,
		"otps":      otps,
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
	json.NewEncoder(w).Encode(map[string]string{"status": status})
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

	http.HandleFunc("/health", handleHealth)
	http.HandleFunc("/email/inbound", handleInbound)
	http.HandleFunc("/email/query/", handleQuery)

	addr := fmt.Sprintf(":%s", port)
	log.Printf("email-handler listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
