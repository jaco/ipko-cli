package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Każdy przelew: formularz -> submit (bank wysyła powiadomienie do IKO) -> zatwierdzenie
// w IKO -> submit -> END. Przed submit wywołujący musi uzyskać zgodę użytkownika (confirm).

type NormalTransfer struct {
	From                *Account
	To, Name, Address   string
	Amount, Title, Date string
	PaymentType         string // ELIXIR, EXPRESS-ELIXIR, SORBNET
}

type OwnTransfer struct {
	From, To            *Account
	Amount, Title, Date string
}

type TaxTransfer struct {
	From         *Account
	Symbol       string
	Account      string // mikrorachunek podatkowy albo rachunek urzędu
	TaxOffice    bool   // true = rachunek urzędu (TAX_OFFICE) zamiast mikrorachunku
	Amount, Date string
	PaymentType  string // ELIXIR, EXPRESS-ELIXIR
	Period       TaxPeriod
	ObligationID string
}

// Confirmer pyta użytkownika o zgodę; zwraca błąd, gdy jej nie ma.
type Confirmer func(summary string) error

// bankSummary wypisuje to, co bank pokazał po walidacji formularza (bank odbiorcy, opłata, nazwa urzędu).
func (c *Client) bankSummary(rep *Reply, paymentType string) {
	var r struct {
		Data struct {
			RecipientBank string `json:"recipient_bank"`
			RecipientName string `json:"recipient_name"`
			Fees          map[string]struct {
				Amount   string `json:"amount"`
				Currency string `json:"currency"`
			} `json:"transaction_fees"`
		} `json:"data"`
	}
	if json.Unmarshal(rep.Response, &r) != nil {
		return
	}
	if b := strings.TrimSpace(r.Data.RecipientBank); b != "" {
		fmt.Fprintf(c.out, "Bank odbiorcy wg PKO: %s\n", safe(b))
	}
	if n := strings.TrimSpace(r.Data.RecipientName); n != "" {
		fmt.Fprintf(c.out, "Odbiorca wg PKO: %s\n", safe(n))
	}
	if fee, ok := r.Data.Fees[paymentType]; ok {
		fmt.Fprintf(c.out, "Opłata (%s): %s %s\n", paymentType, safe(fee.Amount), safe(fee.Currency))
	}
	fmt.Fprintln(c.out, "Sprawdź dane na ekranie IKO; jeśli coś się nie zgadza, odrzuć tam dyspozycję.")
}

func (c *Client) formRejected(f *Flow, rep *Reply, what string) error {
	f.Cancel()
	return fmt.Errorf("%s: bank odrzucił formularz (stan %q)%s", what, safe(rep.StateID), errorsSuffix(rep))
}

func (c *Client) TransferNormal(t NormalTransfer, confirm Confirmer) error {
	if t.From.Currency != "PLN" {
		return fmt.Errorf("przelew krajowy wymaga rachunku w PLN (%s jest w %s)", t.From.Label(), t.From.Currency)
	}
	summary := fmt.Sprintf(
		"PRZELEW ZWYKŁY\n  z:        %s\n  do:       %s\n  odbiorca: %s\n",
		t.From.Label(), formatNRB(t.To), safe(t.Name))
	if t.Address != "" {
		summary += fmt.Sprintf("  adres:    %s\n", safe(t.Address))
	}
	summary += fmt.Sprintf("  kwota:    %s PLN\n  tytuł:    %s\n  data:     %s, %s\n", t.Amount, safe(t.Title), t.Date, t.PaymentType)
	if t.PaymentType != "ELIXIR" {
		summary += "  uwaga:    " + t.PaymentType + " jest zwykle płatny wg cennika banku (opłata pokaże się przed IKO)\n"
	}
	if err := confirm(summary); err != nil {
		return err
	}
	f, rep, err := c.StartFlow("/ipko3/transactions/transfers/normal/shortcut", nil)
	if err != nil {
		return fmt.Errorf("przelew: %w", err)
	}
	if rep.StateID != "fill_form" {
		f.Cancel()
		return fmt.Errorf("przelew: nieoczekiwany stan %q", rep.StateID)
	}
	data := map[string]any{
		"recipient_account": t.To,
		"recipient_name":    t.Name,
		"hold_create":       false,
		"money":             map[string]any{"amount": t.Amount, "currency": "PLN"},
		"payment_date":      t.Date,
		"payment_type":      t.PaymentType,
		"source_account":    t.From.ID,
		"title":             t.Title,
	}
	if t.Address != "" {
		data["recipient_address"] = t.Address
	}
	rep, err = f.Step("submit", data)
	if err != nil {
		return fmt.Errorf("przelew: %w", err)
	}
	if rep.StateID == "fill_form" {
		return c.formRejected(f, rep, "przelew")
	}
	c.bankSummary(rep, t.PaymentType)
	if _, err := c.finishAuthorized(f, rep, "przelew"); err != nil {
		return err
	}
	return nil
}

