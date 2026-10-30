// Package session validates the application's existing cookie JWTs. Cloud Run
// workload identity tokens require a separate audience-aware verifier.
package session

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

var ErrInvalidUserID = errors.New("invalid user ID")

type Claims struct {
	UserID      int
	Communities []string
}

func Validate(encoded, secret string) (Claims, error) {
	if strings.TrimSpace(secret) == "" {
		return Claims{}, errors.New("session signing key is not configured")
	}
	token, err := jwt.Parse(encoded, func(*jwt.Token) (any, error) {
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithExpirationRequired(), jwt.WithJSONNumber())
	if err != nil {
		return Claims{}, err
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return Claims{}, errors.New("invalid session token")
	}
	id, err := userID(claims["user_id"])
	if err != nil {
		return Claims{}, err
	}
	result := Claims{UserID: id}
	if raw, ok := claims["communities"].([]any); ok {
		for _, item := range raw {
			if community, ok := item.(string); ok && community != "" {
				result.Communities = append(result.Communities, community)
			}
		}
	}
	return result, nil
}

func userID(value any) (int, error) {
	var text string
	switch typed := value.(type) {
	case json.Number:
		text = typed.String()
	case string:
		text = typed
	default:
		return 0, ErrInvalidUserID
	}
	if text == "" {
		return 0, ErrInvalidUserID
	}
	for _, char := range text {
		if char < '0' || char > '9' {
			return 0, ErrInvalidUserID
		}
	}
	parsed, err := strconv.ParseUint(text, 10, strconv.IntSize-1)
	// Existing handlers consume float64 IDs; reject values it cannot represent
	// safely before storing them in the Gin context or converting them to int.
	if err != nil || parsed == 0 || parsed > 1<<53-1 {
		return 0, ErrInvalidUserID
	}
	return int(parsed), nil
}
