package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const usage = `ipko — klient wiersza poleceń do iPKO (PKO BP)

Użycie:
  ipko accounts
  ipko history   KONTO [--from RRRR-MM-DD] [--to RRRR-MM-DD] [--json]
  ipko statement KONTO --from RRRR-MM-DD --to RRRR-MM-DD [--format pdf|csv|xls|xml|html] [-o PLIK] [--force]
  ipko recipients [--json]
  ipko transfer  --from KONTO --to NRB --name ODBIORCA --amount KWOTA --title TYTUŁ
                 [--address ADRES] [--date RRRR-MM-DD] [--express | --sorbnet] [--yes]
  ipko transfer  --recipient ODBIORCA --amount KWOTA [--title TYTUŁ] [--from KONTO]
                 [--date RRRR-MM-DD] [--express | --sorbnet] [--yes]
  ipko own       --from KONTO --to KONTO --amount KWOTA [--title TYTUŁ] [--date RRRR-MM-DD] [--yes]
  ipko tax       --from KONTO --symbol SYMBOL --account NRB --amount KWOTA --period OKRES
                 [--tax-office] [--obligation ID] [--date RRRR-MM-DD] [--express] [--yes]

KONTO   końcówka numeru (min. 4 cyfry), pełny numer albo fragment nazwy rachunku
ODBIORCA  z książki odbiorców: Twoja nazwa, nazwa odbiorcy albo końcówka jego numeru
KWOTA   np. 250 albo 1234,56
OKRES   RRRR-MM (miesiąc), RRRR-Qn (kwartał), RRRR-Hn (półrocze), RRRR (rok),
        RRRR-MM-Dn (dekada), RRRR-MM-DD (dzień)
--account     mikrorachunek podatkowy; z --tax-office: rachunek urzędu

Każde uruchomienie loguje się od nowa i wymaga zatwierdzenia logowania w IKO,
przelew wymaga osobnego zatwierdzenia w IKO; na końcu sesja jest wylogowywana.
Login i hasło: pęk kluczy macOS, usługa "ipko-cli":
  security add-generic-password -s ipko-cli -a LOGIN -w
IPKO_DEBUG=1 wypisuje przebieg zapytań (bez danych) na stderr.
`

func main() { os.Exit(run(os.Args[1:])) }

// probeHook: tylko w buildzie z tagiem "probe" (narzędzia deweloperskie, poza zwykłą binarką).
var probeHook func(c *Client, accts []Account) error

