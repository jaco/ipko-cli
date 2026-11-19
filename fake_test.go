package main

// Fałszywy serwer iPKO odtwarzający przebiegi nagrane w prawdziwej sesji
// (logowanie z IKO, przelew zwykły, własny, podatkowy, zestawienie, historia, wylogowanie).

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type fakeCall struct {
	Path, State, Action, FlowID string
	Data                        map[string]any
}

type fakeIPKO struct {
	t          *testing.T
	mu         sync.Mutex
	calls      []fakeCall
	seq        int
	polls      int
	authReady  map[string]bool
	rejectIKO  bool
	wrongPass  bool
	loginIKO   bool // bank żąda IKO w trakcie logowania (nowe urządzenie)
	lpGone     bool
	noIKO      bool   // bank w ogóle nie żąda IKO (np. zaufane urządzenie)
	redirectTo string // przekierowanie POST z hasłem
	dropFinal  bool   // zerwanie połączenia po ostatnim kroku przelewu
	wrongFlow  bool   // odpowiedź z innym flow_id // po zatwierdzeniu lp zwraca OBJECT_NOT_FOUND zamiast READY
	loggedOut  bool
}

func newFake(t *testing.T) (*fakeIPKO, *httptest.Server) {
	f := &fakeIPKO{t: t, authReady: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeIPKO) reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Session-Id", "SESSION-1")
	_ = json.NewEncoder(w).Encode(v)
}

func flowReply(state, flow string, resp any, finished bool) map[string]any {
	r := map[string]any{"response": resp, "httpStatus": 200, "state_id": state, "flow_id": flow, "token": "tok-" + flow + "-" + state}
	if finished {
		r["finished"] = true
	}
	return r
}

func authObj(id string) map[string]any {
	return map[string]any{"auth_method": "mobile_application", "approval_period": 120, "authorization_id": id,
		"device_name": "Urządzenie IOS", "session_id": "LP-" + id, "sync_request_period": 1}
}

