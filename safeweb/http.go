// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package safeweb provides a wrapper around an http.Server that applies
// basic web application security defenses by default. The wrapper can be
// used in place of an http.Server. A safeweb.Server adds mitigations for
// Cross-Site Request Forgery (CSRF) attacks, and annotates requests with
// appropriate Cross-Origin Resource Sharing (CORS), Content-Security-Policy,
// X-Content-Type-Options, and Referer-Policy headers.
//
// To use safeweb, the application must separate its "browser" routes from "API"
// routes, with each on its own http.ServeMux. When serving requests, the
// server will first check the browser mux, and if no matching route is found it
// will defer to the API mux.
//
// # Browser Routes
//
// All routes in the browser mux enforce CSRF protection using the gorilla/csrf
// package. The application must template the CSRF token into its forms using
// the [TemplateField] and [TemplateTag] APIs. Applications that are served in a
// secure context (over HTTPS) should also set the SecureContext field to true
// to ensure that the the CSRF cookies are marked as Secure.
//
// In addition, browser routes will also have the following applied:
//   - Content-Security-Policy header that disallows inline scripts, framing, and third party resources.
//   - X-Content-Type-Options header on responses set to "nosniff" to prevent MIME type sniffing attacks.
//   - Referer-Policy header set to "same-origin" to prevent leaking referrer information to third parties.
//
// By default the Content-Security-Policy header will disallow inline styles.
// This can be overridden by setting the CSPAllowInlineStyles field to true in
// the safeweb.Config struct.
//
// # API routes
//
// safeweb inspects the Content-Type header of incoming requests to the API mux
// and prohibits the use of `application/x-www-form-urlencoded` values.  If the
// application provides a list of allowed origins and methods in its
// configuration safeweb will set the appropriate CORS headers on pre-flight
// OPTIONS requests served by the API mux.
//
// # HTTP Redirects
//
// The [RedirectHTTP] method returns a handler that redirects all incoming HTTP
// requests to HTTPS at the same path on the provided fully qualified domain
// name (FQDN).
//
// # Example usage
//
//	h := http.NewServeMux()
//	h.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
//		fmt.Fprint(w, "Hello, world!")
//	})
//	s, err := safeweb.NewServer(safeweb.Config{
//		BrowserMux: h,
//	})
//	if err != nil {
//		log.Fatalf("failed to create server: %v", err)
//	}
//	ln, err := net.Listen("tcp", ":8080")
//	if err != nil {
//		log.Fatalf("failed to listen: %v", err)
//	}
//	defer ln.Close()
//	if err := s.Serve(ln); err != nil && err != http.ErrServerClosed {
//		log.Fatalf("failed to serve: %v", err)
//	}
//
// [TemplateField]: https://pkg.go.dev/github.com/gorilla/csrf#TemplateField
// [TemplateTag]: https://pkg.go.dev/github.com/gorilla/csrf#TemplateTag
package safeweb

import (
	"cmp"
	crand "crypto/rand"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/gorilla/csrf"
)

// The default Content-Security-Policy header.
var defaultCSP = strings.Join([]string{
	`default-src 'self'`,      // origin is the only valid source for all content types
	`script-src 'self'`,       // disallow inline javascript
	`frame-ancestors 'none'`,  // disallow framing of the page
	`form-action 'self'`,      // disallow form submissions to other origins
	`base-uri 'self'`,         // disallow base URIs from other origins
	`block-all-mixed-content`, // disallow mixed content when serving over HTTPS
	`object-src 'self'`,       // disallow embedding of resources from other origins
}, "; ")

// Config contains the configuration for a safeweb server.
type Config struct {
	// SecureContext specifies whether the Server is running in a secure (HTTPS) context.
	// Setting this to true will cause the Server to set the Secure flag on CSRF cookies.
	SecureContext bool

	// BrowserMux is the HTTP handler for any routes in your application that
	// should only be served to browsers in a primary origin context. These
	// requests will be subject to CSRF protection and will have
	// browser-specific headers in their responses.
	BrowserMux *http.ServeMux

	// APIMux is the HTTP handler for any routes in your application that
	// should only be served to non-browser clients or to browsers in a
	// cross-origin resource sharing context.
	APIMux *http.ServeMux

	// AccessControlAllowOrigin specifies the Access-Control-Allow-Origin header sent in response to pre-flight OPTIONS requests.
	// Provide a list of origins, e.g. ["https://foobar.com", "https://foobar.net"] or the wildcard value ["*"].
	// No headers will be sent if no origins are provided.
	AccessControlAllowOrigin []string
	// AccessControlAllowMethods specifies the Access-Control-Allow-Methods header sent in response to pre-flight OPTIONS requests.
	// Provide a list of methods, e.g. ["GET", "POST", "PUT", "DELETE"].
	// No headers will be sent if no methods are provided.
	AccessControlAllowMethods []string

	// CSRFSecret is the secret used to sign CSRF tokens. It must be 32 bytes long.
	// This should be considered a sensitive value and should be kept secret.
	// If this is not provided, the Server will generate a random CSRF secret on
	// startup.
	CSRFSecret []byte

	// CSPAllowInlineStyles specifies whether to include `style-src:
	// unsafe-inline` in the Content-Security-Policy header to permit the use of
	// inline CSS.
	CSPAllowInlineStyles bool

	// CookiesSameSiteLax specifies whether to use SameSite=Lax in cookies. The
	// default is to set SameSite=Strict.
	CookiesSameSiteLax bool
}