func run(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(usage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	cmds := map[string]func([]string) error{
		"accounts":   cmdAccounts,
		"history":    cmdHistory,
		"statement":  cmdStatement,
		"transfer":   cmdTransfer,
		"recipients": cmdRecipients,
		"own":        cmdOwn,
		"tax":        cmdTax,
	}
	if probeHook != nil {
		cmds["probe-recipients"] = func([]string) error { return withSession(probeHook) }
	}
	f, ok := cmds[args[0]]
	if !ok {
		fmt.Fprintf(os.Stderr, "ipko: nieznana komenda %q\n\n%s", args[0], usage)
		return 2
	}
	if err := f(args[1:]); err != nil {
		var ue usageError
		fmt.Fprintln(os.Stderr, "ipko:", err)
		switch {
		case errors.As(err, &ue):
			return 2
		case errors.Is(err, errOutcomeUnknown):
			return 3 // nie ponawiać automatycznie: dyspozycja mogła zostać przyjęta
		}
		return 1
	}
	return 0
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error { return usageError{fmt.Sprintf(format, args...)} }

// parseArgs pozwala mieszać argumenty pozycyjne z flagami (np. "history 6666 --from ...").
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usagef("%s: %v", fs.Name(), err)
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// withSession: logowanie (z IKO), praca, zawsze wylogowanie.
func withSession(fn func(c *Client, accts []Account) error) (err error) {
	// Szczegółowe śledzenie HTTP/2 w Go wypisuje nagłówki i fragmenty ramek, czyli sesję i hasło.
	if strings.Contains(os.Getenv("GODEBUG"), "http2debug") {
		return errors.New("GODEBUG zawiera http2debug: to wypisałoby dane logowania; usuń tę zmienną")
	}
	login, password, err := keychainCredentials(envOr("IPKO_KEYCHAIN_SERVICE", "ipko-cli"))
	if err != nil {
		return err
	}
	fp, err := loadFingerprint()
	if err != nil {
		return fmt.Errorf("identyfikator instalacji: %w", err)
	}
	c := NewClient(bankBase, fp)
	if os.Getenv("IPKO_DEBUG") == "1" {
		c.debug = os.Stderr
	}
	defer func() {
		// Błąd wylogowania nigdy nie zmienia wyniku komendy (np. przyjętego przelewu):
		// inaczej skrypt mógłby ponowić dyspozycję. Sesja i tak wygasa po stronie banku.
		if lerr := c.Logout(); lerr != nil {
			fmt.Fprintf(os.Stderr, "ipko: ostrzeżenie: wylogowanie nie powiodło się (%v); sesja wygaśnie po kilku minutach\n", lerr)
		}
	}()
	if err := c.Login(login, password); err != nil {
		return err
	}
	accts, err := c.Accounts()
	if err != nil {
		return err
	}
	return fn(c, accts)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func cmdAccounts(args []string) error {
	fs := flag.NewFlagSet("accounts", flag.ContinueOnError)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("accounts: nieoczekiwany argument %q", pos[0])
	}
	return withSession(func(c *Client, accts []Account) error {
		for _, a := range accts {
			name := a.Name
			if a.Nickname != "" {
				name = a.Nickname
			}
			fmt.Printf("%s\t%s\t%s %s\t(księgowe %s, blokady %s)\n", formatNRB(a.Number), safe(name), safe(a.Balance), safe(a.Currency), safe(a.Ledger), safe(a.Holds))
		}
		return nil
	})
}

func cmdHistory(args []string) error {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	from := fs.String("from", "", "")
	to := fs.String("to", "", "")
	asJSON := fs.Bool("json", false, "")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("history: podaj dokładnie jedno konto")
	}
	if err := checkDates(*from, *to, false); err != nil {
		return err
	}
	return withSession(func(c *Client, accts []Account) error {
		acc, err := resolveAccount(accts, pos[0])
		if err != nil {
			return err
		}
		ops, err := c.History(acc, *from, *to)
		if err != nil {
			return err
		}
		if *asJSON {
			raws := make([]json.RawMessage, len(ops))
			for i, op := range ops {
				raws[i] = op.Raw
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(raws)
		}
		for _, op := range ops {
			bal := ""
			if op.EndingBalance != nil {
				bal = op.EndingBalance.Amount
			}
			fmt.Printf("%s\t%s %s\t%s\t%s\t%s\t%s\t%s\n", oneLine(op.Date), oneLine(op.Money.Amount), oneLine(op.Money.Currency),
				oneLine(op.OtherSide), oneLine(op.Title), oneLine(formatNRB(op.Details.OtherAccount)), oneLine(op.Type), oneLine(bal))
		}
		return nil
	})
}

// oneLine: jedna linia TSV, bez znaków sterujących z danych banku.
func oneLine(s string) string { return safe(strings.Join(strings.Fields(s), " ")) }

func checkDates(from, to string, required bool) error {
	if required && (from == "" || to == "") {
		return usagef("podaj --from i --to")
	}
	if from != "" {
		if err := checkDate(from, "--from"); err != nil {
			return usageError{err.Error()}
		}
	}
	if to != "" {
		if err := checkDate(to, "--to"); err != nil {
			return usageError{err.Error()}
		}
	}
	if from != "" && to != "" && from > to {
		return usagef("--from jest później niż --to")
	}
	return nil
}

func cmdStatement(args []string) error {
	fs := flag.NewFlagSet("statement", flag.ContinueOnError)
	from := fs.String("from", "", "")
	to := fs.String("to", "", "")
	format := fs.String("format", "pdf", "")
	outPath := fs.String("o", "", "")
	force := fs.Bool("force", false, "")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("statement: podaj dokładnie jedno konto")
	}
	if err := checkDates(*from, *to, true); err != nil {
		return err
	}
	*format = strings.ToLower(*format)
	okFormat := false
	for _, f := range statementFormats {
		okFormat = okFormat || f == *format
	}
	if !okFormat {
		return usagef("--format: jeden z %s", strings.Join(statementFormats, ", "))
	}
	if *force && *outPath == "" {
		return usagef("--force wymaga -o PLIK (nazwy od banku nigdy nie nadpisujemy)")
	}
	if *outPath != "" {
		if st, err := os.Stat(filepath.Dir(*outPath)); err != nil || !st.IsDir() {
			return usagef("-o: katalog %s nie istnieje", filepath.Dir(*outPath))
		}
		if _, err := os.Lstat(*outPath); err == nil && !*force {
			return usagef("%s już istnieje (użyj --force)", *outPath)
		}
	}
	return withSession(func(c *Client, accts []Account) error {
		acc, err := resolveAccount(accts, pos[0])
		if err != nil {
			return err
		}
		name, data, err := c.Statement(acc, *from, *to, *format)
		if err != nil {
			return err
		}
		mode, path := saveUnique, statementFileName(name, *from, *to, *format)
		if *outPath != "" {
			mode, path = saveNew, *outPath
			if *force {
				mode = saveReplace
			}
		}
		path, err = saveFile(path, data, mode)
		if err != nil {
			return fmt.Errorf("zapis zestawienia: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Zapisano: %s (%d B)\n", safe(path), len(data))
		return nil
	})
}

// statementFileName: nazwa od banku (jak w przeglądarce), o ile jest bezpieczna i ma
// zamówione rozszerzenie; w przeciwnym razie nazwa lokalna z dat.
func statementFileName(name, from, to, format string) string {
	n := filepath.Base(strings.NewReplacer("/", "-", "\\", "-").Replace(name))
	if n == "" || n == "." || n == ".." || strings.HasPrefix(n, ".") || !strings.EqualFold(filepath.Ext(n), "."+format) ||
		!utf8.ValidString(n) || strings.ContainsFunc(n, isUnsafeRune) {
		return fmt.Sprintf("zestawienie_%s_%s.%s", from, to, format)
	}
	return n
}

type saveMode int

const (
	saveNew     saveMode = iota // błąd, gdy plik istnieje
	saveReplace                 // zastępuje wpis (także dowiązanie, nie jego cel)
	saveUnique                  // "nazwa (2).ext", gdy zajęte
)

// saveFile zapisuje atomowo: plik tymczasowy 0600 w katalogu docelowym, fsync, potem
// link (bez nadpisywania) albo rename (zastąpienie). Nigdy nie zostaje plik częściowy
// i nigdy nie piszemy przez istniejące dowiązanie symboliczne.
func saveFile(path string, data []byte, mode saveMode) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ipko-*.part")
	if err != nil {
		return path, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return path, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return path, err
	}
	if err := tmp.Close(); err != nil {
		return path, err
	}
	switch mode {
	case saveReplace:
		return path, os.Rename(tmpName, path)
	case saveNew:
		return path, os.Link(tmpName, path)
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for i := 1; i < 1000; i++ {
		p := path
		if i > 1 {
			p = fmt.Sprintf("%s (%d)%s", base, i, ext)
		}
		err := os.Link(tmpName, p)
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return p, err
		}
	}
	return path, fmt.Errorf("%s: zbyt wiele plików o tej nazwie", path)
}

// confirmer pyta na terminalu (nie na stdin, żeby nie dało się "przepchnąć" zgody potokiem).
// Podsumowanie i pytanie idą na ten sam terminal, z którego czytamy odpowiedź.
func confirmer(yes bool) Confirmer {
	return func(summary string) error {
		if yes {
			fmt.Fprint(os.Stderr, summary)
			return nil
		}
		tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			return errors.New("brak terminala do potwierdzenia; użyj --yes, jeśli na pewno")
		}
		defer tty.Close()
		fmt.Fprint(tty, summary+"Wysłać do banku? [t/N] ")
		line, err := bufio.NewReader(tty).ReadString('\n')
		if err != nil {
			return errors.New("przerwano (brak pełnej odpowiedzi), nic nie wysłano")
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "t", "tak", "y", "yes":
			return nil
		}
		return errors.New("przerwano, nic nie wysłano")
	}
}

