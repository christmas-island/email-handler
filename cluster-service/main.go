package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

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
	Type   string `json:"type"` // "otp", "verification_link", "magic_link"
	Source string `json:"source"`
}

// Common OTP patterns
var otpPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:code|otp|pin|token)[:\s]+(\d{4,8})`),
	regexp.MustCompile(`(?i)(?:verification|confirm)[:\s]+(\d{4,8})`),
	regexp.MustCompile(`(?i)(\d{6})\s+is your (?:verification|confirmation|login) code`),
	regexp.MustCompile(`(?i)your (?:code|otp|pin) is[:\s]+(\d{4,8})`),
	regexp.MustCompile(`(?i)enter (?:the )?(?:code|otp)[:\s]+(\d{4,8})`),
}

// Verification link patterns
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
				matches = append(matches, OTPMatch{
					Code:   match[1],
					Type:   "otp",
					Source: match[0],
				})
			}
		}
	}

	for _, pattern := range linkPatterns {
		for _, match := range pattern.FindAllStringSubmatch(body, -1) {
			if len(match) > 0 && !seen[match[0]] {
				seen[match[0]] = true
				matches = append(matches, OTPMatch{
					Code:   match[0],
					Type:   "verification_link",
					Source: match[0],
				})
			}
		}
	}

	return matches
}

// postToDiscord sends an email notification to the Discord #email channel via webhook
func postToDiscord(email InboundEmail, otps []OTPMatch) {
	webhookURL := os.Getenv("DISCORD_WEBHOOK_URL")
	if webhookURL == "" {
		log.Println("[DISCORD] No webhook URL configured, skipping notification")
		return
	}

	// Build the message
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📧 **New email for `%s`**\n", email.To))
	sb.WriteString(fmt.Sprintf("**From:** %s\n", email.From))
	sb.WriteString(fmt.Sprintf("**Subject:** %s\n", email.Subject))
	sb.WriteString(fmt.Sprintf("**Received:** %s\n", email.ReceivedAt))

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

	// Extract a preview from the raw email (skip headers, first 500 chars of body)
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

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[DISCORD] Failed to marshal payload: %v", err)
		return
	}

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

// extractBody tries to get the text body from a raw email, skipping headers
func extractBody(raw string) string {
	// Find the blank line separating headers from body
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

	// Auth check
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

	// Extract OTP codes
	otps := extractOTPs(email.Raw)
	if len(otps) > 0 {
		log.Printf("[OTP] Found %d codes/links for %s:", len(otps), email.LocalPart)
		for _, otp := range otps {
			log.Printf("  - [%s] %s", otp.Type, otp.Code)
		}
	}

	// Post to Discord #email channel
	go postToDiscord(email, otps)

	// Store email record (TODO: persist to DB)
	record := map[string]interface{}{
		"to":          email.To,
		"from":        email.From,
		"subject":     email.Subject,
		"localPart":   email.LocalPart,
		"receivedAt":  email.ReceivedAt,
		"processedAt": time.Now().UTC().Format(time.RFC3339),
		"otps":        otps,
	}
	_ = record

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":        true,
		"recipient": email.LocalPart,
		"otps":      otps,
	})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleQuery lets claws check their inbox for recent OTPs
func handleQuery(w http.ResponseWriter, r *http.Request) {
	localPart := strings.TrimPrefix(r.URL.Path, "/email/query/")
	if localPart == "" {
		http.Error(w, "missing localPart", http.StatusBadRequest)
		return
	}

	// TODO: query stored emails for this localPart
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":        true,
		"recipient": localPart,
		"message":   "not yet implemented — needs DB backend",
	})
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	webhookURL := os.Getenv("DISCORD_WEBHOOK_URL")
	if webhookURL != "" {
		log.Printf("Discord webhook configured ✓")
	} else {
		log.Printf("No DISCORD_WEBHOOK_URL set — Discord notifications disabled")
	}

	http.HandleFunc("/health", handleHealth)
	http.HandleFunc("/email/inbound", handleInbound)
	http.HandleFunc("/email/query/", handleQuery)

	addr := fmt.Sprintf(":%s", port)
	log.Printf("email-handler listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
