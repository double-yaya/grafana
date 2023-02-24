package clients

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gopkg.in/square/go-jose.v2"
	"gopkg.in/square/go-jose.v2/jwt"

	"github.com/grafana/grafana/pkg/extensions/oauthserver"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/services/authn"
	"github.com/grafana/grafana/pkg/services/user"
	"github.com/grafana/grafana/pkg/setting"
	"github.com/grafana/grafana/pkg/util/errutil"
)

var _ authn.Client = new(ExtendedJWT)

var (
	ErrInvalidToken = errutil.NewBase(errutil.StatusUnauthorized,
		"invalid_token", errutil.WithPublicMessage("Failed to verify JWT"))

	publicKeyRaw, err = pem.Decode([]byte(`-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAvDNW/jqNoL6cJ7m1T/qM
fNxouV9kItOWlA8NKm9vDickN8Dz+jMqog9/BJH5k2S5+AzB9aTo52Sm6XqiBvK3
lrHA3aH2z9Zn0UVpccKxlsRfqaE1HYRFhRB80+gzZpeSHQmSYPLqOzhSB+Ytqz1Z
mkW/DqjTwKrBSjP+RrFUZoDGU+/1FD92s0lMZbAlT+SDvawC5zuxWk7N9BuCZQ35
FYKs7YM8wQv/mcq3kmeH47CGF7OQyH1sPfA+2GN4s+8UtK24rPd+ecS0pOD/pP5m
W9J8Hl7JHR1e/5apPTEKovsKkgj4IMr8+2CXMkMTS1s1yY0enWdkzv4kiiHnJIHn
XwIDAQAB
-----END PUBLIC KEY-----`))
	timeNow      = time.Now
	parsedKey, _ = x509.ParsePKIXPublicKey(publicKeyRaw.Bytes)
	publicKey    = parsedKey.(*rsa.PublicKey)
)

const (
	SigningMethodNone = jose.SignatureAlgorithm("none")
	ExpectedIssuer    = "http://localhost:3000"              // move to config
	ExpectedAudiance  = "http://localhost:3000/oauth2/token" // move to config
)

func ProvideExtendedJWT(userService user.Service, cfg *setting.Cfg, oauthService oauthserver.OAuth2Service) *ExtendedJWT {
	return &ExtendedJWT{
		cfg:          cfg,
		log:          log.New(authn.ClientJWT),
		userService:  userService,
		oauthService: oauthService,
	}
}

type ExtendedJWT struct {
	cfg          *setting.Cfg
	log          log.Logger
	userService  user.Service
	oauthService oauthserver.OAuth2Service
}

func (s *ExtendedJWT) Authenticate(ctx context.Context, r *authn.Request) (*authn.Identity, error) {
	jwtToken := s.retrieveToken(r.HTTPRequest)

	claims, err := s.VerifyRFC9068Token(ctx, jwtToken)
	if err != nil {
		s.log.Debug("Failed to verify JWT", "error", err)
		return nil, ErrInvalidToken.Errorf("failed to verify JWT: %w", err)
	}

	// user:18
	userID, err := strconv.ParseInt(strings.TrimPrefix(claims["sub"].(string), fmt.Sprintf("%s:", authn.NamespaceUser)), 10, 64)
	if err != nil {
		return nil, ErrJWTInvalid.Errorf("failed to parse sub: %w", err)
	}

	signedInUser, err := s.userService.GetSignedInUserWithCacheCtx(ctx, &user.GetSignedInUserQuery{OrgID: r.OrgID, UserID: userID})
	if err != nil {
		return nil, err
	}

	targetOrgID, err := s.parseOrgIDFromScopes(claims["scp"])
	if err != nil {
		return nil, err
	}

	if signedInUser.Permissions == nil {
		signedInUser.Permissions = make(map[int64]map[string][]string)
	}

	signedInUser.Permissions[targetOrgID] = s.parseEntitlements(claims["entitlements"].(map[string]interface{}))

	return authn.IdentityFromSignedInUser(authn.NamespacedID(authn.NamespaceUser, signedInUser.UserID), signedInUser, authn.ClientParams{SyncPermissionsFromDB: false}), nil
}

