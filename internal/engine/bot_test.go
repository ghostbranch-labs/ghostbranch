package engine

import (
	"testing"
	"time"
)

// Shadow-only commits are signed as the Ghostbranch GitHub App's bot, so
// GitHub shows the app's name and logo.
func TestBotIsTheAppBot(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	e := &Engine{Now: func() time.Time { return when }}
	got := e.bot()
	if got.Name != "ghostbranch-cli[bot]" {
		t.Errorf("name = %q", got.Name)
	}
	if got.Email != "340449680+ghostbranch-cli[bot]@users.noreply.github.com" {
		t.Errorf("email = %q", got.Email)
	}
	if !got.When.Equal(when) {
		t.Errorf("when = %v", got.When)
	}
}
