package whatsmeow_service

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

func TestReconnectBackoff(t *testing.T) {
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute}
	for attempt, w := range want {
		if got := reconnectBackoff(attempt); got != w {
			t.Errorf("attempt %d: got %s, want %s", attempt, got, w)
		}
	}
	if got := reconnectBackoff(100); got != 5*time.Minute {
		t.Errorf("attempt 100: got %s, want 5m", got)
	}
}

func TestShouldRetryReconnect(t *testing.T) {
	jid := types.NewJID("5527999999999", types.DefaultUserServer)
	paired := &whatsmeow.Client{Store: &store.Device{ID: &jid}}
	unpaired := &whatsmeow.Client{Store: &store.Device{}}

	cases := []struct {
		name   string
		jid    string
		client *whatsmeow.Client
		want   bool
	}{
		{"pareada, Connect falhou", "5527999999999@s.whatsapp.net", paired, true},
		{"falhou antes de criar o client (GetDevice)", "5527999999999@s.whatsapp.net", nil, true},
		{"sem jid no banco (nunca pareada / deslogada)", "", paired, false},
		{"jid no banco mas device store sumiu: precisa de QR", "5527999999999@s.whatsapp.net", unpaired, false},
	}
	for _, c := range cases {
		if got := shouldRetryReconnect(c.jid, c.client); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
