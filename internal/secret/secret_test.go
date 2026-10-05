package secret

import (
	"errors"
	"strings"
	"testing"
)

func mustKeys(t *testing.T) (string, *Keys) {
	t.Helper()
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	keys, err := Derive(key)
	if err != nil {
		t.Fatal(err)
	}
	return key, keys
}

func TestDeriveIsStableAndKeepsTheKeyOutOfTheToken(t *testing.T) {
	key, keys := mustKeys(t)
	again, err := Derive(" " + key + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if again.HubToken != keys.HubToken {
		t.Fatal("the same key gave two hub tokens")
	}
	if !strings.HasPrefix(keys.HubToken, TokenPrefix) || strings.Contains(keys.HubToken, strings.TrimPrefix(key, KeyPrefix)) {
		t.Fatalf("hub token %q is not a derived token", keys.HubToken)
	}
}

func TestDeriveRefusesWhatIsNotAKey(t *testing.T) {
	_, keys := mustKeys(t)
	for _, bad := range []string{"", "ctk_", "ctk_short", "hello", keys.HubToken} {
		if _, err := Derive(bad); err == nil {
			t.Errorf("Derive(%q) succeeded", bad)
		}
	}
}

func TestSealOpensOnlyForTheSameKeyAndRoute(t *testing.T) {
	_, keys := mustKeys(t)
	_, other := mustKeys(t)
	box, err := keys.Seal([]byte(`{"do":"refresh"}`), "macbook", "lychee")
	if err != nil {
		t.Fatal(err)
	}

	got, err := keys.Open(box, "macbook", "lychee")
	if err != nil || string(got) != `{"do":"refresh"}` {
		t.Fatalf("Open = %q, %v", got, err)
	}

	// The hub could try any of these. None may open.
	if _, err := keys.Open(box, "macbook", "studio"); !errors.Is(err, ErrSealed) {
		t.Error("opened for a device it was not sealed for")
	}
	if _, err := keys.Open(box, "studio", "lychee"); !errors.Is(err, ErrSealed) {
		t.Error("opened as coming from another device")
	}
	if _, err := other.Open(box, "macbook", "lychee"); !errors.Is(err, ErrSealed) {
		t.Error("opened with another key")
	}
	if _, err := keys.Open(box[:len(box)-4]+"AAAA", "macbook", "lychee"); !errors.Is(err, ErrSealed) {
		t.Error("opened after being altered")
	}
	if _, err := keys.Open("!!", "macbook", "lychee"); !errors.Is(err, ErrSealed) {
		t.Error("opened nonsense")
	}
}

func TestSealNeverRepeats(t *testing.T) {
	_, keys := mustKeys(t)
	a, _ := keys.Seal([]byte("x"), "a", "b")
	b, _ := keys.Seal([]byte("x"), "a", "b")
	if a == b {
		t.Fatal("two seals of one message were identical")
	}
}