func (c *Client) TransferOwn(t OwnTransfer, confirm Confirmer) error {
	if t.From.ID == t.To.ID {
		return fmt.Errorf("rachunek źródłowy i docelowy są takie same")
	}
	if t.From.Currency != t.To.Currency {
		return fmt.Errorf("przelew między rachunkami w różnych walutach nie jest obsługiwany (%s -> %s)", t.From.Currency, t.To.Currency)
	}
	if err := confirm(fmt.Sprintf(
		"PRZELEW WŁASNY\n  z:     %s\n  do:    %s\n  kwota: %s %s\n  tytuł: %s\n  data:  %s\n",
		t.From.Label(), t.To.Label(), t.Amount, safe(t.From.Currency), safe(t.Title), t.Date)); err != nil {
		return err
	}
	f, rep, err := c.StartFlow("/ipko3/transactions/transfers/own", nil)
	if err != nil {
		return fmt.Errorf("przelew własny: %w", err)
	}
	if rep.StateID != "transfer_0" {
		f.Cancel()
		return fmt.Errorf("przelew własny: nieoczekiwany stan %q", rep.StateID)
	}
	rep, err = f.Step("submit", map[string]any{
		"source_account":      t.From.ID,
		"destination_account": t.To.ID,
		"money":               map[string]any{"amount": t.Amount, "currency": t.From.Currency},
		"payment_date":        t.Date,
		"title":               t.Title,
		"hold_create":         false,
	})
	if err != nil {
		return fmt.Errorf("przelew własny: %w", err)
	}
	if rep.StateID == "transfer_0" {
		return c.formRejected(f, rep, "przelew własny")
	}
	if _, err := c.finishAuthorized(f, rep, "przelew własny"); err != nil {
		return err
	}
	return nil
}

var taxGroups = []string{"FREQUENT", "OTHER", "TICKETS"}

