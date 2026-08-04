package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// tokenRefreshBuffer is how long before real expiry we proactively refresh.
const tokenRefreshBuffer = 5 * time.Minute

// Token mirrors the on-disk kiro-auth-token.json written by the Kiro desktop
// app. It is read only when importing local credentials into the account store
// (see importLocalCredentials); the running service is served entirely from the
// multi-account store.
type Token struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    string `json:"expiresAt"`
	ClientIDHash string `json:"clientIdHash"`
	AuthMethod   string `json:"authMethod"`
	Provider     string `json:"provider"`
	Region       string `json:"region"`
	ProfileArn   string `json:"profileArn"` // present on some social tokens
}

// expiry parses the ExpiresAt field into a time. Zero time means unknown.
func (t Token) expiry() time.Time {
	if t.ExpiresAt == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02T15:04:05Z",
	} {
		if ts, err := time.Parse(layout, t.ExpiresAt); err == nil {
			return ts
		}
	}
	return time.Time{}
}

// clientRegistration mirrors the <clientIdHash>.json cache file.
type clientRegistration struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

// loadToken reads the token file into its typed representation.
func loadToken(path string) (Token, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Token{}, err
	}
	var tok Token
	if err := json.Unmarshal(data, &tok); err != nil {
		return Token{}, fmt.Errorf("parse token json: %w", err)
	}
	return tok, nil
}
