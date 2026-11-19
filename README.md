# ipko

**Klient wiersza poleceń do iPKO — bankowości internetowej PKO Banku Polskiego.**
Konta, historia, zestawienia i przelewy (zwykłe, własne, podatkowe) prosto z terminala,
z każdą sesją i każdym przelewem zatwierdzanym w aplikacji IKO.

*Unofficial command-line client for iPKO (PKO Bank Polski online banking): accounts, history,
statements and transfers, every login and transfer confirmed in the IKO mobile app.*

> [!WARNING]
> Projekt **nieoficjalny**, niezwiązany z PKO Bankiem Polskim. Protokół został odtworzony
> z ruchu sieciowego przeglądarki i może przestać działać po zmianach po stronie banku.
> Używasz na własną odpowiedzialność — to narzędzie potrafi wysyłać prawdziwe pieniądze.

---

## Dlaczego warto

- **Jeden plik binarny.** Go, wyłącznie biblioteka standardowa, zero zależności — aktualizacja
  Pythona, Node czy czegokolwiek innego go nie zepsuje.
- **Jednorazowy dostęp.** Każde uruchomienie to nowe logowanie zatwierdzane w IKO i wylogowanie
  na końcu. Żadnej zapisanej sesji, żadnych ciasteczek na dysku, nigdy „zaufanego urządzenia”.
- **Dwa klucze do przelewu.** Najpierw podsumowanie i pytanie w terminalu, potem zatwierdzenie
  w IKO, gdzie widzisz kwotę i rachunek odbiorcy.
- **Hasło w pęku kluczy macOS**, nie w pliku konfiguracyjnym ani w argumentach.
- **Walidacja przed wysłaniem.** Suma kontrolna numeru rachunku, format kwoty, długości pól —
  literówka w numerze konta kończy się błędem, zanim cokolwiek pójdzie do banku.

## Wymagania

- macOS (pęk kluczy przez `/usr/bin/security`)
- Go 1.27+ do zbudowania
- konto w iPKO z aplikacją IKO jako metodą autoryzacji

## Instalacja

```sh
git clone https://github.com/jaco/ipko-cli.git
cd ipko-cli
go build -o ipko .
sudo mv ipko /usr/local/bin/        # opcjonalnie
```

Dodaj login i hasło do pęku kluczy (hasło wpisujesz w terminalu, nie trafia do historii powłoki):

```sh
security add-generic-password -s ipko-cli -a TWOJ_LOGIN_IPKO -w
```

Chcesz, żeby macOS pytał o zgodę przy każdym odczycie hasła? Dodaj `-T ''` (żadna aplikacja
nie jest wtedy domyślnie zaufana):

```sh
security add-generic-password -s ipko-cli -a TWOJ_LOGIN_IPKO -T '' -w
```

## Użycie

Każda komenda loguje się od nowa — telefon zawibruje, zatwierdzasz logowanie w IKO.

### Konta

```console
$ ipko accounts
Zatwierdź logowanie w aplikacji IKO (Urządzenie IOS), masz 120 s...
80 1020 0000 0000 1111 2222 3333   PKO KONTO ZA ZERO    1000.00 PLN   (księgowe 1000.00, blokady 0.00)
27 1020 0000 0000 4444 5555 6666   RACHUNEK FIRMOWY    25000.00 PLN   (księgowe 25000.00, blokady 0.00)
```

Rachunek wskazujesz **końcówką numeru** (min. 4 cyfry), pełnym numerem albo **fragmentem
nazwy**: `6666`, `firmowy`, `80 1020 0000 0000 1111 2222 3333`.

### Historia operacji

```sh
ipko history 6666 --from 2026-10-01 --to 2026-10-31          # TSV: data, kwota, strona, tytuł, rachunek, typ, saldo
ipko history firmowy --from 2026-10-01 --to 2026-10-31 --json # pełne dane z banku
```

