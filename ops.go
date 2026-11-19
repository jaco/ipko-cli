package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

type Operation struct {
	ID        string `json:"operation_id"`
	Date      string `json:"date"`
	Title     string `json:"title"`
	Type      string `json:"operation_type"`
	OtherSide string `json:"other_side"`
	IsHold    bool   `json:"is_hold"`
	Money     struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	} `json:"money"`
	EndingBalance *struct {
		Amount string `json:"amount"`
	} `json:"ending_balance"`
	Details struct {
		OtherAccount string `json:"other_account"`
	} `json:"details"`
	Raw json.RawMessage `json:"-"`
}

const maxHistoryPages = 500

// History pobiera zrealizowane operacje (wszystkie strony).
func (c *Client) History(acc *Account, from, to string) ([]Operation, error) {
	filter := map[string]any{"source_account": acc.ID}
	if from != "" || to != "" {
		filter = map[string]any{
			"amount_from": "", "amount_to": "", "date_from": from, "date_to": to,
			"operation_type": "ALL", "search_phrase": "", "source_account": acc.ID,
		}
	}
	action := json.RawMessage(`{"type":"SEARCH"}`)
	var out []Operation
	seen := map[string]bool{}
	for page := 0; page < maxHistoryPages; page++ {
		rep, err := c.Call("/ipko3/accounts/operations/completed/full", Request{Data: map[string]any{
			"action": action, "filter_form": filter,
			"choose_filters": false, "only_filters": false, "should_initialize": false,
		}})
		if err != nil {
			return out, fmt.Errorf("historia: %w", err)
		}
		var r struct {
			Data struct {
				Items []json.RawMessage `json:"items"`
				Next  json.RawMessage   `json:"next"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rep.Response, &r); err != nil {
			return out, fmt.Errorf("historia: niepoprawna odpowiedź")
		}
		if msgs := collectErrors(rep.Response); len(msgs) > 0 {
			return out, fmt.Errorf("historia: %s", strings.Join(msgs, "; "))
		}
		added := 0
		for _, raw := range r.Data.Items {
			var op Operation
			if err := json.Unmarshal(raw, &op); err != nil {
				return out, fmt.Errorf("historia: niepoprawna operacja: %w", err)
			}
			op.Raw = raw
			// Deduplikacja tylko po stabilnym identyfikatorze: pozycji bez ID nigdy nie zlewamy.
			if op.ID != "" {
				key := op.ID + "|" + op.Date + "|" + op.Money.Amount
				if seen[key] {
					continue
				}
				seen[key] = true
			}
			out = append(out, op)
			added++
		}
		if len(r.Data.Next) == 0 || string(r.Data.Next) == "null" {
			return out, nil
		}
		if added == 0 {
			// Jest "następna strona", ale nic nowego: nie udajemy, że lista jest kompletna.
			return out, fmt.Errorf("historia: stronicowanie banku się zapętliło; wynik może być niepełny")
		}
		action = r.Data.Next
	}
	return out, fmt.Errorf("historia: przekroczono limit %d stron", maxHistoryPages)
}

var statementFormats = []string{"pdf", "csv", "xls", "xml", "html"}

// Statement pobiera zestawienie operacji za okres; bank zwraca plik w base64 w JSON.
func (c *Client) Statement(acc *Account, from, to, format string) (name string, data []byte, err error) {
	f, rep, err := c.StartFlow("/ipko3/accounts/operations/completed/download-preferences", map[string]any{
		"search_phrase": "", "source_account": acc.ID, "amount_from": "", "amount_to": "",
		"operation_type": "ALL", "date_from": from, "date_to": to,
	})
	if err != nil {
		return "", nil, fmt.Errorf("zestawienie: %w", err)
	}
	if fs := parseFields(rep); len(fs) > 0 {
		if dt, ok := fs["document_type"]; ok && len(dt.Widget.Items) > 0 && !dt.hasItem(format) {
			f.Cancel()
			return "", nil, fmt.Errorf("zestawienie: bank nie oferuje formatu %q", format)
		}
	}
	rep, err = f.Step("submit", map[string]any{"document_type": format, "hide_balance": false})
	if err != nil {
		return "", nil, fmt.Errorf("zestawienie: %w", err)
	}
	if rep.StateID != "END" || !rep.Finished {
		f.Cancel()
		return "", nil, fmt.Errorf("zestawienie: bank nie zwrócił pliku (stan %q)%s", safe(rep.StateID), errorsSuffix(rep))
	}
	var r struct {
		Data struct {
			Name    string `json:"name"`
			Content struct {
				Val string `json:"val"`
				Enc string `json:"enc"`
				Fmt string `json:"fmt"`
			} `json:"content"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rep.Response, &r); err != nil {
		return "", nil, fmt.Errorf("zestawienie: niepoprawna odpowiedź")
	}
	if r.Data.Content.Enc != "base64" {
		return "", nil, fmt.Errorf("zestawienie: nieobsługiwane kodowanie %q", safe(r.Data.Content.Enc))
	}
	if r.Data.Content.Fmt != "" && !strings.EqualFold(r.Data.Content.Fmt, format) {
		return "", nil, fmt.Errorf("zestawienie: bank zwrócił format %q zamiast %q", safe(r.Data.Content.Fmt), format)
	}
	data, err = base64.StdEncoding.DecodeString(strings.Join(strings.Fields(r.Data.Content.Val), ""))
	if err != nil {
		return "", nil, fmt.Errorf("zestawienie: błędny base64: %w", err)
	}
	if len(data) == 0 {
		return "", nil, fmt.Errorf("zestawienie: bank zwrócił pusty plik")
	}
	return r.Data.Name, data, nil
}