func (c *Config) setDefaults() error {
	if c.BrowserMux == nil {
		c.BrowserMux = &http.ServeMux{}
	}

	if c.APIMux == nil {
		c.APIMux = &http.ServeMux{}
	}

	if c.CSRFSecret == nil || len(c.CSRFSecret) == 0 {
		c.CSRFSecret = make([]byte, 32)
		if _, err := crand.Read(c.CSRFSecret); err != nil {
			return fmt.Errorf("failed to generate CSRF secret: %w", err)
		}
	}

	return nil
}

// Server is a safeweb server.
type Server struct {
	Config
	h           *http.Server
	csp         string
	csrfProtect func(http.Handler) http.Handler
}

// NewServer creates a safeweb server with the provided configuration. It will
// validate the configuration to ensure that it is complete and return an error
// if not.
func NewServer(config Config) (*Server, error) {
	// ensure that CORS configuration is complete
	corsMethods := len(config.AccessControlAllowMethods) > 0
	corsHosts := len(config.AccessControlAllowOrigin) > 0
	if corsMethods != corsHosts {
		return nil, fmt.Errorf("must provide both AccessControlAllowOrigin and AccessControlAllowMethods or neither")
	}

	// fill in any missing fields
	if err := config.setDefaults(); err != nil {
		return nil, fmt.Errorf("failed to set defaults: %w", err)
	}

	sameSite := csrf.SameSiteStrictMode
	if config.CookiesSameSiteLax {
		sameSite = csrf.SameSiteLaxMode
	}
	s := &Server{
		Config: config,
		csp:    defaultCSP,
		// only set Secure flag on CSRF cookies if we are in a secure context
		// as otherwise the browser will reject the cookie
		csrfProtect: csrf.Protect(config.CSRFSecret, csrf.Secure(config.SecureContext), csrf.SameSite(sameSite)),
	}
	if config.CSPAllowInlineStyles {
		s.csp = defaultCSP + `; style-src 'self' 'unsafe-inline'`
	}
	s.h = &http.Server{Handler: s}
	return s, nil
}

type handlerType int

const (
	unknownHandler handlerType = iota
	apiHandler
	browserHandler
)

// checkHandlerType returns either apiHandler or browserHandler, depending on
// whether apiPattern or browserPattern is more specific (i.e. which pattern
// contains more pathname components). If they are equally specific, it returns
// unknownHandler.
func checkHandlerType(apiPattern, browserPattern string) handlerType {
	c := cmp.Compare(strings.Count(path.Clean(apiPattern), "/"), strings.Count(path.Clean(browserPattern), "/"))
	switch {
	case c > 0:
		return apiHandler
	case c < 0:
		return browserHandler
	default:
		return unknownHandler
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, bp := s.BrowserMux.Handler(r)
	_, ap := s.APIMux.Handler(r)
	switch {
	case bp == "" && ap != "": // APIMux match
		s.serveAPI(w, r)
	case bp != "" && ap == "": // BrowserMux match
		s.serveBrowser(w, r)
	case bp == "" && ap == "": // neither match
		http.NotFound(w, r)
	case bp != "" && ap != "":
		// Both muxes match the path. Route to the more-specific handler (as
		// determined by the number of components in the path). If it somehow
		// happens that both patterns are equally specific, something strange
		// has happened; say so.
		//
		// NOTE: checkHandlerType does not know about what the serve* handlers
		// will do — including, possibly, redirecting to more specific patterns.
		// If you have a less-specific pattern that redirects to something more
		// specific, this logic will not do what you wanted.
		handler := checkHandlerType(ap, bp)
		switch handler {
		case apiHandler:
			s.serveAPI(w, r)
		case browserHandler:
			s.serveBrowser(w, r)
		default:
			s := http.StatusInternalServerError
			log.Printf("conflicting mux paths in safeweb: request %q matches browser mux pattern %q and API mux pattern %q; returning %d", r.URL.Path, bp, ap, s)
			http.Error(w, "multiple handlers match this request", s)
		}
	}
}

func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	// disallow x-www-form-urlencoded requests to the API
	if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
		http.Error(w, "invalid content type", http.StatusBadRequest)
		return
	}

	// set CORS headers for pre-flight OPTIONS requests if any were configured
	if r.Method == "OPTIONS" && len(s.AccessControlAllowOrigin) > 0 {
		w.Header().Set("Access-Control-Allow-Origin", strings.Join(s.AccessControlAllowOrigin, ", "))
		w.Header().Set("Access-Control-Allow-Methods", strings.Join(s.AccessControlAllowMethods, ", "))
	}
	s.APIMux.ServeHTTP(w, r)
}

func (s *Server) serveBrowser(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", s.csp)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referer-Policy", "same-origin")
	s.csrfProtect(s.BrowserMux).ServeHTTP(w, r)
}

// ServeRedirectHTTP serves a single HTTP handler on the provided listener that
// redirects all incoming HTTP requests to the HTTPS address of the provided
// fully qualified domain name (FQDN). Callers are responsible for closing the
// listener.
func (s *Server) ServeRedirectHTTP(ln net.Listener, fqdn string) error {
	return http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		new := url.URL{
			Scheme:   "https",
			Host:     fqdn,
			Path:     r.URL.Path,
			RawQuery: r.URL.RawQuery,
		}

		http.Redirect(w, r, new.String(), http.StatusMovedPermanently)
	}))
}

// Serve starts the server and listens on the provided listener. It will block
// until the server is closed. The caller is responsible for closing the
// listener.
func (s *Server) Serve(ln net.Listener) error {
	return s.h.Serve(ln)
}

// ListenAndServe listens on the TCP network address addr and then calls Serve
// to handle requests on incoming connections. If addr == "", ":http" is used.
func (s *Server) ListenAndServe(addr string) error {
	if addr == "" {
		addr = ":http"
	}
	lst, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(lst)
}

// Close closes all client connections and stops accepting new ones.
func (s *Server) Close() error {
	return s.h.Close()
}
