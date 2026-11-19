package main

// Książka odbiorców (przelewy zwykłe): lista i przelew do zapisanego odbiorcy.
// Przelew startuje z /normal/recipient-pay, które zwraca formularz wypełniony przez bank
// (rachunek, nazwa, adres zablokowane do edycji; tytuł i rachunek źródłowy domyślne).

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Recipient struct {
	ID              string
	Name            string // nazwa odbiorcy (jak w przelewie)
	OwnName         string // Twoja nazwa w książce
	Account         string // NRB
	Address         string
	Title           string // domyślny tytuł
	SourceAccountID string // rachunek, z którego zwykle płacisz (identyfikator sesyjny)
}

func (r *Recipient) Label() string {
	n := r.Name
	if r.OwnName != "" && !strings.EqualFold(r.OwnName, r.Name) {
		n = r.OwnName + " (" + r.Name + ")"
	}
	return fmt.Sprintf("%s, %s", safe(n), formatNRB(safe(r.Account)))
}

const maxRecipientPages = 50

func (c *Client) Recipients() ([]Recipient, error) {
	var out []Recipient
	seen := map[string]bool{}
	data := map[string]any{}
	for page := 0; page < maxRecipientPages; page++ {
		rep, err := c.Call("/ipko3/transactions/recipients/normal", Request{Data: data})
		if err != nil {
			return out, fmt.Errorf("odbiorcy: %w", err)
		}
		var r struct {
			Data struct {
				Items []struct {
					ID               string `json:"recipient_id"`
					Name             string `json:"recipient_name"`
					OwnName          string `json:"recipient_own_name"`
					Address          string `json:"recipient_address"`
					Title            string `json:"title"`
					RecipientAccount struct {
						Number string `json:"number"`
					} `json:"recipient_account"`
					SourceAccount struct {
						ID string `json:"id"`
					} `json:"source_account"`
				} `json:"items"`
				Next json.RawMessage `json:"next"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rep.Response, &r); err != nil {
			return out, fmt.Errorf("odbiorcy: niepoprawna odpowiedź")
		}
		added := 0
		for _, it := range r.Data.Items {
			if it.ID == "" || seen[it.ID] {
				continue
			}
			seen[it.ID] = true
			out = append(out, Recipient{ID: it.ID, Name: it.Name, OwnName: it.OwnName, Account: it.RecipientAccount.Number,
				Address: it.Address, Title: it.Title, SourceAccountID: it.SourceAccount.ID})
			added++
		}
		if len(r.Data.Next) == 0 || string(r.Data.Next) == "null" {
			return out, nil
		}
		if added == 0 {
			return out, fmt.Errorf("odbiorcy: stronicowanie banku się zapętliło; lista może być niepełna")
		}
		data = map[string]any{"action": r.Data.Next, "filter_form": map[string]any{"recipient": nil, "source_account": "ALL"}}
	}
	return out, fmt.Errorf("odbiorcy: przekroczono limit %d stron", maxRecipientPages)
}

// resolveRecipient: Twoja nazwa, nazwa odbiorcy (fragment, bez wielkości liter) albo końcówka numeru (min. 4 cyfry).
// Dokładne dopasowanie nazwy wygrywa z fragmentem, żeby "mBank" nie kolidował z "mBank firmowy".
func resolveRecipient(list []Recipient, q string) (*Recipient, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, fmt.Errorf("nie podano odbiorcy")
	}
	digits := strings.TrimPrefix(strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(q)), "PL")
	var exact, partial []*Recipient
	for i := range list {
		r := &list[i]
		if strings.Trim(digits, "0123456789") == "" {
			if len(digits) >= 4 && strings.HasSuffix(r.Account, digits) {
				exact = append(exact, r)
			}
			continue
		}
		lq := strings.ToLower(q)
		switch {
		case strings.EqualFold(r.OwnName, q) || strings.EqualFold(r.Name, q):
			exact = append(exact, r)
		case strings.Contains(strings.ToLower(r.OwnName), lq) || strings.Contains(strings.ToLower(r.Name), lq):
			partial = append(partial, r)
		}
	}
	if strings.Trim(digits, "0123456789") == "" && len(digits) < 4 {
		return nil, fmt.Errorf("odbiorca %q: podaj co najmniej 4 ostatnie cyfry numeru", safe(q))
	}
	matches := exact
	if len(matches) == 0 {
		matches = partial
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		var l []string
		for i := range list {
			l = append(l, list[i].Label())
		}
		return nil, fmt.Errorf("odbiorca %q: nie znaleziono w książce; dostępni: %s", safe(q), strings.Join(l, "; "))
	default:
		var l []string
		for _, m := range matches {
			l = append(l, m.Label())
		}
		return nil, fmt.Errorf("odbiorca %q: niejednoznaczny: %s", safe(q), strings.Join(l, "; "))
	}
}

type RecipientTransfer struct {
	Recipient   *Recipient
	From        *Account // nil = rachunek przypisany do odbiorcy
	Amount      string
	Title       string // "" = domyślny tytuł z książki
	Date        string
	PaymentType string // "" = domyślny wg banku (np. INTERNAL dla odbiorcy w PKO)
}

func (f formField) stringValue() string {
	var s string
	if json.Unmarshal(f.Value, &s) != nil {
		return ""
	}
	return s
}

func (c *Client) TransferToRecipient(t RecipientTransfer, accts []Account, confirm Confirmer) error {
	r := t.Recipient
	// Otwarcie formularza niczego nie zleca; potrzebujemy go, żeby pokazać dane z banku przed zgodą.
	f, rep, err := c.StartFlow("/ipko3/transactions/transfers/normal/recipient-pay", map[string]any{"id": r.ID})
	if err != nil {
		return fmt.Errorf("przelew do odbiorcy: %w", err)
	}
	if rep.StateID != "fill_form" {
		f.Cancel()
		return fmt.Errorf("przelew do odbiorcy: nieoczekiwany stan %q", safe(rep.StateID))
	}
	fs := parseFields(rep)
	account := fs["recipient_account"].stringValue()
	name := fs["recipient_name"].stringValue()
	address := fs["recipient_address"].stringValue()
	// Integralność: formularz banku musi dotyczyć dokładnie tego odbiorcy, którego wybrano z listy.
	if account == "" || account != r.Account || name == "" {
		f.Cancel()
		return fmt.Errorf("przelew do odbiorcy: formularz banku nie zgadza się z wybranym odbiorcą; przerwano")
	}
	if _, err := normalizeNRB(account); err != nil {
		f.Cancel()
		return fmt.Errorf("przelew do odbiorcy: %v", err)
	}
	if err := checkUserText("nazwa odbiorcy", name+address); err != nil {
		f.Cancel()
		return fmt.Errorf("przelew do odbiorcy: dane z książki: %v", err)
	}
	source := fs["source_account"].stringValue()
	if t.From != nil {
		if !fs["source_account"].hasItem(t.From.ID) {
			f.Cancel()
			return fmt.Errorf("bank nie pozwala płacić temu odbiorcy z rachunku %s", t.From.Label())
		}
		source = t.From.ID
	}
	var src *Account
	for i := range accts {
		if accts[i].ID == source {
			src = &accts[i]
		}
	}
	if src == nil {
		f.Cancel()
		return fmt.Errorf("przelew do odbiorcy: nieznany rachunek źródłowy; podaj --from")
	}
	if src.Currency != "PLN" {
		f.Cancel()
		return fmt.Errorf("przelew krajowy wymaga rachunku w PLN (%s jest w %s)", src.Label(), safe(src.Currency))
	}
	pt := fs["payment_type"].stringValue()
	if t.PaymentType != "" {
		if !fs["payment_type"].hasItem(t.PaymentType) {
			f.Cancel()
			return fmt.Errorf("rodzaj przelewu %s jest niedostępny dla tego odbiorcy", t.PaymentType)
		}
		pt = t.PaymentType
	}
	if pt == "" {
		pt = "ELIXIR"
	}
	title := t.Title
	if title == "" {
		title = fs["title"].stringValue()
	}
	if strings.TrimSpace(title) == "" {
		f.Cancel()
		return fmt.Errorf("odbiorca nie ma domyślnego tytułu; podaj --title")
	}
	var info struct {
		Data struct {
			RecipientBank string `json:"recipient_bank"`
		} `json:"data"`
	}
	json.Unmarshal(rep.Response, &info)

	summary := fmt.Sprintf("PRZELEW DO ODBIORCY Z KSIĄŻKI\n  z:        %s\n  do:       %s\n  odbiorca: %s\n",
		src.Label(), formatNRB(account), safe(name))
	if r.OwnName != "" && !strings.EqualFold(r.OwnName, name) {
		summary += fmt.Sprintf("  w książce: %s\n", safe(r.OwnName))
	}
	if address != "" {
		summary += fmt.Sprintf("  adres:    %s\n", safe(address))
	}
	if b := strings.TrimSpace(info.Data.RecipientBank); b != "" {
		summary += fmt.Sprintf("  bank:     %s\n", safe(b))
	}
	summary += fmt.Sprintf("  kwota:    %s PLN\n  tytuł:    %s\n  data:     %s, %s\n", t.Amount, safe(title), t.Date, pt)
	if err := confirm(summary); err != nil {
		f.Cancel()
		return err
	}
	data := map[string]any{
		"recipient_account": account,
		"recipient_name":    name,
		"hold_create":       false,
		"money":             map[string]any{"amount": t.Amount, "currency": "PLN"},
		"payment_date":      t.Date,
		"payment_type":      pt,
		"source_account":    source,
		"title":             title,
	}
	if address != "" {
		data["recipient_address"] = address
	}
	rep, err = f.Step("submit", data)
	if err != nil {
		return fmt.Errorf("przelew do odbiorcy: %w", err)
	}
	if rep.StateID == "fill_form" {
		return c.formRejected(f, rep, "przelew do odbiorcy")
	}
	c.bankSummary(rep, pt)
	if _, err := c.finishAuthorized(f, rep, "przelew"); err != nil {
		return err
	}
	return nil
}
