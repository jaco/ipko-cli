package main

// Klient protokołu iPKO ("ipko3"): każde zapytanie to POST z kopertą JSON
// {version, seq, location, [flow_id, state_id, token, action], data}.
// Wieloetapowe operacje (logowanie, przelewy, zestawienia) to "flow":
// serwer zwraca flow_id, state_id i token, które trzeba odesłać w kolejnym kroku.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	bankBase  = "https://www.ipko.pl" // celowo stały: dane logowania trafiają wyłącznie tutaj
	userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/158.0.0.0 Safari/537.36"
	secChUA   = `"Chromium";v="158", "Not.A/Brand";v="24", "Google Chrome";v="158"`

	maxResponseBytes = 64 << 20 // zestawienia w base64 bywają duże; nic większego nie wczytujemy
	requestTimeout   = 90 * time.Second
)

type Client struct {
	base        *url.URL
	hc          *http.Client
	seq         int
	sessionID   string
	loggedIn    bool
	fingerprint string
	debug       io.Writer // nil = brak logowania; nigdy nie loguje treści zapytań ani odpowiedzi
	out         io.Writer // komunikaty dla użytkownika (stderr)
}

// NewClient: w programie zawsze z bankBase; inny adres przyjmują tylko testy (fałszywy serwer).
func NewClient(base, fingerprint string) *Client {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		panic(err)
	}
	jar, _ := cookiejar.New(nil)
	c := &Client{base: u, fingerprint: fingerprint, out: os.Stderr}
	c.hc = &http.Client{
		Jar:     jar,
		Timeout: requestTimeout,
		// Bez przekierowań poza adres banku: ciało POST (hasło, przelew) i nagłówki
		// sesji nie mogą trafić gdzie indziej. Zapytania API nie są przekierowywane wcale.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 || req.Method != http.MethodGet || !c.sameOrigin(req.URL) {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	return c
}

func (c *Client) sameOrigin(u *url.URL) bool {
	return u.Scheme == c.base.Scheme && u.Host == c.base.Host
}

func (c *Client) origin() string { return c.base.Scheme + "://" + c.base.Host }

func (c *Client) logf(format string, args ...any) {
	if c.debug != nil {
		fmt.Fprintf(c.debug, "[ipko] "+format+"\n", args...)
	}
}

func (c *Client) referer() string {
	if c.loggedIn {
		return c.origin() + "/index.html"
	}
	return c.origin() + "/"
}

// transportError: zapytanie mogło dotrzeć do banku, ale nie mamy czytelnej odpowiedzi.
type transportError struct{ err error }

