package main

import (
	"fmt"
	"math/big"
	"strings"
	"time"
	_ "time/tzdata" // strefa Europe/Warsaw niezależnie od systemu
	"unicode/utf8"
)

// normalizeNRB usuwa spacje/myślniki i prefiks PL, sprawdza długość i cyfry kontrolne IBAN.
// Cyfry kontrolne muszą być kanoniczne (98 - reszta), nie tylko dawać resztę 1:
// inaczej np. "00" przeszłoby tam, gdzie poprawne jest "97".
func normalizeNRB(s string) (string, error) {
	s = strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "\u00a0", "").Replace(strings.TrimSpace(s)))
	s = strings.TrimPrefix(s, "PL")
	if len(s) != 26 || strings.Trim(s, "0123456789") != "" {
		return "", fmt.Errorf("numer konta %q: oczekiwane 26 cyfr", safe(s))
	}
	n, _ := new(big.Int).SetString(s[2:]+"252100", 10) // BBAN + "PL" (P=25, L=21) + "00"
	want := fmt.Sprintf("%02d", 98-new(big.Int).Mod(n, big.NewInt(97)).Int64())
	if s[:2] != want {
		return "", fmt.Errorf("numer konta %s: błędna suma kontrolna (literówka?)", formatNRB(s))
	}
	return s, nil
}

func formatNRB(s string) string {
	if len(s) != 26 {
		return s
	}
	var b strings.Builder
	b.WriteString(s[:2])
	for i := 2; i < 26; i += 4 {
		b.WriteByte(' ')
		b.WriteString(s[i : i+4])
	}
	return b.String()
}

// normalizeAmount: "250", "250,5", "1 234,56" -> "250.00", "250.50", "1234.56". Bez float.
// Spacje dozwolone wyłącznie jako separator tysięcy w poprawnych grupach po 3 cyfry.
func normalizeAmount(s string) (string, error) {
	orig := s
	bad := fmt.Errorf("kwota %q: niepoprawny format (np. 250, 250,50 albo 1 234,56)", safe(orig))
	s = strings.ReplaceAll(strings.TrimSpace(s), "\u00a0", " ")
	s = strings.Replace(s, ",", ".", 1)
	intPart, frac, hasDot := strings.Cut(s, ".")
	if strings.Contains(intPart, " ") {
		groups := strings.Split(intPart, " ")
		if len(groups[0]) < 1 || len(groups[0]) > 3 {
			return "", bad
		}
		for _, g := range groups[1:] {
			if len(g) != 3 {
				return "", bad
			}
		}
		intPart = strings.Join(groups, "")
	}
	if intPart == "" || strings.Trim(intPart, "0123456789") != "" || strings.Trim(frac, "0123456789") != "" || len(frac) > 2 || (hasDot && frac == "") {
		return "", bad
	}
	intPart = strings.TrimLeft(intPart, "0")
	if intPart == "" {
		intPart = "0"
	}
	for len(frac) < 2 {
		frac += "0"
	}
	if intPart == "0" && frac == "00" {
		return "", fmt.Errorf("kwota %q: musi być większa od zera", safe(orig))
	}
	if len(intPart) > 12 {
		return "", fmt.Errorf("kwota %q: za duża", safe(orig))
	}
	return intPart + "." + frac, nil
}

var warsaw = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Warsaw")
	if err != nil {
		return time.Local
	}
	return loc
}()

func today() string { return time.Now().In(warsaw).Format("2006-01-02") }

func checkDate(s, what string) error {
	if _, err := time.Parse("2006-01-02", s); err != nil {
		return fmt.Errorf("%s %q: oczekiwany format RRRR-MM-DD", what, s)
	}
	return nil
}

// resolveAccount: końcówka numeru (min. 4 cyfry), pełny numer albo fragment nazwy.
func resolveAccount(accts []Account, q string) (*Account, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, fmt.Errorf("nie podano konta")
	}
	digits := strings.TrimPrefix(strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(q)), "PL")
	var matches []*Account
	if strings.Trim(digits, "0123456789") == "" {
		if len(digits) < 4 {
			return nil, fmt.Errorf("konto %q: podaj co najmniej 4 ostatnie cyfry numeru", q)
		}
		for i := range accts {
			if strings.HasSuffix(accts[i].Number, digits) {
				matches = append(matches, &accts[i])
			}
		}
	} else {
		lq := strings.ToLower(q)
		for i := range accts {
			if strings.Contains(strings.ToLower(accts[i].Name), lq) || (accts[i].Nickname != "" && strings.Contains(strings.ToLower(accts[i].Nickname), lq)) {
				matches = append(matches, &accts[i])
			}
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return nil, fmt.Errorf("konto %q: nie znaleziono; dostępne: %s", q, accountList(accts))
	default:
		var l []string
		for _, m := range matches {
			l = append(l, m.Label())
		}
		return nil, fmt.Errorf("konto %q: niejednoznaczne: %s", q, strings.Join(l, ", "))
	}
}