func checkText(name, v string, max int, required bool) error {
	if required && strings.TrimSpace(v) == "" {
		return usagef("podaj %s", name)
	}
	if err := checkUserText(name, v); err != nil {
		return usageError{err.Error()}
	}
	if n := utf8.RuneCountInString(v); n > max {
		return usagef("%s: za długie (%d znaków, max %d)", name, n, max)
	}
	return nil
}

func paymentType(express, sorbnet bool) (string, error) {
	switch {
	case express && sorbnet:
		return "", usagef("--express i --sorbnet wykluczają się")
	case express:
		return "EXPRESS-ELIXIR", nil
	case sorbnet:
		return "SORBNET", nil
	}
	return "ELIXIR", nil
}

func cmdTransfer(args []string) error {
	fs := flag.NewFlagSet("transfer", flag.ContinueOnError)
	from := fs.String("from", "", "")
	to := fs.String("to", "", "")
	recipient := fs.String("recipient", "", "")
	name := fs.String("name", "", "")
	address := fs.String("address", "", "")
	amount := fs.String("amount", "", "")
	title := fs.String("title", "", "")
	date := fs.String("date", "", "")
	express := fs.Bool("express", false, "")
	sorbnet := fs.Bool("sorbnet", false, "")
	yes := fs.Bool("yes", false, "")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("transfer: nieoczekiwany argument %q", pos[0])
	}
	if *recipient != "" {
		if *to != "" || *name != "" || *address != "" {
			return usagef("--recipient wyklucza --to, --name i --address (dane odbiorcy są w książce)")
		}
		return transferToRecipient(*recipient, *from, *amount, *title, *date, *express, *sorbnet, *yes)
	}
	if *from == "" {
		return usagef("podaj --from (albo --recipient)")
	}
	nrb, err := normalizeNRB(*to)
	if err != nil {
		return usagef("--to: %v", err)
	}
	amt, err := normalizeAmount(*amount)
	if err != nil {
		return usagef("--amount: %v", err)
	}
	for _, e := range []error{checkText("--name", *name, 70, true), checkText("--title", *title, 140, true), checkText("--address", *address, 70, false)} {
		if e != nil {
			return e
		}
	}
	pt, err := paymentType(*express, *sorbnet)
	if err != nil {
		return err
	}
	if *date == "" {
		*date = today()
	} else if err := checkDate(*date, "--date"); err != nil {
		return usageError{err.Error()}
	}
	return withSession(func(c *Client, accts []Account) error {
		acc, err := resolveAccount(accts, *from)
		if err != nil {
			return err
		}
		if err := c.TransferNormal(NormalTransfer{From: acc, To: nrb, Name: *name, Address: *address,
			Amount: amt, Title: *title, Date: *date, PaymentType: pt}, confirmer(*yes)); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Przelew przyjęty przez bank.")
		return nil
	})
}

