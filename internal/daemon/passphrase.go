package daemon

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"

	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
)

// ErrNoPassphraseSource is returned when there is neither a passphrase file nor
// a terminal to prompt on.
//
// This is a refusal, not a fallback. The alternatives — an unencrypted keyring,
// a fixed default, a key derived from the hostname — all end with device
// credentials readable from a backup, and every one of them would look like it
// worked.
var ErrNoPassphraseSource = errors.New(
	"daemon: no passphrase available — stdin is not a terminal and " +
		EnvPassphraseFile + " is not set. Either run the daemon attached to a " +
		"terminal for the first-time prompt, or point " + EnvPassphraseFile +
		" at a file with mode 600 (see the unattended-boot tradeoff in the docs)")

// passphraseFor obtains the operator passphrase.
//
// firstRun changes the interactive path only: a new keyring asks twice, because
// a typo there is not a login failure, it is a keyring nobody can ever open. An
// existing keyring asks once and lets the unwrap be the check.
func passphraseFor(cfg Config, firstRun bool, in *os.File, out io.Writer) ([]byte, error) {
	if cfg.PassphraseFile != "" {
		return secrets.ReadPassphraseFile(cfg.PassphraseFile)
	}
	fd := int(in.Fd())
	if !term.IsTerminal(fd) {
		return nil, ErrNoPassphraseSource
	}
	if !firstRun {
		return promptOnce(fd, out, "Operator passphrase: ")
	}

	fmt.Fprintf(out, "No keyring found in %s — creating one.\n", cfg.DataDir)
	fmt.Fprintf(out, "This passphrase protects every device credential. "+
		"There is no recovery if it is lost.\n")
	first, err := promptOnce(fd, out, "New operator passphrase: ")
	if err != nil {
		return nil, err
	}
	again, err := promptOnce(fd, out, "Repeat passphrase: ")
	if err != nil {
		return nil, err
	}
	if string(first) != string(again) {
		clear(first)
		clear(again)
		return nil, errors.New("daemon: the two passphrases do not match")
	}
	clear(again)
	return first, nil
}

func promptOnce(fd int, out io.Writer, prompt string) ([]byte, error) {
	fmt.Fprint(out, prompt)
	pass, err := term.ReadPassword(fd)
	fmt.Fprintln(out)
	if err != nil {
		return nil, fmt.Errorf("daemon: read passphrase: %w", err)
	}
	if len(pass) == 0 {
		return nil, secrets.ErrNoPassphrase
	}
	return pass, nil
}