func accountList(accts []Account) string {
	var l []string
	for i := range accts {
		l = append(l, accts[i].Label())
	}
	return strings.Join(l, ", ")
}

// TaxPeriod to okres rozliczeniowy przelewu podatkowego.
type TaxPeriod struct {
	Type, Number, Month, Year string
}

func (p TaxPeriod) asData() map[string]any {
	return map[string]any{"type": p.Type, "number": p.Number, "month": p.Month, "year": p.Year}
}

// parsePeriod: RRRR-MM (miesiąc), RRRR-Qn (kwartał), RRRR-Hn (półrocze), RRRR (rok),
// RRRR-MM-Dn (dekada), RRRR-MM-DD (dzień).
func parsePeriod(s string) (TaxPeriod, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	bad := fmt.Errorf("okres %q: użyj RRRR-MM, RRRR-Qn, RRRR-Hn, RRRR, RRRR-MM-Dn albo RRRR-MM-DD", s)
	parts := strings.Split(s, "-")
	year := parts[0]
	if len(year) != 4 || strings.Trim(year, "0123456789") != "" {
		return TaxPeriod{}, bad
	}
	num2 := func(x string, lo, hi int) (string, bool) {
		var n int
		if _, err := fmt.Sscanf(x, "%d", &n); err != nil || strings.Trim(x, "0123456789") != "" || n < lo || n > hi {
			return "", false
		}
		return fmt.Sprintf("%02d", n), true
	}
	switch len(parts) {
	case 1:
		return TaxPeriod{Type: "YEAR", Year: year}, nil
	case 2:
		p := parts[1]
		switch {
		case strings.HasPrefix(p, "Q"):
			if n, ok := num2(p[1:], 1, 4); ok {
				return TaxPeriod{Type: "QUARTER", Number: n, Year: year}, nil
			}
		case strings.HasPrefix(p, "H"):
			if n, ok := num2(p[1:], 1, 2); ok {
				return TaxPeriod{Type: "SEMESTER", Number: n, Year: year}, nil
			}
		default:
			if m, ok := num2(p, 1, 12); ok {
				return TaxPeriod{Type: "MONTH", Month: m, Year: year}, nil
			}
		}
	case 3:
		m, ok := num2(parts[1], 1, 12)
		if !ok {
			return TaxPeriod{}, bad
		}
		p := parts[2]
		if strings.HasPrefix(p, "D") {
			if n, ok := num2(p[1:], 1, 3); ok {
				return TaxPeriod{Type: "DECADE", Number: n, Month: m, Year: year}, nil
			}
		} else if d, ok := num2(p, 1, 31); ok {
			if _, err := time.Parse("2006-01-02", year+"-"+m+"-"+d); err != nil {
				return TaxPeriod{}, bad
			}
			return TaxPeriod{Type: "DAY", Number: d, Month: m, Year: year}, nil
		}
	}
	return TaxPeriod{}, bad
}

// safe zamienia znaki sterujące, niepoprawne UTF-8 i znaki zmiany kierunku pisma na widoczne
// sekwencje \uXXXX. Używane dla wszystkiego, co przychodzi z banku i trafia na terminal.
func safe(s string) string {
	if !strings.ContainsFunc(s, isUnsafeRune) && utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	for i, r := range s {
		if r == utf8.RuneError {
			if _, size := utf8.DecodeRuneInString(s[i:]); size == 1 {
				b.WriteString(`\uFFFD`)
				continue
			}
		}
		if isUnsafeRune(r) {
			fmt.Fprintf(&b, `\u%04X`, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isUnsafeRune(r rune) bool {
	switch {
	case r < 0x20, r == 0x7f, r >= 0x80 && r < 0xa0: // C0, DEL, C1 (w tym ESC i CSI)
		return true
	case r == 0x061c, r == 0x200e, r == 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069: // sterowanie kierunkiem
		return true
	case r == 0xfeff, r == 0x2028, r == 0x2029:
		return true
	}
	return false
}

// checkUserText: pola dyspozycji (nazwa, tytuł, adres) bez znaków sterujących i z poprawnym UTF-8.
func checkUserText(name, v string) error {
	if !utf8.ValidString(v) {
		return fmt.Errorf("%s: niepoprawne kodowanie (wymagane UTF-8)", name)
	}
	if strings.ContainsFunc(v, isUnsafeRune) {
		return fmt.Errorf("%s: zawiera niedozwolone znaki sterujące", name)
	}
	return nil
}
