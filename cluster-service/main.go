package main

import (
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

	// Store email record (TODO: persist to DB)
	record := map[string]interface{}{
		"to":         email.To,
		"from":       email.From,
		"subject":    email.Subject,
		"localPart":  email.LocalPart,
		"receivedAt": email.ReceivedAt,
		"processedAt": time.Now().UTC().Format(time.RFC3339),
		"otps":       otps,
	}

	// TODO: Route to the correct claw via Discord webhook or agent API
	// For now, log and respond
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
	// Return most recent OTPs

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

	http.HandleFunc("/health", handleHealth)
	http.HandleFunc("/email/inbound", handleInbound)
	http.HandleFunc("/email/query/", handleQuery)

	addr := fmt.Sprintf(":%s", port)
	log.Printf("email-handler listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