func (f *fakeIPKO) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.t
	if r.Method == "GET" && r.URL.Path == "/" {
		http.SetCookie(w, &http.Cookie{Name: "TS010e943a", Value: "x"})
		w.Write([]byte("<html>login</html>"))
		return
	}
	body, _ := io.ReadAll(r.Body)
	if r.URL.Path == "/api/get_lp_hook_status" {
		var lp map[string]string
		json.Unmarshal(body, &lp)
		id := strings.TrimPrefix(lp["lp_key"], "auth_id:")
		if lp["lp_type"] != "AUTHORIZATION_STATUS" || lp["lp_id"] != "LP-"+id || lp["lp_value"] != "PENDING" {
			t.Errorf("lp hook: zły request %v", lp)
		}
		f.polls++
		if f.polls%2 == 1 { // pierwszy poll: jeszcze czeka
			f.reply(w, map[string]string{"lp_status": "LP_NOT_CHANGED", "lp_value": "PENDING"})
			return
		}
		f.authReady[id] = true
		if f.lpGone {
			f.reply(w, map[string]string{"lp_status": "LP_ERROR", "lp_value": "OBJECT_NOT_FOUND"})
			return
		}
		f.reply(w, map[string]string{"lp_status": "LP_CHANGED", "lp_value": "READY"})
		return
	}
	var env struct {
		Version int            `json:"version"`
		Seq     *int           `json:"seq"`
		FlowID  string         `json:"flow_id"`
		StateID string         `json:"state_id"`
		Token   string         `json:"token"`
		Action  string         `json:"action"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("%s: niepoprawny JSON", r.URL.Path)
	}
	if env.Version != 3 || env.Seq == nil || *env.Seq != f.seq {
		t.Errorf("%s: version=%d seq=%v, oczekiwane 3/%d", r.URL.Path, env.Version, env.Seq, f.seq)
	}
	f.seq++
	if r.Header.Get("X-Requested-With") != "XMLHttpRequest" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("ipko-locale") != "pl" {
		t.Errorf("%s: brak nagłówków ajax", r.URL.Path)
	}
	if env.FlowID != "" {
		if r.Header.Get("x-transaction-id") != env.FlowID {
			t.Errorf("%s: x-transaction-id=%q, flow=%q", r.URL.Path, r.Header.Get("x-transaction-id"), env.FlowID)
		}
		if env.Token != "tok-"+env.FlowID+"-"+env.StateID {
			t.Errorf("%s: zły token %q dla stanu %q", r.URL.Path, env.Token, env.StateID)
		}
	}
	afterLogin := r.URL.Path != "/ipko3/login"
	if afterLogin && (r.Header.Get("x-session-id") != "SESSION-1" || r.Header.Get("x-application-context") != "COMBINED") {
		t.Errorf("%s: brak x-session-id/x-application-context", r.URL.Path)
	}
	f.calls = append(f.calls, fakeCall{r.URL.Path, env.StateID, env.Action, env.FlowID, env.Data})

	needAuth := func(id string) bool {
		if !f.authReady[id] {
			t.Errorf("%s: submit przed zatwierdzeniem w IKO", r.URL.Path)
			return false
		}
		return true
	}
	if env.Action == "cancel" {
		f.reply(w, flowReply("CANCEL", env.FlowID, map[string]any{}, true))
		return
	}
	switch p := r.URL.Path; {
	case p == "/ipko3/login" && env.StateID == "login":
		if env.Data["login"] != "LOGIN" || len(fmt.Sprint(env.Data["fingerprint"])) != 32 {
			t.Errorf("login: dane %v", env.Data)
		}
		f.reply(w, flowReply("password", "L", map[string]any{"fields": map[string]any{}}, false))
	case p == "/ipko3/login" && env.StateID == "password":
		if f.redirectTo != "" {
			http.Redirect(w, r, f.redirectTo, http.StatusTemporaryRedirect)
			return
		}
		if f.wrongPass || env.Data["password"] != "PASS" {
			f.reply(w, flowReply("password", "L", map[string]any{"fields": map[string]any{"errors": map[string]any{"hint": "Nieprawidłowy login lub hasło."}}}, false))
			return
		}
		if f.loginIKO {
			f.reply(w, flowReply("mobile_application", "L", map[string]any{"auth": authObj("150")}, false))
			return
		}
		f.reply(w, flowReply("END", "L", map[string]any{"data": map[string]any{"login_type": "NORMAL"}}, true))
	case p == "/ipko3/login" && env.StateID == "mobile_application":
		if needAuth("150") {
			f.reply(w, flowReply("END", "L", map[string]any{"data": map[string]any{"login_type": "NORMAL"}}, true))
		}
	case p == "/ipko3/after-login/added-security":
		if f.loginIKO || f.noIKO {
			f.reply(w, map[string]any{"httpStatus": 200, "response": map[string]any{"data": map[string]any{"has_trusted_device": false, "ref_skip": map[string]any{}, "ref_skip_with_auth_verify": nil}}})
			return
		}
		f.reply(w, map[string]any{"httpStatus": 200, "response": map[string]any{"data": map[string]any{
			"has_trusted_device": false, "ref_skip": nil,
			"ref_skip_with_auth_verify": map[string]any{"data": map[string]any{"operation_type": "OT", "access_limiter_id": "AL"}}}}})
	case p == "/ipko3/auth-verify" && env.StateID == "":
		if env.Data["operation_type"] != "OT" || env.Data["access_limiter_id"] != "AL" {
			t.Errorf("auth-verify: dane %v", env.Data)
		}
		f.reply(w, flowReply("confirmation", "AV", map[string]any{"auth": authObj("100")}, false))
	case p == "/ipko3/auth-verify" && env.StateID == "confirmation":
		if needAuth("100") {
			f.reply(w, flowReply("END", "AV", map[string]any{}, true))
		}
	case p == "/ipko3/init":
		f.reply(w, map[string]any{"httpStatus": 200, "response": map[string]any{"data": map[string]any{"accounts": map[string]any{
			"ID-OSOB":  map[string]any{"balance": "1000.00", "ledger": "1000.00", "holds": "0.00", "currency": "PLN", "name": "PKO KONTO ZA ZERO", "nickname": nil, "is_business": false, "number": map[string]any{"value": "80102000000000111122223333"}},
			"ID-FIRMA": map[string]any{"balance": "25000.00", "ledger": "25000.00", "holds": "0.00", "currency": "PLN", "name": "RACHUNEK FIRMOWY", "nickname": nil, "is_business": true, "number": map[string]any{"value": "27102000000000444455556666"}},
		}}}})
	case p == "/ipko3/transactions/transfers/normal/shortcut":
		switch env.StateID {
		case "":
			f.reply(w, flowReply("fill_form", "N", map[string]any{"fields": map[string]any{}}, false))
		case "fill_form":
			want := map[string]any{"recipient_account": "47116000000000000012345678", "recipient_name": "Urzad Miasta", "hold_create": false,
				"money": map[string]any{"amount": "250.00", "currency": "PLN"}, "payment_date": "2026-11-19", "payment_type": "ELIXIR",
				"source_account": "ID-OSOB", "title": "Podatek od nieruchomosci IV rata"}
			assertJSON(t, "normal submit", env.Data, want)
			if f.wrongFlow {
				f.reply(w, flowReply("tan_state", "INNY", map[string]any{"auth": authObj("200")}, false))
				return
			}
			f.reply(w, flowReply("tan_state", "N", map[string]any{"auth": authObj("200"), "data": map[string]any{
				"recipient_bank": "MIL   Bankowy Punkt Rozliczeń nr 1", "transaction_fees": map[string]any{"ELIXIR": map[string]any{"amount": "0.00", "currency": "PLN"}}}}, false))
		case "tan_state":
			if !needAuth("200") {
				return
			}
			if f.dropFinal {
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
				return
			}
			if f.rejectIKO {
				f.reply(w, flowReply("tan_state", "N", map[string]any{"errors": map[string]any{"hint": "Dyspozycja odrzucona w aplikacji."}}, false))
				return
			}
			f.reply(w, flowReply("END", "N", map[string]any{"data": map[string]any{}}, true))
		}
	case p == "/ipko3/transactions/transfers/own":
		switch env.StateID {
		case "":
			f.reply(w, flowReply("transfer_0", "O", map[string]any{"fields": map[string]any{}}, false))
		case "transfer_0":
			assertJSON(t, "own submit", env.Data, map[string]any{"source_account": "ID-FIRMA", "destination_account": "ID-OSOB",
				"money": map[string]any{"amount": "150.00", "currency": "PLN"}, "payment_date": "2026-11-19", "title": "Przelew", "hold_create": false})
			f.reply(w, flowReply("confirmation_1", "O", map[string]any{"auth": authObj("300")}, false))
		case "confirmation_1":
			if needAuth("300") {
				f.reply(w, flowReply("END", "O", map[string]any{}, true))
			}
		}
	case p == "/ipko3/transactions/transfers/tax/create":
		switch {
		case env.StateID == "":
			f.reply(w, flowReply("fill_form", "T", map[string]any{"fields": map[string]any{
				"payer_identifier": map[string]any{"value": map[string]any{
					"id_card": map[string]any{"value": nil}, "passport": map[string]any{"value": nil}, "nip": map[string]any{"value": nil},
					"pesel": map[string]any{"value": "900******01"}, "regon": map[string]any{"value": nil}, "other_document": map[string]any{"value": nil},
					"type": map[string]any{"value": "PESEL"}, "errors": nil}},
			}}, false))
		case env.Action == "partial":
			items := []any{map[string]any{"id": ""}, map[string]any{"id": "CIT"}, map[string]any{"id": "PIT-5L"}}
			if env.Data["tax_type_group"] == "OTHER" {
				items = []any{map[string]any{"id": ""}, map[string]any{"id": "AKC"}, map[string]any{"id": "PPE"}}
			}
			f.reply(w, flowReply("fill_form", "T", map[string]any{"fields": map[string]any{"symbol": map[string]any{"widget": map[string]any{"items": items}, "value": env.Data["symbol"]}}}, false))
		case env.StateID == "fill_form" && env.Action == "submit":
			assertJSON(t, "tax submit", env.Data, map[string]any{"symbol": "PPE", "amount": "12345.00",
				"payer_identifier": map[string]any{"type": "PESEL", "id_card": "", "passport": "", "nip": "", "pesel": "900******01", "regon": "", "other_document": ""},
				"source_account":   "ID-FIRMA", "payment_date": "2026-11-19", "payment_type": "ELIXIR", "hold_create": false,
				"individual_tax_account": "95101000712222000000012345", "recipient_account_type": "INDIVIDUAL", "tax_type_group": "OTHER",
				"period": map[string]any{"type": "MONTH", "number": "", "month": "10", "year": "2026"}})
			f.reply(w, flowReply("confirmation", "T", map[string]any{"auth": authObj("400"), "data": map[string]any{"recipient_name": "Urząd Skarbowy Centrum Rozliczeniowe"}}, false))
		case env.StateID == "confirmation":
			if needAuth("400") {
				f.reply(w, flowReply("END", "T", map[string]any{}, true))
			}
		}
	case p == "/ipko3/accounts/operations/completed/download-preferences":
		if env.StateID == "" {
			if env.Data["date_from"] != "2026-08-01" || env.Data["source_account"] != "ID-FIRMA" {
				t.Errorf("statement start: %v", env.Data)
			}
			f.reply(w, flowReply("fill_form", "S", map[string]any{"fields": map[string]any{"document_type": map[string]any{
				"widget": map[string]any{"items": []any{map[string]any{"id": "pdf"}, map[string]any{"id": "csv"}}}}}}, false))
			return
		}
		assertJSON(t, "statement submit", env.Data, map[string]any{"document_type": "csv", "hide_balance": false})
		content := base64.StdEncoding.EncodeToString([]byte("data;kwota\n2026-10-02;-67.89\n"))
		f.reply(w, flowReply("END", "S", map[string]any{"data": map[string]any{"type": "text/csv", "name": "Zestawienie.csv",
			"content": map[string]any{"val": content[:10] + "\n" + content[10:], "enc": "base64", "fmt": "csv"}}}, true))
	case p == "/ipko3/accounts/operations/completed/full":
		action, _ := env.Data["action"].(map[string]any)
		page := map[string]any{"items": []any{
			map[string]any{"operation_id": "1002", "date": "2026-10-15", "title": "PRZELEW", "money": map[string]any{"amount": "-123.45", "currency": "PLN"}},
		}, "next": map[string]any{"type": "NEXT"}}
		if action["type"] == "NEXT" {
			page = map[string]any{"items": []any{
				map[string]any{"operation_id": "1001", "date": "2026-10-02", "title": "PRZELEW", "money": map[string]any{"amount": "-67.89", "currency": "PLN"}},
			}, "next": nil}
		}
		f.reply(w, map[string]any{"httpStatus": 200, "response": map[string]any{"case_id": "ITEMS", "data": page}})
	case p == "/ipko3/logout":
		if env.Data["reason"] != "CLIENT-LOGOUT" {
			t.Errorf("logout: %v", env.Data)
		}
		f.loggedOut = true
		f.reply(w, map[string]any{"httpStatus": 200, "response": map[string]any{}})
	default:
		t.Errorf("nieobsłużone: %s state=%s action=%s", p, env.StateID, env.Action)
		w.WriteHeader(404)
	}
}

func assertJSON(t *testing.T, what string, got, want any) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	var gv, wv any
	json.Unmarshal(g, &gv)
	json.Unmarshal(w, &wv)
	gs, _ := json.Marshal(gv)
	ws, _ := json.Marshal(wv)
	if string(gs) != string(ws) {
		t.Errorf("%s:\n got  %s\n want %s", what, gs, ws)
	}
}

func newTestClient(srv *httptest.Server) *Client {
	c := NewClient(srv.URL, "0123456789abcdef0123456789abcdef")
	c.out = io.Discard
	return c
}

func loginTest(t *testing.T, f *fakeIPKO, c *Client) []Account {
	t.Helper()
	if err := c.Login("LOGIN", "PASS"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	accts, err := c.Accounts()
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	return accts
}

func yesConfirm(string) error { return nil }

func TestLoginRequiresIKOAndLogout(t *testing.T) {
	f, srv := newFake(t)
	c := newTestClient(srv)
	accts := loginTest(t, f, c)
	if !f.authReady["100"] {
		t.Error("logowanie nie czekało na IKO")
	}
	if len(accts) != 2 || accts[0].ID != "ID-OSOB" || accts[1].ID != "ID-FIRMA" {
		t.Errorf("konta: %+v", accts)
	}
	if err := c.Logout(); err != nil || !f.loggedOut {
		t.Errorf("Logout: %v, loggedOut=%v", err, f.loggedOut)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	f, srv := newFake(t)
	f.wrongPass = true
	err := newTestClient(srv).Login("LOGIN", "PASS")
	if err == nil || !strings.Contains(err.Error(), "Nieprawidłowy login lub hasło") {
		t.Errorf("Login: %v", err)
	}
}

func TestTransferNormal(t *testing.T) {
	f, srv := newFake(t)
	c := newTestClient(srv)
	accts := loginTest(t, f, c)
	err := c.TransferNormal(NormalTransfer{From: &accts[0], To: "47116000000000000012345678", Name: "Urzad Miasta",
		Amount: "250.00", Title: "Podatek od nieruchomosci IV rata", Date: "2026-11-19", PaymentType: "ELIXIR"}, yesConfirm)
	if err != nil {
		t.Fatalf("TransferNormal: %v", err)
	}
}

func TestTransferNormalRejectedInIKO(t *testing.T) {
	f, srv := newFake(t)
	c := newTestClient(srv)
	accts := loginTest(t, f, c)
	f.rejectIKO = true
	err := c.TransferNormal(NormalTransfer{From: &accts[0], To: "47116000000000000012345678", Name: "Urzad Miasta",
		Amount: "250.00", Title: "Podatek od nieruchomosci IV rata", Date: "2026-11-19", PaymentType: "ELIXIR"}, yesConfirm)
	if err == nil || !strings.Contains(err.Error(), "odrzucona") {
		t.Errorf("oczekiwany błąd odrzucenia, jest: %v", err)
	}
}

func TestTransferNotConfirmedSendsNothing(t *testing.T) {
	f, srv := newFake(t)
	c := newTestClient(srv)
	accts := loginTest(t, f, c)
	before := len(f.calls)
	err := c.TransferNormal(NormalTransfer{From: &accts[0], To: "47116000000000000012345678", Name: "X", Amount: "1.00", Title: "t",
		Date: "2026-11-19", PaymentType: "ELIXIR"}, func(string) error { return fmt.Errorf("przerwano") })
	if err == nil || len(f.calls) != before {
		t.Errorf("bez zgody nic nie powinno pójść do banku: err=%v, zapytań=%d", err, len(f.calls)-before)
	}
}

func TestTransferOwn(t *testing.T) {
	f, srv := newFake(t)
	c := newTestClient(srv)
	accts := loginTest(t, f, c)
	if err := c.TransferOwn(OwnTransfer{From: &accts[1], To: &accts[0], Amount: "150.00", Title: "Przelew", Date: "2026-11-19"}, yesConfirm); err != nil {
		t.Fatalf("TransferOwn: %v", err)
	}
}

func TestTransferTaxFindsGroup(t *testing.T) {
	f, srv := newFake(t)
	c := newTestClient(srv)
	accts := loginTest(t, f, c)
	per, _ := parsePeriod("2026-10")
	err := c.TransferTax(TaxTransfer{From: &accts[1], Symbol: "PPE", Account: "95101000712222000000012345", Amount: "12345.00",
		Date: "2026-11-19", PaymentType: "ELIXIR", Period: per}, yesConfirm)
	if err != nil {
		t.Fatalf("TransferTax: %v", err)
	}
	var groups []any
	for _, call := range f.calls {
		if call.Action == "partial" {
			groups = append(groups, call.Data["tax_type_group"])
		}
	}
	if fmt.Sprint(groups) != "[FREQUENT OTHER]" {
		t.Errorf("kolejność grup: %v", groups)
	}
}

func TestStatement(t *testing.T) {
	f, srv := newFake(t)
	c := newTestClient(srv)
	accts := loginTest(t, f, c)
	name, data, err := c.Statement(&accts[1], "2026-08-01", "2026-08-31", "csv")
	if err != nil || name != "Zestawienie.csv" || !strings.Contains(string(data), "-67.89") {
		t.Fatalf("Statement: %q %q %v", name, data, err)
	}
	if _, _, err := c.Statement(&accts[1], "2026-08-01", "2026-08-31", "xls"); err == nil {
		t.Error("Statement: format spoza listy banku powinien dać błąd")
	}
}

func TestHistoryPaging(t *testing.T) {
	f, srv := newFake(t)
	c := newTestClient(srv)
	accts := loginTest(t, f, c)
	ops, err := c.History(&accts[1], "2026-08-01", "2026-11-19")
	if err != nil || len(ops) != 2 || ops[0].ID != "1002" || ops[1].ID != "1001" {
		t.Fatalf("History: %+v %v", ops, err)
	}
}

func TestLoginWithIKODuringLogin(t *testing.T) {
	f, srv := newFake(t)
	f.loginIKO = true
	c := newTestClient(srv)
	var out strings.Builder
	c.out = &out
	if err := c.Login("LOGIN", "PASS"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !f.authReady["150"] || f.authReady["100"] {
		t.Errorf("oczekiwane jedno potwierdzenie IKO w trakcie logowania: %v", f.authReady)
	}
	if strings.Contains(out.String(), "nie zażądał") {
		t.Errorf("niepotrzebne ostrzeżenie: %q", out.String())
	}
}

func TestLoginIKOObjectGoneAfterApproval(t *testing.T) {
	f, srv := newFake(t)
	f.loginIKO, f.lpGone = true, true
	c := newTestClient(srv)
	if err := c.Login("LOGIN", "PASS"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !c.loggedIn {
		t.Error("po zatwierdzeniu klient powinien być zalogowany")
	}
}

func normalTestTransfer(from *Account) NormalTransfer {
	return NormalTransfer{From: from, To: "47116000000000000012345678", Name: "Urzad Miasta",
		Amount: "250.00", Title: "Podatek od nieruchomosci IV rata", Date: "2026-11-19", PaymentType: "ELIXIR"}
}

func TestRedirectDoesNotLeakPassword(t *testing.T) {
	var leaked bool
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = true
	}))
	defer evil.Close()
	f, srv := newFake(t)
	f.redirectTo = evil.URL + "/steal"
	err := newTestClient(srv).Login("LOGIN", "PASS")
	if err == nil {
		t.Fatal("Login po przekierowaniu powinien się nie udać")
	}
	if leaked {
		t.Fatal("hasło poszło za przekierowaniem na obcy host")
	}
}

func TestLoginWithoutIKOIsRefused(t *testing.T) {
	f, srv := newFake(t)
	f.noIKO = true
	c := newTestClient(srv)
	err := c.Login("LOGIN", "PASS")
	if err == nil || !strings.Contains(err.Error(), "IKO") {
		t.Fatalf("logowanie bez IKO powinno zostać przerwane, jest: %v", err)
	}
	if err := c.Logout(); err != nil || !f.loggedOut {
		t.Errorf("po odmowie sesja powinna dać się wylogować: %v", err)
	}
}

func TestTransferLostResponseIsOutcomeUnknown(t *testing.T) {
	f, srv := newFake(t)
	c := newTestClient(srv)
	accts := loginTest(t, f, c)
	f.dropFinal = true
	err := c.TransferNormal(normalTestTransfer(&accts[0]), yesConfirm)
	if !errors.Is(err, errOutcomeUnknown) {
		t.Fatalf("utrata odpowiedzi po zatwierdzeniu = wynik nieznany, jest: %v", err)
	}
}

func TestTransferFlowMismatchFails(t *testing.T) {
	f, srv := newFake(t)
	c := newTestClient(srv)
	accts := loginTest(t, f, c)
	f.wrongFlow = true
	if err := c.TransferNormal(normalTestTransfer(&accts[0]), yesConfirm); err == nil {
		t.Fatal("odpowiedź z innym flow_id powinna przerwać przelew")
	}
	if f.authReady["200"] {
		t.Error("przy niespójnej odpowiedzi klient nie powinien czekać na IKO")
	}
}

func TestTaxSummaryShowsPayerBeforeConsent(t *testing.T) {
	f, srv := newFake(t)
	c := newTestClient(srv)
	accts := loginTest(t, f, c)
	per, _ := parsePeriod("2026-10")
	var summary string
	before := len(f.calls)
	err := c.TransferTax(TaxTransfer{From: &accts[1], Symbol: "PPE", Account: "95101000712222000000012345", Amount: "12345.00",
		Date: "2026-11-19", PaymentType: "ELIXIR", Period: per}, func(s string) error { summary = s; return errors.New("nie") })
	if err == nil || !strings.Contains(summary, "PESEL 900******01") || !strings.Contains(summary, "grupa OTHER") {
		t.Fatalf("podsumowanie: %q, err=%v", summary, err)
	}
	for _, call := range f.calls[before:] {
		if call.Action == "submit" {
			t.Fatal("bez zgody nie wolno wysłać formularza podatkowego")
		}
	}
	if last := f.calls[len(f.calls)-1]; last.Action != "cancel" {
		t.Errorf("po odmowie formularz powinien zostać anulowany, ostatnia akcja: %q", last.Action)
	}
}
