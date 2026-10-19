package defaults

import "testing"

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