func (e *transportError) Error() string { return e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

func transportErrorf(format string, args ...any) error {
	return &transportError{fmt.Errorf(format, args...)}
}

// do wysyła zapytanie HTTP z nagłówkami jak przeglądarka i zapamiętuje x-session-id.
func (c *Client) do(ctx context.Context, method, path string, body []byte, hdr map[string]string) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.origin()+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", "pl-PL,pl;q=0.9,en-US;q=0.8,en;q=0.7")
	req.Header.Set("sec-ch-ua", secChUA)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"macOS"`)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, transportErrorf("%s %s: błąd połączenia: %v", method, path, err)
	}
	defer resp.Body.Close()
	if !c.sameOrigin(resp.Request.URL) {
		return resp.StatusCode, nil, transportErrorf("%s %s: odpowiedź spoza adresu banku", method, path)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return resp.StatusCode, nil, transportErrorf("%s %s: przerwany odczyt odpowiedzi: %v", method, path, err)
	}
	if len(b) > maxResponseBytes {
		return resp.StatusCode, nil, transportErrorf("%s %s: odpowiedź przekracza %d MB", method, path, maxResponseBytes>>20)
	}
	if sid := resp.Header.Get("X-Session-Id"); sid != "" {
		c.sessionID = sid
	}
	return resp.StatusCode, b, nil
}

type Request struct {
	FlowID, StateID, Token, Action string
	Data                           any // nil = bez pola "data"
}

type Reply struct {
	Response   json.RawMessage `json:"response"`
	HTTPStatus int             `json:"httpStatus"`
	StateID    string          `json:"state_id"`
	FlowID     string          `json:"flow_id"`
	Token      string          `json:"token"`
	Finished   bool            `json:"finished"`
}

// Call wysyła jedną kopertę ipko3.
func (c *Client) Call(path string, r Request) (*Reply, error) {
	env := map[string]any{"version": 3, "seq": c.seq, "location": ""}
	c.seq++
	if r.FlowID != "" {
		env["flow_id"] = r.FlowID
	}
	if r.StateID != "" {
		env["state_id"] = r.StateID
	}
	if r.Token != "" {
		env["token"] = r.Token
	}
	if r.Action != "" {
		env["action"] = r.Action
	}
	if r.Data != nil {
		env["data"] = r.Data
	}
	body, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	hdr := map[string]string{
		"Accept":           "application/json",
		"Content-Type":     "application/json",
		"X-Requested-With": "XMLHttpRequest",
		"ipko-locale":      "pl",
		"Origin":           c.origin(),
		"Referer":          c.referer(),
	}
	if c.sessionID != "" {
		hdr["x-session-id"] = c.sessionID
	}
	if c.loggedIn {
		hdr["x-application-context"] = "COMBINED"
	}
	if r.FlowID != "" {
		hdr["x-transaction-id"] = r.FlowID
	}
	c.logf("-> %s state=%s action=%s", path, r.StateID, r.Action)
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	status, b, err := c.do(ctx, "POST", path, body, hdr)
	if err != nil {
		return nil, err
	}
	// Nigdy nie wklejamy treści odpowiedzi do komunikatów: mogą w niej być tokeny i dane kont.
	if status != http.StatusOK {
		return nil, transportErrorf("%s: HTTP %d", path, status)
	}
	var rep Reply
	if err := json.Unmarshal(b, &rep); err != nil {
		return nil, transportErrorf("%s: niepoprawny JSON w odpowiedzi (%d B)", path, len(b))
	}
	c.logf("<- %s httpStatus=%d state=%s finished=%v", path, rep.HTTPStatus, rep.StateID, rep.Finished)
	if rep.HTTPStatus != http.StatusOK {
		msg := strings.Join(collectErrors(rep.Response), "; ")
		if msg == "" {
			msg = "brak opisu"
		}
		return &rep, fmt.Errorf("%s: błąd banku (httpStatus %d): %s", path, rep.HTTPStatus, msg)
	}
	return &rep, nil
}

// collectErrors zbiera komunikaty z pól "errors" (hint/description/title) w odpowiedzi.
func collectErrors(raw json.RawMessage) []string {
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(path, msg string) {
		msg = strings.TrimSpace(msg)
		if msg == "" {
			return
		}
		if path != "" {
			msg = path + ": " + msg
		}
		msg = safe(msg)
		if !seen[msg] {
			seen[msg] = true
			out = append(out, msg)
		}
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch t := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				val := t[k]
				if k == "errors" && val != nil {
					errPath := strings.TrimSuffix(strings.TrimPrefix(path, "fields."), ".")
					switch e := val.(type) {
					case map[string]any:
						found := false
						for _, f := range []string{"hint", "description", "title", "message"} {
							if s, ok := e[f].(string); ok && strings.TrimSpace(s) != "" {
								add(errPath, s)
								found = true
								break
							}
						}
						if !found {
							walk(path+k+".", e)
						}
					case []any:
						for _, x := range e {
							if s, ok := x.(string); ok {
								add(errPath, s)
							} else {
								walk(path+k+".", x)
							}
						}
					case string:
						add(errPath, e)
					}
					continue
				}
				walk(path+k+".", val)
			}
		case []any:
			for _, x := range t {
				walk(path, x)
			}
		}
	}
	walk("", v)
	return out
}

func errorsSuffix(rep *Reply) string {
	if rep == nil {
		return ""
	}
	if msgs := collectErrors(rep.Response); len(msgs) > 0 {
		return ": " + strings.Join(msgs, "; ")
	}
	return ""
}

// Flow to wieloetapowa operacja po stronie banku.
type Flow struct {
	c                *Client
	path             string
	id, state, token string
}

func (c *Client) StartFlow(path string, data any) (*Flow, *Reply, error) {
	rep, err := c.Call(path, Request{Data: data})
	if err != nil {
		return nil, rep, err
	}
	if rep.FlowID == "" || rep.StateID == "" || rep.Token == "" {
		return nil, rep, fmt.Errorf("%s: bank nie rozpoczął operacji%s", path, errorsSuffix(rep))
	}
	return &Flow{c: c, path: path, id: rep.FlowID, state: rep.StateID, token: rep.Token}, rep, nil
}

func (f *Flow) Step(action string, data any) (*Reply, error) {
	rep, err := f.c.Call(f.path, Request{FlowID: f.id, StateID: f.state, Token: f.token, Action: action, Data: data})
	if err != nil {
		return rep, err
	}
	// Odpowiedź musi dotyczyć tej samej operacji i nieść komplet danych do kolejnego kroku.
	if rep.FlowID != f.id {
		return rep, transportErrorf("%s: odpowiedź dotyczy innej operacji", f.path)
	}
	if rep.StateID == "" || (!rep.Finished && rep.Token == "") {
		return rep, transportErrorf("%s: niekompletna odpowiedź (brak stanu lub tokenu)", f.path)
	}
	f.state = rep.StateID
	if rep.Token != "" {
		f.token = rep.Token
	}
	return rep, nil
}

// Cancel przerywa operację (np. gdy użytkownik nie potwierdził przelewu albo coś poszło nie tak).
func (f *Flow) Cancel() {
	rep, err := f.Step("cancel", map[string]any{})
	switch {
	case err != nil:
		f.c.logf("anulowanie %s: %v", f.path, err)
	case rep.StateID != "CANCEL":
		f.c.logf("anulowanie %s: stan %q", f.path, rep.StateID)
	}
}

// ---------- autoryzacja w aplikacji IKO ----------

type AuthInfo struct {
	Method            string `json:"auth_method"`
	ApprovalPeriod    int    `json:"approval_period"`
	AuthorizationID   string `json:"authorization_id"`
	DeviceName        string `json:"device_name"`
	SessionID         string `json:"session_id"`
	SyncRequestPeriod int    `json:"sync_request_period"`
}

func parseAuth(rep *Reply) (*AuthInfo, error) {
	var r struct {
		Auth *AuthInfo `json:"auth"`
	}
	if err := json.Unmarshal(rep.Response, &r); err != nil || r.Auth == nil {
		return nil, fmt.Errorf("bank nie zażądał autoryzacji (stan %q, pola odpowiedzi: %s)%s", rep.StateID, responseKeys(rep), errorsSuffix(rep))
	}
	if r.Auth.AuthorizationID == "" || r.Auth.SessionID == "" {
		return nil, fmt.Errorf("niekompletne dane autoryzacji IKO (stan %q)", rep.StateID)
	}
	return r.Auth, nil
}

func hasAuth(rep *Reply) bool {
	_, err := parseAuth(rep)
	return err == nil
}

// responseKeys zwraca same nazwy pól odpowiedzi (bez wartości) do diagnostyki.
func responseKeys(rep *Reply) string {
	var m map[string]json.RawMessage
	if rep == nil || json.Unmarshal(rep.Response, &m) != nil {
		return "-"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, safe(k))
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

var (
	errAuthTimeout = errors.New("minął czas na zatwierdzenie w IKO")
	// errAuthGone: obiekt autoryzacji zniknął z kolejki powiadomień (np. już obsłużony).
	// Wynik i tak rozstrzyga bank w kolejnym kroku operacji, więc wtedy go wysyłamy.
	errAuthGone = errors.New("autoryzacja IKO: OBJECT_NOT_FOUND")
	// errOutcomeUnknown: dyspozycja mogła zostać przyjęta, ale odpowiedź się nie dotarła.
	errOutcomeUnknown = errors.New("WYNIK NIEZNANY: bank mógł przyjąć dyspozycję; sprawdź historię rachunku przed ponowieniem")
)

func clampDuration(d, lo, hi time.Duration) time.Duration {
	return max(lo, min(d, hi))
}

// WaitMobileAuth czeka (long-poll), aż użytkownik zareaguje w aplikacji IKO.
// READY oznacza tylko, że stan autoryzacji się zmienił: o wyniku (zatwierdzono
// czy odrzucono) decyduje odpowiedź na kolejny krok operacji.
func (c *Client) WaitMobileAuth(a *AuthInfo, what string) error {
	if a.Method != "mobile_application" {
		return fmt.Errorf("nieobsługiwana metoda autoryzacji %q (obsługiwana jest tylko aplikacja IKO)", safe(a.Method))
	}
	period := clampDuration(time.Duration(a.ApprovalPeriod)*time.Second, 30*time.Second, 5*time.Minute)
	if a.ApprovalPeriod <= 0 {
		period = 2 * time.Minute
	}
	device := safe(a.DeviceName)
	if device == "" {
		device = "telefon"
	}
	fmt.Fprintf(c.out, "Zatwierdź %s w aplikacji IKO (%s), masz %d s...\n", what, device, int(period.Seconds()))
	interval := clampDuration(time.Duration(a.SyncRequestPeriod)*time.Millisecond, time.Millisecond, 5*time.Second)
	if a.SyncRequestPeriod <= 0 {
		interval = time.Second
	}
	body, _ := json.Marshal(map[string]string{
		"lp_type":  "AUTHORIZATION_STATUS",
		"lp_id":    a.SessionID,
		"lp_key":   "auth_id:" + a.AuthorizationID,
		"lp_value": "PENDING",
	})
	hdr := map[string]string{
		"Accept":       "application/json",
		"Content-Type": "application/json",
		"Origin":       c.origin(),
		"Referer":      c.referer(),
	}
	started := time.Now()
	// Jeden twardy termin na całe oczekiwanie: obejmuje i zapytania, i przerwy.
	ctx, cancel := context.WithDeadline(context.Background(), started.Add(period+15*time.Second))
	defer cancel()
	for {
		status, b, err := c.do(ctx, "POST", "/api/get_lp_hook_status", body, hdr)
		if ctx.Err() != nil {
			return errAuthTimeout
		}
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("status autoryzacji: HTTP %d", status)
		}
		var st struct {
			LPStatus string `json:"lp_status"`
			LPValue  string `json:"lp_value"`
			Status   string `json:"status"`
			Value    string `json:"value"`
		}
		if err := json.Unmarshal(b, &st); err != nil {
			return fmt.Errorf("status autoryzacji: niepoprawny JSON")
		}
		lpStatus, lpValue := st.LPStatus, st.LPValue
		if lpStatus == "" {
			lpStatus = st.Status
		}
		if lpValue == "" {
			lpValue = st.Value
		}
		c.logf("auth poll +%ds: %s %s", int(time.Since(started).Seconds()), safe(lpStatus), safe(lpValue))
		switch {
		case lpStatus == "LP_CHANGED" && lpValue == "READY":
			return nil
		case lpStatus == "LP_ERROR" && lpValue == "OBJECT_NOT_FOUND":
			return errAuthGone
		case lpStatus == "LP_NOT_CHANGED":
			select {
			case <-ctx.Done():
				return errAuthTimeout
			case <-time.After(interval):
			}
		default:
			return fmt.Errorf("autoryzacja IKO: nieoczekiwany status %s %s", safe(lpStatus), safe(lpValue))
		}
	}
}

// finishAuthorized czeka na IKO i wysyła ostatni krok operacji.
// Sukces wyłącznie przy stanie END, finished=true i braku błędów w odpowiedzi.
func (c *Client) finishAuthorized(f *Flow, rep *Reply, what string) (*Reply, error) {
	auth, err := parseAuth(rep)
	if err != nil {
		f.Cancel()
		return nil, err
	}
	if err := c.WaitMobileAuth(auth, what); err != nil {
		if !errors.Is(err, errAuthGone) {
			f.Cancel()
			return nil, err
		}
		c.logf("autoryzacja zniknęła z kolejki; pytam bank o wynik")
	}
	final, err := f.Step("submit", map[string]any{})
	if err != nil {
		var te *transportError
		if errors.As(err, &te) {
			return final, fmt.Errorf("%w (%s: %v)", errOutcomeUnknown, what, err)
		}
		return final, err
	}
	if final.StateID != "END" || !final.Finished {
		if !final.Finished {
			f.Cancel()
		}
		return final, fmt.Errorf("%s nie został zatwierdzony (stan %q)%s", what, safe(final.StateID), errorsSuffix(final))
	}
	if msgs := collectErrors(final.Response); len(msgs) > 0 {
		return final, fmt.Errorf("%w (%s: bank zakończył operację z komunikatem: %s)", errOutcomeUnknown, what, strings.Join(msgs, "; "))
	}
	return final, nil
}

// ---------- formularze ----------

type formField struct {
	Widget struct {
		Items []json.RawMessage `json:"items"`
	} `json:"widget"`
	Value  json.RawMessage `json:"value"`
	Errors json.RawMessage `json:"errors"`
}

func parseFields(rep *Reply) map[string]formField {
	var r struct {
		Fields map[string]json.RawMessage `json:"fields"`
	}
	out := map[string]formField{}
	if rep == nil || json.Unmarshal(rep.Response, &r) != nil {
		return out
	}
	for k, raw := range r.Fields {
		var f formField
		if json.Unmarshal(raw, &f) == nil {
			out[k] = f
		}
	}
	return out
}

func (f formField) hasItem(id string) bool {
	for _, raw := range f.Widget.Items {
		var obj struct {
			ID string `json:"id"`
		}
		var s string
		if json.Unmarshal(raw, &obj) == nil && obj.ID == id {
			return true
		}
		if json.Unmarshal(raw, &s) == nil && s == id {
			return true
		}
	}
	return false
}
