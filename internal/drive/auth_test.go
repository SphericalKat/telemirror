package drive_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/SphericalKat/telemirror/internal/drive"
)

const clientSecretJSON = `{
  "installed": {
    "client_id": "test-client-id.apps.googleusercontent.com",
    "client_secret": "test-client-secret",
    "auth_uri": "https://accounts.google.com/o/oauth2/auth",
    "token_uri": "https://oauth2.googleapis.com/token",
    "redirect_uris": ["http://localhost"]
  }
}`

// protectedServer accepts one request and records its Authorization header.
func protectedServer(t *testing.T) (*httptest.Server, *string) {
	t.Helper()
	var authHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &authHeader
}

// tokenEndpoint answers one OAuth token exchange with a fixed token.
func tokenEndpoint(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("grant_type") != "authorization_code" {
			t.Errorf("grant_type = %q, want authorization_code", r.FormValue("grant_type"))
		}
		if r.FormValue("code") != "test-code" {
			t.Errorf("code = %q, want test-code", r.FormValue("code"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func oauthConfig(endpoint oauth2.Endpoint) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
		Endpoint:     endpoint,
		RedirectURL:  "http://localhost",
		Scopes:       []string{drive.Scope},
	}
}

func TestClientReusesSavedTokenWithoutPrompting(t *testing.T) {
	protected, authHeader := protectedServer(t)
	tokenPath := filepath.Join(t.TempDir(), "token.json")
	saved, err := json.Marshal(&oauth2.Token{
		AccessToken:  "saved-access",
		RefreshToken: "saved-refresh",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("marshal token: %v", err)
	}
	if err := os.WriteFile(tokenPath, saved, 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	var prompt strings.Builder
	auth := drive.NewAuthenticator(oauthConfig(oauth2.Endpoint{}), tokenPath, strings.NewReader(""), &prompt)

	client, err := auth.Client(context.Background())
	if err != nil {
		t.Fatalf("Client() error = %v", err)
	}
	if _, err := client.Get(protected.URL); err != nil {
		t.Fatalf("request with saved token: %v", err)
	}

	if *authHeader != "Bearer saved-access" {
		t.Errorf("Authorization = %q, want Bearer saved-access", *authHeader)
	}
	if prompt.Len() != 0 {
		t.Errorf("prompt output = %q, want none when reusing a saved token", prompt.String())
	}
}

func TestClientCompletesInteractiveFlowAndSavesReusableToken(t *testing.T) {
	tokens := tokenEndpoint(t)
	protected, authHeader := protectedServer(t)
	tokenPath := filepath.Join(t.TempDir(), "token.json")
	cfg := oauthConfig(oauth2.Endpoint{
		AuthURL:  tokens.URL + "/auth",
		TokenURL: tokens.URL + "/token",
	})

	var prompt strings.Builder
	auth := drive.NewAuthenticator(cfg, tokenPath, strings.NewReader("test-code\n"), &prompt)

	client, err := auth.Client(context.Background())
	if err != nil {
		t.Fatalf("Client() error = %v", err)
	}
	if !strings.Contains(prompt.String(), "Authorize this app") || !strings.Contains(prompt.String(), tokens.URL+"/auth") {
		t.Errorf("prompt output = %q, want the authorization URL and instructions", prompt.String())
	}
	if _, err := client.Get(protected.URL); err != nil {
		t.Fatalf("request with new token: %v", err)
	}
	if *authHeader != "Bearer new-access" {
		t.Errorf("Authorization = %q, want Bearer new-access", *authHeader)
	}

	// A later run must reuse the saved token without prompting again.
	var secondPrompt strings.Builder
	again := drive.NewAuthenticator(cfg, tokenPath, strings.NewReader(""), &secondPrompt)
	secondClient, err := again.Client(context.Background())
	if err != nil {
		t.Fatalf("second Client() error = %v", err)
	}
	if _, err := secondClient.Get(protected.URL); err != nil {
		t.Fatalf("request with reused token: %v", err)
	}
	if *authHeader != "Bearer new-access" {
		t.Errorf("Authorization = %q, want Bearer new-access on reuse", *authHeader)
	}
	if secondPrompt.Len() != 0 {
		t.Errorf("second prompt output = %q, want none", secondPrompt.String())
	}
}

func TestNewAuthenticatorFromSecretLoadsClientCredentials(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "client_secret.json")
	if err := os.WriteFile(secretPath, []byte(clientSecretJSON), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	// A valid saved token lets Client run without a prompt, which shows the
	// client credentials were parsed and are usable.
	tokenPath := filepath.Join(dir, "token.json")
	saved, err := json.Marshal(&oauth2.Token{
		AccessToken:  "saved-access",
		RefreshToken: "saved-refresh",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("marshal token: %v", err)
	}
	if err := os.WriteFile(tokenPath, saved, 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	auth, err := drive.NewAuthenticatorFromSecret(secretPath, tokenPath, nil, nil)
	if err != nil {
		t.Fatalf("NewAuthenticatorFromSecret() error = %v", err)
	}
	if _, err := auth.Client(context.Background()); err != nil {
		t.Fatalf("Client() error = %v", err)
	}
}

func TestNewAuthenticatorFromSecretMissingFileFailsClearly(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "absent_secret.json")
	_, err := drive.NewAuthenticatorFromSecret(secretPath, "token.json", nil, nil)
	if err == nil {
		t.Fatal("NewAuthenticatorFromSecret() error = nil, want failure")
	}
	if !strings.Contains(err.Error(), secretPath) {
		t.Errorf("error = %v, want it to name %s", err, secretPath)
	}
}

func TestClientRejectsCorruptSavedToken(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token.json")
	if err := os.WriteFile(tokenPath, []byte("not json"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	auth := drive.NewAuthenticator(oauthConfig(oauth2.Endpoint{}), tokenPath, nil, nil)
	_, err := auth.Client(context.Background())
	if err == nil {
		t.Fatal("Client() error = nil, want failure for a corrupt token")
	}
	if !strings.Contains(err.Error(), tokenPath) {
		t.Errorf("error = %v, want it to name %s", err, tokenPath)
	}
}
