package session

import (
	"encoding/base64"
	"errors"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/crypto"
)

// ExternalClaims is the format for JWT for authentication from external sources
type ExternalClaims struct {
	jwt.RegisteredClaims
	Name  string `json:"name"`
	Code  string `json:"code"`
	Email string `json:"email,omitempty"`
	UUID  string `json:"uuid,omitempty"`
}

// CheckDelegatedJWT checks if a delegated JWT is valid for a given instance
func CheckDelegatedJWT(instance *instance.Instance, token string) error {
	authenticationConfig := config.GetConfig().Authentication
	context := instance.ContextName

	if context == "" {
		context = config.DefaultInstanceContext
	}
	delegatedTypes, ok := authenticationConfig[context]
	if !ok {
		return errors.New("No delegated authentication defined for this context")
	}

	JWTSecret, ok := delegatedTypes.(map[string]interface{})["jwt_secret"]
	if !ok {
		return errors.New("JWT delegated type is not defined for this context")
	}

	claims := ExternalClaims{}
	keyFunc := func(token *jwt.Token) (interface{}, error) {
		return base64.StdEncoding.DecodeString(JWTSecret.(string))
	}

	err := crypto.ParseJWT(token, keyFunc, &claims)
	if err != nil {
		return err
	}

	if claims.RegisteredClaims.ExpiresAt == nil || claims.RegisteredClaims.ExpiresAt.Before(time.Now()) {
		return errors.New("Token has expired")
	}

	if claims.Name != instance.Domain {
		return errors.New("Issuer is not valid")
	}

	return nil
}
