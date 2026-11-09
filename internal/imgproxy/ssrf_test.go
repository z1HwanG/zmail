package imgproxy

import (
	"context"
	"net"
	"testing"
)

func TestIsPublicIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{{"8.8.8.8", true}, {"2001:4860:4860::8888", true}, {"127.0.0.1", false}, {"10.0.0.1", false}, {"100.64.0.1", false}, {"192.0.2.1", false}, {"198.18.0.1", false}, {"203.0.113.1", false}, {"224.0.0.1", false}, {"240.0.0.1", false}, {"2001:db8::1", false}, {"2002:0a00:0001::1", false}, {"64:ff9b::a00:1", false}}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			if got := isPublicIP(net.ParseIP(tc.ip)); got != tc.want {
				t.Fatalf("isPublicIP(%s)=%v want %v", tc.ip, got, tc.want)
			}
		})
	}
}

func TestValidateTargetBlocksLocalTargets(t *testing.T) {
	p := New(Options{})
	for _, raw := range []string{"http://127.0.0.1/a.png", "http://localhost/a.png", "file:///etc/passwd", "http://user:pass@example.com/a.png", "http://8.8.8.8:25/a.png"} {
		if _, err := p.validateTarget(context.Background(), raw); err == nil {
			t.Errorf("expected rejection for %q", raw)
		}
	}
	if !isLocalHostname("mail.local") || !isLocalHostname("x.home.arpa") || isLocalHostname("example.org") {
		t.Fatal("local hostname classification incorrect")
	}
}

func TestImageContentTypeRejectsActiveContent(t *testing.T) {
	for _, raw := range []string{"text/html", "image/svg+xml", "application/octet-stream", ""} {
		if _, err := imageContentType(raw); err == nil {
			t.Errorf("expected reject %q", raw)
		}
	}
	for _, raw := range []string{"image/png", "image/jpeg; charset=binary"} {
		if _, err := imageContentType(raw); err != nil {
			t.Errorf("unexpected reject %q: %v", raw, err)
		}
	}
}
