package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"go.uber.org/fx/fxtest"
	"junglegaming/internal/platform/config"
)

func TestJWTClaimsAndSignature(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "key-1", Algorithm: "RS256", Use: "sig"}}})
	}))
	defer keys.Close()
	cfg := config.Defaults()
	cfg.OIDCIssuerURL = "https://issuer.example/realms/wagercore"
	cfg.OIDCJWKSURL = keys.URL
	lifecycle := fxtest.NewLifecycle(t)
	validator := NewValidator(lifecycle, cfg)
	lifecycle.RequireStart()
	defer lifecycle.RequireStop()
	valid := func() map[string]any {
		return map[string]any{"iss": cfg.OIDCIssuerURL, "aud": cfg.OIDCAudience, "sub": "service-account", "exp": time.Now().Add(time.Minute).Unix(), "nbf": time.Now().Add(-time.Minute).Unix(), "azp": "provider-a", "provider_id": "provider-a", "typ": "Bearer", "realm_access": map[string]any{"roles": []string{"provider"}}}
	}
	sign := func(claims map[string]any, private *rsa.PrivateKey) string {
		t.Helper()
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: private}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "key-1"))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := jwt.Signed(signer).Claims(claims).Serialize()
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	principal, err := validator.Verify(context.Background(), sign(valid(), key))
	if err != nil || principal.ProviderID != "provider-a" || principal.Internal {
		t.Fatalf("principal=%+v error=%v", principal, err)
	}
	for _, tt := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"wrong issuer", func(c map[string]any) { c["iss"] = "https://other.example" }},
		{"wrong audience", func(c map[string]any) { c["aud"] = "other-api" }},
		{"expired", func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }},
		{"not yet valid", func(c map[string]any) { c["nbf"] = time.Now().Add(time.Hour).Unix() }},
		{"missing subject", func(c map[string]any) { delete(c, "sub") }},
		{"ID token type", func(c map[string]any) { c["typ"] = "ID" }},
		{"provider spoofing", func(c map[string]any) { c["provider_id"] = "provider-b" }},
		{"missing role", func(c map[string]any) { delete(c, "realm_access") }},
		{"mixed roles", func(c map[string]any) {
			c["realm_access"] = map[string]any{"roles": []string{"provider", "wallet-service"}}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			claims := valid()
			tt.edit(claims)
			if _, err := validator.Verify(context.Background(), sign(claims, key)); err == nil {
				t.Fatal("invalid claims accepted")
			}
		})
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validator.Verify(context.Background(), sign(valid(), other)); err == nil {
		t.Fatal("forged signature accepted")
	}
}
