package mail

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	gomail "github.com/wneessen/go-mail"
	"zmail/internal/config"
	"zmail/internal/store"
)

func TestCheckPublicIP(t *testing.T) {
	cases := []struct {
		ip      string
		wantErr bool
	}{
		{"8.8.8.8", false}, {"2001:4860:4860::8888", false},
		{"127.0.0.1", true}, {"10.1.2.3", true}, {"100.64.0.1", true},
		{"192.0.2.1", true}, {"198.18.0.1", true}, {"203.0.113.1", true},
		{"224.0.0.1", true}, {"240.0.0.1", true}, {"2001:db8::1", true},
		{"2002:0a00:0001::1", true}, {"64:ff9b::a00:1", true},
	}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			err := checkPublicIP("test", net.ParseIP(tc.ip))
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestResolveMailHost(t *testing.T) {
	m := &Manager{cfg: &config.Config{}}
	if _, err := m.resolveMailHost(context.Background(), "localhost"); err == nil {
		t.Fatal("localhost should be blocked")
	}
	if _, err := m.resolveMailHost(context.Background(), "127.0.0.1"); err == nil {
		t.Fatal("loopback should be blocked")
	}
	m.cfg.AllowPrivateIMAP = true
	ips, err := m.resolveMailHost(context.Background(), "127.0.0.1")
	if err != nil || len(ips) != 1 || !ips[0].Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("ips=%v err=%v", ips, err)
	}
	if _, err = dialPinned(context.Background(), 25, nil, time.Second); err == nil {
		t.Fatal("empty IP list must fail")
	}
}

func TestIsAmbiguousSMTPError(t *testing.T) {
	cases := []struct {
		name   string
		reason gomail.SendErrReason
		want   bool
	}{
		{"data", gomail.ErrSMTPData, false}, {"content", gomail.ErrWriteContent, true},
		{"data close", gomail.ErrSMTPDataClose, true}, {"reset", gomail.ErrSMTPReset, true},
		{"recipient", gomail.ErrSMTPRcptTo, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := &gomail.SendError{Reason: tc.reason}
			if got := isAmbiguousSMTPError(err); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
	if isAmbiguousSMTPError(errors.New("network connection closed")) {
		t.Fatal("pre-DATA transport error should be safely retryable")
	}
}

func TestOutgoingMessageIDIsStable(t *testing.T) {
	acc := &store.Account{Email: "owner@example.com", Username: "owner@example.com"}
	req := SendRequest{MessageID: "fixed-id@example.com", To: []string{"user@example.net"}, Subject: "test", TextBody: "hello"}
	_, _, id1, err := buildOutgoingMessage(acc, req)
	if err != nil {
		t.Fatal(err)
	}
	_, _, id2, err := buildOutgoingMessage(acc, req)
	if err != nil {
		t.Fatal(err)
	}
	if id1 != req.MessageID || id2 != id1 {
		t.Fatalf("ids %q %q", id1, id2)
	}
}

func TestMailPublicUnicast(t *testing.T) {
	if !mailPublicUnicast(netip.MustParseAddr("9.9.9.9")) {
		t.Fatal("public IPv4 rejected")
	}
	if mailPublicUnicast(netip.MustParseAddr("192.0.2.5")) {
		t.Fatal("documentation range accepted")
	}
}
