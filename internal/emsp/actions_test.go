package emsp

import "testing"

func TestNextPageURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, link, current, want string
	}{
		{name: "next quoted", link: `<https://cpo.example/locations?offset=100>; rel="next"`, current: "https://cpo.example/locations?offset=0", want: "https://cpo.example/locations?offset=100"},
		{name: "relative next", link: `</api/ocpi/cpo/2.2.1/locations?limit=10&offset=10>; rel="next"`, current: "http://cpo.internal/api/ocpi/cpo/2.2.1/locations?limit=10", want: "http://cpo.internal/api/ocpi/cpo/2.2.1/locations?limit=10&offset=10"},
		{name: "multiple", link: `<https://cpo.example/locations?offset=0>; rel="prev", <https://cpo.example/locations?offset=200>; rel="next"`, current: "https://cpo.example/locations?offset=100", want: "https://cpo.example/locations?offset=200"},
		{name: "none", link: `<https://cpo.example/locations?offset=0>; rel="prev"`, current: "https://cpo.example/locations?offset=100"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := nextPageURL(test.link, test.current); got != test.want {
				t.Fatalf("nextPageURL() = %q, want %q", got, test.want)
			}
		})
	}
}
