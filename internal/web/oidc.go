package web

import "net/http"

// OIDCProvider is one configured OpenID Connect provider.
type OIDCProvider struct {
	// Name is the provider's name in URLs and the config: lowercase
	// letters, digits and "-".
	Name string
	// Label is what the login button says after "Log in with".
	Label string
}

func (s *Server) oidcRoutes(mux *http.ServeMux) {}