func cmdOwn(args []string) error {
	fs := flag.NewFlagSet("own", flag.ContinueOnError)
	from := fs.String("from", "", "")
	to := fs.String("to", "", "")
	amount := fs.String("amount", "", "")
	title := fs.String("title", "Przelew", "")
	date := fs.String("date", "", "")
	yes := fs.Bool("yes", false, "")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("own: nieoczekiwany argument %q", pos[0])
	}
	if *from == "" || *to == "" {
		return usagef("podaj --from i --to")
	}
	amt, err := normalizeAmount(*amount)
	if err != nil {
		return usagef("--amount: %v", err)
	}
	if err := checkText("--title", *title, 140, true); err != nil {
		return err
	}
	if *date == "" {
		*date = today()
	} else if err := checkDate(*date, "--date"); err != nil {
		return usageError{err.Error()}
	}
	return withSession(func(c *Client, accts []Account) error {
		src, err := resolveAccount(accts, *from)
		if err != nil {
			return err
		}
		dst, err := resolveAccount(accts, *to)
		if err != nil {
			return err
		}
		if err := c.TransferOwn(OwnTransfer{From: src, To: dst, Amount: amt, Title: *title, Date: *date}, confirmer(*yes)); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Przelew własny przyjęty przez bank.")
		return nil
	})
}

