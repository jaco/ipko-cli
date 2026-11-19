package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// Login: login -> hasło -> (brak zaufanego urządzenia) weryfikacja w IKO.
// Celowo nigdy nie dodajemy zaufanego urządzenia: każde logowanie = zatwierdzenie w IKO.
func (c *Client) Login(login, password string) error {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	status, _, err := c.do(ctx, "GET", "/", nil, map[string]string{
		"Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
	})
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("strona logowania: HTTP %d", status)
	}

	rep, err := c.Call("/ipko3/login", Request{
		StateID: "login",
		Action:  "submit",
		Data:    map[string]any{"login": login, "fingerprint": c.fingerprint},
	})
	if err != nil {
		return fmt.Errorf("logowanie: %w", err)
	}
	if rep.StateID != "password" {
		return fmt.Errorf("logowanie: bank nie poprosił o hasło (stan %q)%s", rep.StateID, errorsSuffix(rep))
	}

	lf := &Flow{c: c, path: "/ipko3/login", id: rep.FlowID, state: rep.StateID, token: rep.Token}
	rep, err = lf.Step("submit", map[string]any{"password": password})
	if err != nil {
		return fmt.Errorf("logowanie: %w", err)
	}
	confirmedInIKO := false
	if rep.StateID != "END" && hasAuth(rep) {
		// Dla nowego "urządzenia" bank żąda potwierdzenia w IKO już w trakcie logowania
		// (stan "mobile_application"); przeglądarka ze znanym odciskiem dostaje je dopiero po logowaniu.
		if rep, err = c.finishAuthorized(lf, rep, "logowanie"); err != nil {
			return fmt.Errorf("logowanie: %w", err)
		}
		confirmedInIKO = true
	}
	if rep.StateID != "END" || !rep.Finished {
		return fmt.Errorf("logowanie nieudane (stan %q, pola odpowiedzi: %s)%s", rep.StateID, responseKeys(rep), errorsSuffix(rep))
	}
	c.loggedIn = true

	rep, err = c.Call("/ipko3/after-login/added-security", Request{Data: map[string]any{}})
	if err != nil {
		return fmt.Errorf("weryfikacja po logowaniu: %w", err)
	}
	var sec struct {
		Data struct {
			HasTrustedDevice bool `json:"has_trusted_device"`
			SkipWithVerify   *struct {
				Data map[string]any `json:"data"`
			} `json:"ref_skip_with_auth_verify"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rep.Response, &sec); err != nil {
		return fmt.Errorf("weryfikacja po logowaniu: niepoprawna odpowiedź")
	}
	if sec.Data.SkipWithVerify == nil || sec.Data.SkipWithVerify.Data == nil {
		if !confirmedInIKO {
			// Zasada "jednorazowego dostępu": bez zatwierdzenia w IKO nie pracujemy w tej sesji
			// (np. gdy ktoś dodał to urządzenie jako zaufane). Wywołujący wyloguje sesję.
			return fmt.Errorf("logowanie przerwane: bank nie zażądał potwierdzenia w IKO (zaufane urządzenie?); ipko wymaga IKO przy każdym logowaniu")
		}
		return nil
	}
	f, rep, err := c.StartFlow("/ipko3/auth-verify", sec.Data.SkipWithVerify.Data)
	if err != nil {
		return fmt.Errorf("potwierdzenie logowania: %w", err)
	}
	if _, err := c.finishAuthorized(f, rep, "logowanie"); err != nil {
		return fmt.Errorf("potwierdzenie logowania: %w", err)
	}
	return nil
}

func (c *Client) Logout() error {
	if !c.loggedIn {
		return nil
	}
	_, err := c.Call("/ipko3/logout", Request{Data: map[string]any{"reason": "CLIENT-LOGOUT"}})
	c.loggedIn = false
	return err
}

type Account struct {
	ID       string // identyfikator sesyjny (zmienia się między sesjami)
	Name     string
	Nickname string
	Number   string // NRB, 26 cyfr
	Currency string
	Balance  string // dostępne środki
	Ledger   string // saldo księgowe
	Holds    string // blokady
	Business bool
}

func (a *Account) Label() string {
	n := a.Name
	if a.Nickname != "" {
		n = a.Nickname
	}
	return fmt.Sprintf("%s (%s)", safe(n), formatNRB(safe(a.Number)))
}

func (c *Client) Accounts() ([]Account, error) {
	rep, err := c.Call("/ipko3/init", Request{Data: map[string]any{"accounts": map[string]any{}}})
	if err != nil {
		return nil, fmt.Errorf("lista kont: %w", err)
	}
	var r struct {
		Data struct {
			Accounts map[string]struct {
				Balance    string  `json:"balance"`
				Ledger     string  `json:"ledger"`
				Holds      string  `json:"holds"`
				Currency   string  `json:"currency"`
				Name       string  `json:"name"`
				Nickname   *string `json:"nickname"`
				IsBusiness bool    `json:"is_business"`
				Number     struct {
					Value string `json:"value"`
				} `json:"number"`
			} `json:"accounts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rep.Response, &r); err != nil {
		return nil, fmt.Errorf("lista kont: niepoprawna odpowiedź: %w", err)
	}
	var out []Account
	for id, a := range r.Data.Accounts {
		acc := Account{
			ID: id, Name: a.Name, Number: a.Number.Value, Currency: a.Currency,
			Balance: a.Balance, Ledger: a.Ledger, Holds: a.Holds, Business: a.IsBusiness,
		}
		if a.Nickname != nil {
			acc.Nickname = *a.Nickname
		}
		out = append(out, acc)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("lista kont: bank nie zwrócił żadnego konta")
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Business != out[j].Business {
			return !out[i].Business
		}
		return out[i].Number < out[j].Number
	})
	return out, nil
}
