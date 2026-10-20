package defaults

import "testing"

func TestPartnerPublicURL(t *testing.T) {
	for _, tc := range []struct{ public, render, want string }{
		{"", "", "http://localhost:8080"},
		{"", "https://console.onrender.com", "https://console.onrender.com"},
		{"https://console.example.com", "https://console.onrender.com", "https://console.example.com"},
	} {
		t.Setenv("PUBLIC_BASE_URL", tc.public)
		t.Setenv("RENDER_EXTERNAL_URL", tc.render)
		if got := PartnerPublicURL(); got != tc.want {
			t.Errorf("PartnerPublicURL() = %q, want %q", got, tc.want)
		}
	}
}

func TestPublicURLAt(t *testing.T) {
	cases := []struct {
		url, mountPath, want string
	}{
		{"https://console/", "", "https://console"},
		{"https://console", "/dual", "https://console/dual"},
		{"https://console/dual", "/dual", "https://console/dual"},
		{" https://console ", "/stg", "https://console/stg"},
		{"https://console/stg/", "/stg", "https://console/stg"},
		{"https://console", "/stg/dual", "https://console/stg/dual"},
		{"https://console/stg/dual", "/stg/dual", "https://console/stg/dual"},
	}
	for _, tc := range cases {
		if got := PublicURLAt(tc.url, tc.mountPath); got != tc.want {
			t.Errorf("PublicURLAt(%q, %q) = %q, want %q", tc.url, tc.mountPath, got, tc.want)
		}
	}
}