### Zestawienie (wyciąg) za okres

```sh
ipko statement 6666 --from 2026-10-01 --to 2026-10-31                  # PDF z nazwą nadaną przez bank
ipko statement 6666 --from 2026-10-01 --to 2026-10-31 --format csv -o pazdziernik.csv
```

Formaty: `pdf`, `csv`, `xls`, `xml`, `html`. Istniejące pliki nie są nadpisywane — przy zajętej
nazwie powstaje `nazwa (2).pdf`. Nadpisanie tylko jawnie: `-o plik.pdf --force`. Zapis jest
atomowy (nigdy nie zostaje pół pliku) i nie podąża za dowiązaniami symbolicznymi.

### Przelew zwykły

```console
$ ipko transfer --from 3333 --to 'NRB_ODBIORCY' \
    --name 'Urząd Miasta' --amount 250 --title 'Podatek od nieruchomości IV rata'
PRZELEW ZWYKŁY
  z:        PKO KONTO ZA ZERO (80 1020 0000 0000 1111 2222 3333)
  do:       47 1160 0000 0000 0000 1234 5678
  odbiorca: Urząd Miasta
  kwota:    250.00 PLN
  tytuł:    Podatek od nieruchomości IV rata
  data:     2026-11-19, ELIXIR
Wysłać do banku? [t/N] t
Bank odbiorcy wg PKO: MIL   Bankowy Punkt Rozliczeń nr 1
Opłata (ELIXIR): 0.00 PLN
Sprawdź dane na ekranie IKO; jeśli coś się nie zgadza, odrzuć tam dyspozycję.
Zatwierdź przelew w aplikacji IKO (Urządzenie IOS), masz 120 s...
Przelew przyjęty przez bank.
```

Opcje: `--date RRRR-MM-DD` (przelew z datą przyszłą), `--express` (Express Elixir),
`--sorbnet`, `--address`.

### Przelew własny

```sh
ipko own --from firmowy --to 3333 --amount 2000 --title 'Wypłata'
```

### Przelew podatkowy (mikrorachunek)

```sh
ipko tax --from firmowy --symbol VAT-7  --account 'TWOJ_MIKRORACHUNEK' --amount 12345 --period 2026-10
ipko tax --from firmowy --symbol PIT-5L --account 'TWOJ_MIKRORACHUNEK' --amount 1234  --period 2026-10
ipko tax --from firmowy --symbol PPE    --account 'TWOJ_MIKRORACHUNEK' --amount 5000  --period 2026-10
```

Grupę formularza („częste” — PIT, VAT, CIT…, albo „pozostałe” — PPE, AKC…) `ipko` dobiera sam
na podstawie symbolu. Identyfikator płatnika bierze z formularza banku i pokazuje go
(zamaskowany) w podsumowaniu przed zgodą. Rachunek urzędu zamiast
mikrorachunku: `--tax-office`. Identyfikacja zobowiązania: `--obligation`.

| `--period`    | znaczenie   |
|---------------|-------------|
| `2026-10`     | miesiąc     |
| `2026-Q3`     | kwartał     |
| `2026-H1`     | półrocze    |
| `2026`        | rok         |
| `2026-10-D2`  | dekada      |
| `2026-10-15`  | dzień       |

### Bez pytania w terminalu

`--yes` pomija pytanie `Wysłać do banku?` (np. w skryptach) — to świadoma rezygnacja
z lokalnej kontroli. Zatwierdzenie w IKO zostaje zawsze — tego nie da się obejść.

Argumenty komend (odbiorca, kwota, tytuł) widać w historii powłoki i liście procesów.
Jeśli to problem, poprzedź komendę spacją (`HISTCONTROL=ignorespace` / `setopt HIST_IGNORE_SPACE`).

### Kody wyjścia

