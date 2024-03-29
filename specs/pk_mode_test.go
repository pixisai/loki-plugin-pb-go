package specs

import (
	"testing"
)

func TestPKModeFromString(t *testing.T) {
	var pkMode PKMode
	if err := pkMode.UnmarshalJSON([]byte(`"loki-id-only"`)); err != nil {
		t.Fatal(err)
	}
	if pkMode != PKModeLokiID {
		t.Fatalf("expected PKModeLokiID, got %v", pkMode)
	}
	if err := pkMode.UnmarshalJSON([]byte(`"default"`)); err != nil {
		t.Fatal(err)
	}
	if pkMode != PKModeDefaultKeys {
		t.Fatalf("expected PKModeCompositeKeys, got %v", pkMode)
	}
}

func TestPKMode(t *testing.T) {
	for _, pkModeStr := range pkModeStrings {
		pkMode, err := PKModeFromString(pkModeStr)
		if err != nil {
			t.Fatal(err)
		}
		if pkModeStr != pkMode.String() {
			t.Fatalf("expected:%s got:%s", pkModeStr, pkMode.String())
		}
	}
}

func TestPKModeMarshalJSON(t *testing.T) {
	pkMode := PKModeLokiID
	if pkModeStr, err := pkMode.MarshalJSON(); err != nil {
		t.Fatal(err)
	} else if string(pkModeStr) != `"loki-id-only"` {
		t.Fatalf("expected:\"loki-id\" got:%s", string(pkModeStr))
	}

	pkMode = PKModeDefaultKeys
	if pkModeStr, err := pkMode.MarshalJSON(); err != nil {
		t.Fatal(err)
	} else if string(pkModeStr) != `"default"` {
		t.Fatalf("expected:\"loki-id\" got:%s", string(pkModeStr))
	}
}
