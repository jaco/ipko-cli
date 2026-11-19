package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// Login i hasło są w pęku kluczy macOS jako "generic password":
//   security add-generic-password -s ipko-cli -a LOGIN -w
// Uwaga: `security -w` zwraca hasła ze znakami spoza ASCII lub z "\" jako hex,
// więc czytamy wariant -g, w którym forma hex ma jednoznaczny prefiks 0x.

const securityBin = "/usr/bin/security"

var (
	reAcct     = regexp.MustCompile(`(?m)^\s*"acct"<blob>=(.*)$`)
	rePassword = regexp.MustCompile(`(?m)^password: (.*)$`)
	reHexValue = regexp.MustCompile(`^0x((?:[0-9A-F]{2})+)(?:\s|$)`)
)

func decodeSecurityValue(s string) (string, bool) {
	s = strings.TrimRight(s, "\r\n")
	if m := reHexValue.FindStringSubmatch(s); m != nil {
		b, err := hex.DecodeString(m[1])
		if err != nil {
			return "", false
		}
		return string(b), true
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1], true
	}
	return "", false
}

func keychainCredentials(service string) (login, password string, err error) {
	cmd := exec.Command(securityBin, "find-generic-password", "-g", "-s", service)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("brak wpisu %q w pęku kluczy; dodaj go: security add-generic-password -s %s -a LOGIN -w", service, service)
	}
	m := reAcct.FindStringSubmatch(stdout.String())
	if m == nil {
		return "", "", errors.New("wpis w pęku kluczy nie ma loginu (pole konta)")
	}
	login, ok := decodeSecurityValue(m[1])
	if !ok || login == "" {
		return "", "", errors.New("nie udało się odczytać loginu z pęku kluczy")
	}
	p := rePassword.FindStringSubmatch(stderr.String())
	if p == nil {
		return "", "", errors.New("nie udało się odczytać hasła z pęku kluczy")
	}
	password, ok = decodeSecurityValue(p[1])
	if !ok || password == "" {
		return "", "", errors.New("hasło w pęku kluczy jest puste lub nieczytelne")
	}
	return login, password, nil
}

// fingerprint: stały losowy identyfikator tej instalacji (jak odcisk przeglądarki).
// Plik czytamy i tworzymy bez podążania za dowiązaniami; uszkodzonego nie nadpisujemy po cichu.
func loadFingerprint() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "ipko-cli")
	path := filepath.Join(dir, "fingerprint")
	if fp, err := readFingerprint(path); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return fp, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	fp := hex.EncodeToString(buf)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return readFingerprint(path) // równoległe pierwsze uruchomienie: bierzemy wartość tamtego procesu
	}
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(fp + "\n"); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return fp, nil
}

func readFingerprint(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return "", fmt.Errorf("%s: jest dowiązaniem symbolicznym; usuń go", path)
		}
		return "", err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return "", fmt.Errorf("%s: to nie jest zwykły plik", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, 64))
	if err != nil {
		return "", err
	}
	fp := strings.TrimSpace(string(b))
	if _, herr := hex.DecodeString(fp); len(fp) != 32 || herr != nil {
		return "", fmt.Errorf("%s: uszkodzony identyfikator instalacji; usuń plik, a zostanie utworzony nowy", path)
	}
	return fp, nil
}
