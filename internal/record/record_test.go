package record

import (
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := &Share{Token: NewToken(), Path: "/x", Created: time.Now()}
	if len(s.Token) != 26 {
		t.Errorf("token %q is not 128 bits of base32", s.Token)
	}
	if err := Create(s); err != nil || !ValidID(s.ID) {
		t.Fatalf("create gave id %q: %v", s.ID, err)
	}
	id := s.ID
	got, err := Load(id)
	if err != nil || got.Token != s.Token || !got.Expires.IsZero() {
		t.Fatalf("load %+v %v", got, err)
	}
	all, _ := List()
	if len(all) != 1 {
		t.Errorf("list has %d", len(all))
	}
	if err := Remove(id); err != nil {
		t.Fatal(err)
	}
	if err := Remove(id); err != nil {
		t.Error("second remove failed")
	}
	for _, bad := range []string{"../x", "ABCDEF", "a1b2c", ""} {
		if _, err := Load(bad); err == nil {
			t.Errorf("Load(%q) accepted", bad)
		}
	}
}

func TestRemoveHubOnlyRemovesItsOwn(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := SaveHub(&Hub{PID: 200}); err != nil {
		t.Fatal(err)
	}
	RemoveHub(100) // an older hub, gone without its supervisor
	if !HubWritten() {
		t.Fatal("a hub removed another hub's hub.json")
	}
	RemoveHub(200)
	if HubWritten() {
		t.Fatal("a hub left its own hub.json behind")
	}
}