| kod | znaczenie |
|-----|-----------|
| `0` | sukces (przelew przyjęty przez bank, nawet jeśli potem nie udało się wylogować) |
| `1` | błąd — operacja nie została wykonana |
| `2` | błędne argumenty — nic nie wysłano do banku |
| `3` | **wynik nieznany** — odpowiedź banku nie dotarła po zatwierdzeniu w IKO; sprawdź historię, **nie ponawiaj automatycznie** |

## Jak to działa

```
ipko ──► GET /  (ciasteczka)
     ──► /ipko3/login  login ──► hasło ──► [IKO] ──► sesja
     ──► /ipko3/init                         (lista rachunków)
     ──► /ipko3/transactions/transfers/...   formularz ──► submit ──► [IKO] ──► END
     ──► /ipko3/logout
```

iPKO to aplikacja webowa rozmawiająca z serwerem kopertami JSON
`{version, seq, flow_id, state_id, token, action, data}`. Operacje wieloetapowe są maszynami
stanów: serwer zwraca kolejny stan i token, klient odsyła go w następnym kroku. Zatwierdzenie
w IKO to long-poll na `/api/get_lp_hook_status`; o wyniku (zatwierdzone / odrzucone) zawsze
rozstrzyga serwer w kolejnym kroku operacji, nigdy klient.

## Bezpieczeństwo

- Dane logowania trafiają wyłącznie do `https://www.ipko.pl` — adres jest wpisany na stałe,
  przekierowania poza niego są blokowane, certyfikat weryfikuje system.
- Logowanie bez zatwierdzenia w IKO jest **odrzucane** (np. gdy ktoś dodał urządzenie jako zaufane).
- Sukces operacji ogłasza wyłącznie bank (stan końcowy, bez błędów, ta sama operacja);
  odrzucenie w IKO, niespójna odpowiedź czy przerwane połączenie nigdy nie dają „sukcesu”.
- Komunikaty błędów i tryb debug nie zawierają treści odpowiedzi banku, tokenów ani hasła.
- Tekst z banku (tytuły, kontrahenci) jest wypisywany bez znaków sterujących terminala;
  znaki sterujące w Twoich polach przelewu są odrzucane.
- Odpowiedzi mają limit rozmiaru, oczekiwanie na IKO ma twardy termin.

Kod przeszedł przegląd bezpieczeństwa pod kątem wycieku sekretów, integralności przelewów,
autoryzacji IKO, sieci, plików i wstrzykiwania do terminala; znalezione problemy są poprawione
i pokryte testami.

## Zmienne środowiskowe

| zmienna                 | opis                                                         |
|-------------------------|--------------------------------------------------------------|
| `IPKO_DEBUG=1`          | przebieg zapytań na stderr (adresy i stany, bez żadnych danych) |
| `IPKO_KEYCHAIN_SERVICE` | nazwa wpisu w pęku kluczy (domyślnie `ipko-cli`)             |

## Testy

```sh
go test ./...
```

Testy działają offline na fałszywym serwerze iPKO, który odtwarza przebiegi nagrane w prawdziwej
sesji: logowanie z IKO, błędne hasło, odrzucenie w IKO, brak zgody (wtedy do banku nie idzie nic),
przelewy wszystkich typów, zestawienia i historię — oraz scenariusze wrogie: przekierowanie hasła
na obcy host, bank bez IKO, zerwane połączenie po zatwierdzeniu, odpowiedź innej operacji.
Dane w testach są fikcyjne.

## Ograniczenia

- Tylko macOS (pęk kluczy) i tylko autoryzacja przez IKO (bez SMS i kart kodów).
- Przelewy krajowe w PLN; bez przelewów zagranicznych, zleceń stałych i doładowań.
- Strona WWW wysyła dodatkowo dane antyfraudowe (analiza zachowania); `ipko` ich nie wysyła.
  Bank traktuje go jak nowe urządzenie i dlatego żąda potwierdzenia w IKO przy każdym logowaniu.

## Licencja

[MIT](LICENSE)
