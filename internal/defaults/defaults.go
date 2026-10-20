// Package defaults holds the connection values a fresh console starts with, so testing against
// Evolt dev does not begin by retyping the same two URLs. Anything typed into the form and saved
// wins over these, and the environment wins over the built-in values so one image can serve both
// a run inside the Evolt network and a partner deployed outside it.
package defaults

import (
	"os"
	"strings"
)

const (
	// partnerPublicURL is the address Evolt calls the mock back on; deployments set PUBLIC_BASE_URL.
	partnerPublicURL = "http://localhost:8080"

	// evoltVersionsURL is Evolt's public OCPI entry point on dev. A deployment can override it
	// through EVOLT_VERSIONS_URL when the entry point changes.
	evoltVersionsURL = "http://bff-external-service.dev.evtech.dev/api/ocpi/versions"
)

// PartnerPublicURL prefers a custom domain, then Render's assigned HTTPS URL.
func PartnerPublicURL() string {
	return env("PUBLIC_BASE_URL", env("RENDER_EXTERNAL_URL", partnerPublicURL))
}

// EvoltVersionsURL answers EVOLT_VERSIONS_URL when the deployment sets it.
func EvoltVersionsURL() string { return env("EVOLT_VERSIONS_URL", evoltVersionsURL) }

// PublicURLAt keeps a typed Partner public URL on the page's mount path ("", "/dual", "/stg", "/stg/dual"),
// so the Versions URL the mock hands to Evolt reaches that same page.
func PublicURLAt(publicURL, mountPath string) string {
	publicURL = strings.TrimRight(strings.TrimSpace(publicURL), "/")
	if mountPath != "" && !strings.HasSuffix(publicURL, mountPath) {
		publicURL += mountPath
	}
	return publicURL
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