func (s *ExtendedJWT) parseOrgIDFromScopes(scopes interface{}) (int64, error) {
	for _, scope := range scopes.([]interface{}) {
		if strings.HasPrefix(scope.(string), "org.") {
			return strconv.ParseInt(strings.TrimPrefix(scope.(string), "org."), 10, 64)
		}
	}

	return 0, fmt.Errorf("no org scope found")
}

func (s *ExtendedJWT) parseEntitlements(entitlements map[string]interface{}) map[string][]string {
	result := map[string][]string{}
	for key, value := range entitlements {
		if value == nil {
			result[key] = []string{}
		} else {
			result[key] = s.parseEntitlementsArray(value)
		}
	}
	return result
}

func (s *ExtendedJWT) parseEntitlementsArray(entitlements interface{}) []string {
	result := []string{}
	for _, entitlement := range entitlements.([]interface{}) {
		result = append(result, entitlement.(string))
	}
	return result
}

// retrieveToken retrieves the JWT token from the request.
func (s *ExtendedJWT) retrieveToken(httpRequest *http.Request) string {
	jwtToken := httpRequest.Header.Get("Authorization")

	// Strip the 'Bearer' prefix if it exists.
	return strings.TrimPrefix(jwtToken, "Bearer ")
}

func (s *ExtendedJWT) Test(ctx context.Context, r *authn.Request) bool {
	// TODO: Create a config for the Extended JWT middleware.
	// if !s.cfg.JWTAuthEnabled || s.cfg.JWTAuthHeaderName == "" {
	// 	return false
	// }

	rawToken := s.retrieveToken(r.HTTPRequest)
	if rawToken == "" {
		return false
	}

	parsedToken, err := jwt.ParseSigned(rawToken)
	if err != nil {
		return false
	}

	var claims jwt.Claims
	if err := parsedToken.UnsafeClaimsWithoutVerification(&claims); err != nil {
		return false
	}

	return claims.Issuer == ExpectedIssuer
}

// VerifyRFC9068Token verifies the token against the RFC 9068 specification.
func (s *ExtendedJWT) VerifyRFC9068Token(ctx context.Context, rawToken string) (map[string]interface{}, error) {
	parsedToken, err := jwt.ParseSigned(rawToken)
	if err != nil {
		return nil, fmt.Errorf("failed to parse JWT: %w", err)
	}

	if len(parsedToken.Headers) != 1 {
		return nil, fmt.Errorf("only one header supported, got %d", len(parsedToken.Headers))
	}

	parsedHeader := parsedToken.Headers[0]

	typeHeader := parsedHeader.ExtraHeaders["typ"]
	if typeHeader == nil {
		return nil, fmt.Errorf("missing 'typ' field from the header")
	}

	jwtType := strings.ToLower(typeHeader.(string))
	if jwtType != "at+jwt" && jwtType != "application/at+jwt" {
		return nil, fmt.Errorf("invalid JWT type: %s", jwtType)
	}

	if parsedHeader.Algorithm == string(SigningMethodNone) {
		return nil, fmt.Errorf("invalid algorithm: %s", parsedHeader.Algorithm)
	}

	var claims jwt.Claims
	var allClaims map[string]interface{}
	err = parsedToken.Claims(publicKey, &claims, &allClaims)
	if err != nil {
		return nil, fmt.Errorf("failed to verify JWT: %w", err)
	}

	err = claims.ValidateWithLeeway(jwt.Expected{
		Issuer:   ExpectedIssuer,
		Audience: jwt.Audience{ExpectedAudiance},
		Time:     timeNow(),
	}, 0)

	if err != nil {
		return nil, fmt.Errorf("failed to validate JWT: %w", err)
	}

	if err := s.validateClientIdClaim(ctx, allClaims); err != nil {
		return nil, err
	}

	return allClaims, nil
}

func (s *ExtendedJWT) validateClientIdClaim(ctx context.Context, claims map[string]interface{}) error {
	clientIdClaim, ok := claims["client_id"]
	if !ok {
		return fmt.Errorf("missing 'client_id' claim")
	}

	clientId, ok := clientIdClaim.(string)
	if !ok {
		return fmt.Errorf("invalid 'client_id' claim: %s", clientIdClaim)
	}

	if _, err := s.oauthService.GetClient(ctx, clientId); err != nil {
		return fmt.Errorf("invalid 'client_id' claim: %s", clientIdClaim)
	}

	return nil
}
