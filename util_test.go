package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestNormalizeNRB(t *testing.T) {
	ok := map[string]string{
		"47 1160 0000 0000 0000 1234 5678":    "47116000000000000012345678",
		"PL47116000000000000012345678":        "47116000000000000012345678",
		"pl 95 1010 0071 2222 0000 0001 2345": "95101000712222000000012345",
	}
	for in, want := range ok {
		got, err := normalizeNRB(in)
		if err != nil || got != want {
			t.Errorf("normalizeNRB(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"47 1160 0000 0000 0000 1234 5679", "123", "4711600000000000001234567X", ""} {
		if _, err := normalizeNRB(in); err == nil {
			t.Errorf("normalizeNRB(%q): expected error", in)
		}
	}
}

func TestNormalizeAmount(t *testing.T) {
	ok := map[string]string{"250": "250.00", "250,5": "250.50", "1 234,00": "1234.00", "0.01": "0.01", "007": "7.00", "12345.00": "12345.00"}
	for in, want := range ok {
		got, err := normalizeAmount(in)
		if err != nil || got != want {
			t.Errorf("normalizeAmount(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "0,00", "-5", "1.234", "1,2,3", "abc", "12.", "1e3", "1234567890123", "12 34", "1_2", "1 23", " 1234 567"} {
		if got, err := normalizeAmount(in); err == nil {
			t.Errorf("normalizeAmount(%q) = %q: expected error", in, got)
		}
	}
}

func TestParsePeriod(t *testing.T) {
	ok := map[string]TaxPeriod{
		"2026-10":    {Type: "MONTH", Month: "10", Year: "2026"},
		"2026-q3":    {Type: "QUARTER", Number: "03", Year: "2026"},
		"2026-H1":    {Type: "SEMESTER", Number: "01", Year: "2026"},
		"2026":       {Type: "YEAR", Year: "2026"},
		"2026-10-D2": {Type: "DECADE", Number: "02", Month: "10", Year: "2026"},
		"2026-10-15": {Type: "DAY", Number: "15", Month: "10", Year: "2026"},
	}
	for in, want := range ok {
		got, err := parsePeriod(in)
		if err != nil || got != want {
			t.Errorf("parsePeriod(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "26-10", "2026-13", "2026-Q5", "2026-H3", "2026-10-D4", "2026-10-32", "2026-1x", "2026-02-29", "2026-04-31"} {
		if _, err := parsePeriod(in); err == nil {
			t.Errorf("parsePeriod(%q): expected error", in)
		}
	}
}

func TestResolveAccount(t *testing.T) {
	accts := []Account{
		{ID: "a", Name: "PKO KONTO ZA ZERO", Number: "80102000000000111122223333"},
		{ID: "b", Name: "RACHUNEK FIRMOWY", Number: "27102000000000444455556666"},
		{ID: "c", Name: "RACHUNEK OSZCZĘDNOŚCIOWY", Number: "07102000000000999955556666"},
	}
	cases := map[string]string{"3333": "a", "80 1020 0000 0000 1111 2222 3333": "a", "firmowy": "b", "zero": "a", "07102000000000999955556666": "c"}
	for q, want := range cases {
		got, err := resolveAccount(accts, q)
		if err != nil || got.ID != want {
			t.Errorf("resolveAccount(%q) = %v, %v; want %s", q, got, err, want)
		}
	}
	for _, q := range []string{"6666", "rachunek", "123", "9999", ""} {
		if got, err := resolveAccount(accts, q); err == nil {
			t.Errorf("resolveAccount(%q) = %s: expected error", q, got.ID)
		}
	}
}

func TestDecodeSecurityValue(t *testing.T) {
	cases := map[string]string{
		`"12345678"`:   "12345678",
		`"Ascii"pass"`: `Ascii"pass`,
		`0x48612273C5826F5C7A65  "Ha"s\305\202o\134ze"`: "Ha\"sło\\ze",
	}
	for in, want := range cases {
		got, ok := decodeSecurityValue(in)
		if !ok || got != want {
			t.Errorf("decodeSecurityValue(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	if _, ok := decodeSecurityValue("<NULL>"); ok {
		t.Error("decodeSecurityValue(<NULL>): expected failure")
	}
}

func TestCollectErrors(t *testing.T) {
	raw := json.RawMessage(`{"fields":{"title":{"value":"","errors":{"title":"","description":"","hint":"Podaj poprawną wartość."}},
		"money":{"value":{"amount":{"value":"","errors":{"hint":"Za mało środków"}}}},"errors":null}}`)
	got := strings.Join(collectErrors(raw), "|")
	if !strings.Contains(got, "title: Podaj poprawną wartość.") || !strings.Contains(got, "Za mało środków") {
		t.Errorf("collectErrors = %q", got)
	}
	if e := collectErrors(json.RawMessage(`{"fields":{"errors":null,"a":{"errors":null}}}`)); len(e) != 0 {
		t.Errorf("collectErrors(no errors) = %v", e)
	}
}

func TestSaveFile(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/Zestawienie.pdf"
	for i, want := range []string{p, dir + "/Zestawienie (2).pdf", dir + "/Zestawienie (3).pdf"} {
		got, err := saveFile(p, []byte("x"), saveUnique)
		if err != nil || got != want {
			t.Fatalf("saveFile unique #%d = %q, %v; want %q", i, got, err, want)
		}
	}
	if _, err := saveFile(p, []byte("y"), saveNew); err == nil {
		t.Error("saveNew nie może nadpisać istniejącego pliku")
	}
	// --force zastępuje dowiązanie, a nie plik, na który wskazuje
	target := dir + "/cel.txt"
	os.WriteFile(target, []byte("nie ruszać"), 0o600)
	link := dir + "/link.pdf"
	os.Symlink(target, link)
	if _, err := saveFile(link, []byte("nowe"), saveReplace); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "nie ruszać" {
		t.Error("zapis poszedł przez dowiązanie symboliczne")
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink != 0 || fi.Mode().Perm() != 0o600 {
		t.Errorf("oczekiwany zwykły plik 0600, jest %v", fi.Mode())
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 5 {
		t.Errorf("zostały pliki tymczasowe: %d wpisów", len(ents))
	}
}

func TestStatementFileName(t *testing.T) {
	cases := map[string]string{
		"Zestawienie operacji za 01.10.2026 - 31.10.2026.pdf": "Zestawienie operacji za 01.10.2026 - 31.10.2026.pdf",
		"../../etc/passwd.pdf":                                "zestawienie_2026-10-01_2026-10-31.pdf",
		"a/b/raport.pdf":                                      "a-b-raport.pdf",
		".hidden.pdf":                                         "zestawienie_2026-10-01_2026-10-31.pdf",
		"plik.exe":                                            "zestawienie_2026-10-01_2026-10-31.pdf",
		"ok\x1b[2J.pdf":                                       "zestawienie_2026-10-01_2026-10-31.pdf",
		"":                                                    "zestawienie_2026-10-01_2026-10-31.pdf",
	}
	for in, want := range cases {
		if got := statementFileName(in, "2026-10-01", "2026-10-31", "pdf"); got != want {
			t.Errorf("statementFileName(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestSafe(t *testing.T) {
	cases := map[string]string{
		"Zażółć gęślą jaźń": "Zażółć gęślą jaźń",
		"a\x1b[31mb":        `a\u001B[31mb`,
		"x\u202Ey":          `x\u202Ey`,
		"tab\there":         `tab\u0009here`,
		"bad\xffutf":        `bad\uFFFDutf`,
	}
	for in, want := range cases {
		if got := safe(in); got != want {
			t.Errorf("safe(%q) = %q; want %q", in, got, want)
		}
	}
	if err := checkUserText("--title", "ok\x1b[2J"); err == nil {
		t.Error("checkUserText powinien odrzucić ESC")
	}
	if err := checkUserText("--title", "Faktura 1/2026 – zażółć"); err != nil {
		t.Errorf("checkUserText: %v", err)
	}
}

func TestNRBCanonicalCheckDigits(t *testing.T) {
	// BBAN, dla którego poprawne cyfry kontrolne to 97 albo 98: wariant o 97 mniejszy (00 / 01) też daje resztę 1.
	for _, bban := range []string{"102000000000000000000000", "116000000000000000000000", "101000712222000000000000"} {
		var good string
		for cc := 2; cc <= 98; cc++ {
			if nrb := fmt.Sprintf("%02d%s", cc, bban); func() bool { _, err := normalizeNRB(nrb); return err == nil }() {
				good = nrb
			}
		}
		if good == "" {
			t.Fatalf("brak poprawnego NRB dla %s", bban)
		}
		if good[:2] == "97" || good[:2] == "98" {
			alias := fmt.Sprintf("%02d%s", (int(good[0]-'0')*10+int(good[1]-'0'))-97, bban)
			if _, err := normalizeNRB(alias); err == nil {
				t.Errorf("niekanoniczne cyfry kontrolne %s przeszły walidację", alias)
			}
		}
	}
}
