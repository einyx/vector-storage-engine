package proxy

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prysmsh/warp-storage-engine/internal/config"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

func TestVerifyJWTSignature_JWKS_EndToEnd(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	kid := "integration-kid"
	jwk := jose.JSONWebKey{
		Key:       priv.Public(),
		Algorithm: "RS256",
		KeyID:     kid,
		Use:       "sig",
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/jwks.json" {
			http.NotFound(w, r)
			return
		}
		set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwk}}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(set); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: priv}, (&jose.SignerOptions{}).WithHeader(jose.HeaderKey("kid"), kid))
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	cl := jwt.Claims{
		Issuer:   "https://example.com/",
		Subject:  "user-1",
		Audience: jwt.Audience{"my-client"},
		Expiry:   jwt.NewNumericDate(now.Add(time.Hour)),
		IssuedAt: jwt.NewNumericDate(now),
	}
	raw, err := jwt.Signed(signer).Claims(cl).Serialize()
	if err != nil {
		t.Fatal(err)
	}

	handler := NewAuth0Handler(&config.Auth0Config{
		JWTValidation:  true,
		Domain:         "example.com",
		ClientID:       "my-client",
		JWKSURL:        ts.URL + "/.well-known/jwks.json",
		SessionTimeout: time.Hour,
	})

	if err := handler.verifyJWTSignature(raw); err != nil {
		t.Fatalf("verifyJWTSignature: %v", err)
	}

	privOther, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signerOther, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: privOther}, (&jose.SignerOptions{}).WithHeader(jose.HeaderKey("kid"), kid))
	if err != nil {
		t.Fatal(err)
	}
	rawWrong, err := jwt.Signed(signerOther).Claims(cl).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.verifyJWTSignature(rawWrong); err == nil {
		t.Fatal("expected verification failure for mismatched signing key")
	}
}