func cmdTax(args []string) error {
	fs := flag.NewFlagSet("tax", flag.ContinueOnError)
	from := fs.String("from", "", "")
	symbol := fs.String("symbol", "", "")
	account := fs.String("account", "", "")
	taxOffice := fs.Bool("tax-office", false, "")
	amount := fs.String("amount", "", "")
	period := fs.String("period", "", "")
	obligation := fs.String("obligation", "", "")
	date := fs.String("date", "", "")
	express := fs.Bool("express", false, "")
	yes := fs.Bool("yes", false, "")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("tax: nieoczekiwany argument %q", pos[0])
	}
	if *from == "" || *symbol == "" || *period == "" {
		return usagef("podaj --from, --symbol i --period")
	}
	nrb, err := normalizeNRB(*account)
	if err != nil {
		return usagef("--account: %v", err)
	}
	amt, err := normalizeAmount(*amount)
	if err != nil {
		return usagef("--amount: %v", err)
	}
	per, err := parsePeriod(*period)
	if err != nil {
		return usageError{err.Error()}
	}
	if err := checkText("--obligation", *obligation, 40, false); err != nil {
		return err
	}
	pt, err := paymentType(*express, false)
	if err != nil {
		return err
	}
	if *date == "" {
		*date = today()
	} else if err := checkDate(*date, "--date"); err != nil {
		return usageError{err.Error()}
	}
	sym := strings.ToUpper(strings.TrimSpace(*symbol))
	return withSession(func(c *Client, accts []Account) error {
		acc, err := resolveAccount(accts, *from)
		if err != nil {
			return err
		}
		if err := c.TransferTax(TaxTransfer{From: acc, Symbol: sym, Account: nrb, TaxOffice: *taxOffice, Amount: amt,
			Date: *date, PaymentType: pt, Period: per, ObligationID: *obligation}, confirmer(*yes)); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Przelew podatkowy przyjęty przez bank.")
		return nil
	})
}

func transferToRecipient(recipient, from, amount, title, date string, express, sorbnet, yes bool) error {
	amt, err := normalizeAmount(amount)
	if err != nil {
		return usagef("--amount: %v", err)
	}
	if err := checkText("--title", title, 140, false); err != nil {
		return err
	}
	pt := ""
	if express || sorbnet {
		if pt, err = paymentType(express, sorbnet); err != nil {
			return err
		}
	}
	if date == "" {
		date = today()
	} else if err := checkDate(date, "--date"); err != nil {
		return usageError{err.Error()}
	}
	return withSession(func(c *Client, accts []Account) error {
		list, err := c.Recipients()
		if err != nil {
			return err
		}
		r, err := resolveRecipient(list, recipient)
		if err != nil {
			return err
		}
		var src *Account
		if from != "" {
			if src, err = resolveAccount(accts, from); err != nil {
				return err
			}
		}
		if err := c.TransferToRecipient(RecipientTransfer{Recipient: r, From: src, Amount: amt, Title: title, Date: date, PaymentType: pt}, accts, confirmer(yes)); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Przelew przyjęty przez bank.")
		return nil
	})
}

func cmdRecipients(args []string) error {
	fs := flag.NewFlagSet("recipients", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("recipients: nieoczekiwany argument %q", pos[0])
	}
	return withSession(func(c *Client, accts []Account) error {
		list, err := c.Recipients()
		if err != nil {
			return err
		}
		if *asJSON {
			type out struct {
				OwnName, Name, Account, Address, Title string
			}
			var o []out
			for _, r := range list {
				o = append(o, out{r.OwnName, r.Name, r.Account, r.Address, r.Title})
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(o)
		}
		for _, r := range list {
			fmt.Printf("%s\t%s\t%s\t%s\n", oneLine(r.OwnName), oneLine(r.Name), formatNRB(oneLine(r.Account)), oneLine(r.Title))
		}
		return nil
	})
}
