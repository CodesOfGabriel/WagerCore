package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"go.uber.org/fx"

	"junglegaming/internal/platform/config"
)

var ErrUnauthorized = errors.New("invalid access token")

type Principal struct {
	ProviderID string
	Internal   bool
}
type Validator struct{ verifier *oidc.IDTokenVerifier }

func NewValidator(lifecycle fx.Lifecycle, cfg config.Config) *Validator {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	ctx := oidc.ClientContext(context.Background(), client)
	keys := oidc.NewRemoteKeySet(ctx, cfg.OIDCJWKSURL)
	validator := &Validator{verifier: oidc.NewVerifier(cfg.OIDCIssuerURL, keys, &oidc.Config{ClientID: cfg.OIDCAudience, SupportedSigningAlgs: []string{oidc.RS256}})}
	lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.OIDCJWKSURL, nil)
			if err != nil {
				return err
			}
			response, err := client.Do(request)
			if err != nil {
				transport.CloseIdleConnections()
				return errors.New("OIDC signing keys are unavailable")
			}
			defer response.Body.Close()
			var jwks struct {
				Keys []json.RawMessage `json:"keys"`
			}
			if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&jwks) != nil || len(jwks.Keys) == 0 {
				transport.CloseIdleConnections()
				return errors.New("invalid OIDC signing keys response")
			}
			return nil
		},
		OnStop: func(context.Context) error { transport.CloseIdleConnections(); return nil },
	})
	return validator
}

func (v *Validator) Verify(ctx context.Context, raw string) (Principal, error) {
	token, err := v.verifier.Verify(ctx, raw)
	if err != nil {
		return Principal{}, ErrUnauthorized
	}
	var claims struct {
		AuthorizedParty string `json:"azp"`
		ProviderID      string `json:"provider_id"`
		Type            string `json:"typ"`
		NotBefore       int64  `json:"nbf"`
		RealmAccess     struct {
			Roles []string `json:"roles"`
		} `json:"realm_access"`
	}
	if token.Subject == "" || token.Claims(&claims) != nil || claims.AuthorizedParty == "" || claims.Type != "Bearer" || claims.NotBefore > time.Now().Unix() {
		return Principal{}, ErrUnauthorized
	}
	internal, provider := false, false
	for _, role := range claims.RealmAccess.Roles {
		internal = internal || role == "wallet-service"
		provider = provider || role == "provider"
	}
	if internal && !provider && claims.AuthorizedParty == "internal-service" && claims.ProviderID == "" {
		return Principal{Internal: true}, nil
	}
	if provider && !internal && claims.ProviderID != "" && claims.ProviderID == claims.AuthorizedParty {
		return Principal{ProviderID: claims.ProviderID}, nil
	}
	return Principal{}, ErrUnauthorized
}