func (c *Client) TransferTax(t TaxTransfer, confirm Confirmer) error {
	if t.From.Currency != "PLN" {
		return fmt.Errorf("przelew podatkowy wymaga rachunku w PLN (%s jest w %s)", t.From.Label(), t.From.Currency)
	}
	accType, accField, accLabel := "INDIVIDUAL", "individual_tax_account", "mikrorachunek"
	if t.TaxOffice {
		accType, accField, accLabel = "TAX_OFFICE", "tax_office_account", "rachunek urzędu"
	}
	period := t.Period.Type + " " + strings.Trim(strings.Join([]string{t.Period.Number, t.Period.Month, t.Period.Year}, "/"), "/")

	// Formularz otwieramy przed zgodą (to nic nie wysyła), żeby w podsumowaniu pokazać
	// identyfikator płatnika i grupę formularza, które podpowiada bank.
	f, rep, err := c.StartFlow("/ipko3/transactions/transfers/tax/create", nil)
	if err != nil {
		return fmt.Errorf("przelew podatkowy: %w", err)
	}
	if rep.StateID != "fill_form" {
		f.Cancel()
		return fmt.Errorf("przelew podatkowy: nieoczekiwany stan %q", safe(rep.StateID))
	}
	// Identyfikator płatnika (np. zamaskowany PESEL) bank podpowiada w formularzu i taki przyjmuje.
	payer, err := payerFromForm(parseFields(rep))
	if err != nil {
		f.Cancel()
		return fmt.Errorf("przelew podatkowy: %w", err)
	}
	// Grupa formularza (częste / pozostałe / mandaty) wynika z symbolu: szukamy jej w listach banku.
	group := ""
	for _, g := range taxGroups {
		rep, err = f.Step("partial", map[string]any{
			"amount": "", "payment_date": t.Date, "payment_type": t.PaymentType,
			"source_account": nil, "symbol": t.Symbol, "tax_type_group": g,
		})
		if err != nil {
			f.Cancel()
			return fmt.Errorf("przelew podatkowy: %w", err)
		}
		if sym, ok := parseFields(rep)["symbol"]; ok && sym.hasItem(t.Symbol) {
			group = g
			break
		}
	}
	if group == "" {
		f.Cancel()
		return fmt.Errorf("przelew podatkowy: bank nie zna symbolu formularza %q", t.Symbol)
	}
	payerType, _ := payer["type"].(string)
	payerID := ""
	for k, v := range payer {
		if s, _ := v.(string); k != "type" && s != "" {
			payerID = s
		}
	}
	summary := fmt.Sprintf(
		"PRZELEW PODATKOWY\n  z:        %s\n  symbol:   %s (grupa %s)\n  %-9s %s\n  kwota:    %s PLN\n  okres:    %s\n  płatnik:  %s %s (z formularza banku)\n  data:     %s, %s\n",
		t.From.Label(), t.Symbol, group, accLabel+":", formatNRB(t.Account), t.Amount, period, safe(payerType), safe(payerID), t.Date, t.PaymentType)
	if t.ObligationID != "" {
		summary += "  identyfikacja zobowiązania: " + safe(t.ObligationID) + "\n"
	}
	if err := confirm(summary); err != nil {
		f.Cancel()
		return err
	}
	data := map[string]any{
		"symbol":                 t.Symbol,
		"amount":                 t.Amount,
		"payer_identifier":       payer,
		"source_account":         t.From.ID,
		"payment_date":           t.Date,
		"payment_type":           t.PaymentType,
		"hold_create":            false,
		accField:                 t.Account,
		"recipient_account_type": accType,
		"tax_type_group":         group,
		"period":                 t.Period.asData(),
	}
	if t.ObligationID != "" {
		data["obligation_id"] = t.ObligationID
	}
	rep, err = f.Step("submit", data)
	if err != nil {
		return fmt.Errorf("przelew podatkowy: %w", err)
	}
	if rep.StateID == "fill_form" {
		return c.formRejected(f, rep, "przelew podatkowy")
	}
	c.bankSummary(rep, t.PaymentType)
	if _, err := c.finishAuthorized(f, rep, "przelew podatkowy"); err != nil {
		return err
	}
	return nil
}

func payerFromForm(fs map[string]formField) (map[string]any, error) {
	pf, ok := fs["payer_identifier"]
	if !ok {
		return nil, fmt.Errorf("brak identyfikatora płatnika w formularzu banku")
	}
	var v map[string]json.RawMessage
	if err := json.Unmarshal(pf.Value, &v); err != nil {
		return nil, fmt.Errorf("niepoprawny identyfikator płatnika w formularzu banku")
	}
	out := map[string]any{}
	for k, raw := range v {
		if k == "errors" {
			continue
		}
		var sub struct {
			Value *string `json:"value"`
		}
		if json.Unmarshal(raw, &sub) != nil {
			continue
		}
		if sub.Value != nil {
			out[k] = *sub.Value
		} else {
			out[k] = ""
		}
	}
	if t, _ := out["type"].(string); t == "" {
		return nil, fmt.Errorf("bank nie podał typu identyfikatora płatnika")
	}
	return out, nil
}
