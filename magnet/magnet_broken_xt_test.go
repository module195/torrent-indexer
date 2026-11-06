package magnet

import "testing"

func TestParseMagnetUriBrokenXtPrefix(t *testing.T) {
	m, err := ParseMagnetUri("magnet:?xt=urn3Abtih:7ebb9cb16c50f119f273d21be2ed87dce29b2931&dn=Chicago.PD.S08E03")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := m.InfoHash.String(); got != "7ebb9cb16c50f119f273d21be2ed87dce29b2931" {
		t.Fatalf("infohash = %s", got)
	}
}

func TestParseMagnetUriShortHashStillFails(t *testing.T) {
	// 39 hex chars: a character is really missing, nothing to recover
	if _, err := ParseMagnetUri("magnet:?xt=urn:btih:eb18ba7a5931e4e3b25c5ca2f378be9100%355381"); err == nil {
		t.Fatal("expected an error")
	}
}

// A 32-character base32 xt with '=' padding decodes to fewer than 20 bytes.
// It used to panic, which ended the process when a scraped page carried it.
func TestParseMagnetUriPaddedBase32IsAnError(t *testing.T) {
	for _, xt := range []string{
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAA===",
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAA====",
		"AAAAAAAAAAAAAAAAAAAAAAAAAA======",
	} {
		if _, err := ParseMagnetUri("magnet:?xt=urn:btih:" + xt + "&dn=x"); err == nil {
			t.Fatalf("xt %s: expected an error", xt)
		}
	}
}
