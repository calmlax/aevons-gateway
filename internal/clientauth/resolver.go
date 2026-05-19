package clientauth

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"github.com/calmlax/aevons-gateway/internal/model"
)

type Resolver struct{}

func NewResolver() *Resolver {
	return &Resolver{}
}

func (r *Resolver) Resolve(header http.Header, user *model.UserIdentity) (*model.ClientIdentity, error) {
	if authHeader := strings.TrimSpace(header.Get("Authorization")); authHeader != "" {
		if identity, ok, err := basicIdentity(authHeader); err != nil {
			return nil, err
		} else if ok {
			return identity, nil
		}
	}

	if clientID := strings.TrimSpace(header.Get("X-Client-Id")); clientID != "" {
		return &model.ClientIdentity{ClientID: clientID, Source: "header"}, nil
	}

	if user != nil && strings.TrimSpace(user.ClientID) != "" {
		return &model.ClientIdentity{ClientID: strings.TrimSpace(user.ClientID), Source: "user"}, nil
	}

	if clientID := strings.TrimSpace(header.Get("X-Internal-Client-Id")); clientID != "" {
		return &model.ClientIdentity{ClientID: clientID, Source: "internal_header"}, nil
	}

	return nil, nil
}

func basicIdentity(authHeader string) (*model.ClientIdentity, bool, error) {
	if !strings.HasPrefix(authHeader, "Basic ") {
		return nil, false, nil
	}

	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(strings.TrimPrefix(authHeader, "Basic ")))
	if err != nil {
		return nil, false, errors.New("gateway.client_invalid")
	}

	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
		return nil, false, errors.New("gateway.client_invalid")
	}

	return &model.ClientIdentity{
		ClientID: strings.TrimSpace(parts[0]),
		Source:   "basic_auth",
	}, true, nil
}
