package drive

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"

	"golang.org/x/oauth2"
)

// Scope is the Drive access scope the bot requests, as in the upstream bot.
const Scope = "https://www.googleapis.com/auth/drive"

// Authenticator completes the user OAuth flow once, saves the token, and
// reuses the saved token on later runs.
type Authenticator struct {
	cfg        *oauth2.Config
	tokenPath  string
	codeReader io.Reader
	prompt     io.Writer
}

// NewAuthenticator builds an authenticator from OAuth client credentials.
// codeReader supplies the authorization code for the first run.
// prompt receives the authorization URL instructions for the first run.
// Both may be nil when a saved token exists.
func NewAuthenticator(cfg *oauth2.Config, tokenPath string, codeReader io.Reader, prompt io.Writer) *Authenticator {
	return &Authenticator{
		cfg:        cfg,
		tokenPath:  tokenPath,
		codeReader: codeReader,
		prompt:     prompt,
	}
}

// NewAuthenticatorFromSecret builds an authenticator from a Google OAuth
// client secret file, as downloaded from the Google Cloud console.
func NewAuthenticatorFromSecret(secretPath, tokenPath string, codeReader io.Reader, prompt io.Writer) (*Authenticator, error) {
	creds, err := readClientSecret(secretPath)
	if err != nil {
		return nil, err
	}
	cfg := &oauth2.Config{
		ClientID:     creds.clientID,
		ClientSecret: creds.clientSecret,
		Endpoint: oauth2.Endpoint{
			AuthURL:  creds.authURL,
			TokenURL: creds.tokenURL,
		},
		RedirectURL: creds.redirectURL,
		Scopes:      []string{Scope},
	}
	return NewAuthenticator(cfg, tokenPath, codeReader, prompt), nil
}

// Client returns an HTTP client authorized for the Drive scope.
// It reuses the saved token when one exists.
func (a *Authenticator) Client(ctx context.Context) (*http.Client, error) {
	token, err := a.loadToken()
	if err != nil {
		return nil, err
	}
	if token == nil {
		token, err = a.authorize(ctx)
		if err != nil {
			return nil, err
		}
	}
	return oauth2.NewClient(ctx, a.cfg.TokenSource(ctx, token)), nil
}

// loadToken returns nil when no token file exists yet.
func (a *Authenticator) loadToken() (*oauth2.Token, error) {
	data, err := os.ReadFile(a.tokenPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read saved OAuth token %s: %w", a.tokenPath, err)
	}
	var token oauth2.Token
	if err := json.Unmarshal(data, &token); err != nil {
		return nil, fmt.Errorf("parse saved OAuth token %s: %w", a.tokenPath, err)
	}
	return &token, nil
}

// authorize runs the terminal authorization flow and saves the token.
func (a *Authenticator) authorize(ctx context.Context) (*oauth2.Token, error) {
	if a.codeReader == nil {
		return nil, fmt.Errorf("no saved OAuth token in %s and no terminal input available to authorize one", a.tokenPath)
	}
	prompt := a.prompt
	if prompt == nil {
		prompt = io.Discard
	}
	authURL := a.cfg.AuthCodeURL("telemirror", oauth2.AccessTypeOffline)
	fmt.Fprintf(prompt, "Authorize this app by visiting this url:\n\n%s\n\nThen enter the code from that page here: ", authURL)

	code, err := bufio.NewReader(a.codeReader).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read authorization code: %w", err)
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, errors.New("read authorization code: the code is empty")
	}

	token, err := a.cfg.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("exchange authorization code: %w", err)
	}
	if err := saveToken(a.tokenPath, token); err != nil {
		return nil, err
	}
	return token, nil
}

// saveToken stores the token for later runs. It keeps the file private.
func saveToken(path string, token *oauth2.Token) error {
	data, err := json.Marshal(token)
	if err != nil {
		return fmt.Errorf("encode OAuth token: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("save OAuth token %s: %w", path, err)
	}
	return nil
}

// clientSecret holds the parsed OAuth client credentials.
type clientSecret struct {
	clientID     string
	clientSecret string
	authURL      string
	tokenURL     string
	redirectURL  string
}

// googleClientSecret mirrors the client secret file format.
type googleClientSecret struct {
	Installed *googleOAuthClient `json:"installed"`
	Web       *googleOAuthClient `json:"web"`
}

type googleOAuthClient struct {
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret"`
	AuthURI      string   `json:"auth_uri"`
	TokenURI     string   `json:"token_uri"`
	RedirectURIs []string `json:"redirect_uris"`
}

// readClientSecret reads and validates a Google client secret file.
func readClientSecret(path string) (clientSecret, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return clientSecret{}, fmt.Errorf("read OAuth client secret %s: %w", path, err)
	}
	var parsed googleClientSecret
	if err := json.Unmarshal(data, &parsed); err != nil {
		return clientSecret{}, fmt.Errorf("parse OAuth client secret %s: %w", path, err)
	}
	client := parsed.Installed
	if client == nil {
		client = parsed.Web
	}
	if client == nil || client.ClientID == "" || client.ClientSecret == "" {
		return clientSecret{}, fmt.Errorf("OAuth client secret %s has no usable installed or web client", path)
	}
	secret := clientSecret{
		clientID:     client.ClientID,
		clientSecret: client.ClientSecret,
		authURL:      client.AuthURI,
		tokenURL:     client.TokenURI,
		redirectURL:  "http://localhost",
	}
	if len(client.RedirectURIs) > 0 {
		secret.redirectURL = client.RedirectURIs[0]
	}
	return secret, nil
}
