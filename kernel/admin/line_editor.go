package admin

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

const maxCommandHistory = 100

type splitReadWriter struct {
	io.Reader
	io.Writer
}

type sessionHistory struct {
	entries []string
}

func (h *sessionHistory) Add(entry string) {
	h.entries = append(h.entries, entry)
}

func (h *sessionHistory) Len() int {
	return len(h.entries)
}

func (h *sessionHistory) At(index int) string {
	return h.entries[len(h.entries)-1-index]
}

func (h *sessionHistory) replaceLatest(entry string) {
	h.dropLatest()
	h.Add(entry)
	h.finalizeLatest()
}

func (h *sessionHistory) dropLatest() {
	if len(h.entries) > 0 {
		h.entries = h.entries[:len(h.entries)-1]
	}
}

func (h *sessionHistory) finalizeLatest() {
	if len(h.entries) == 0 {
		return
	}
	latest := len(h.entries) - 1
	if h.entries[latest] == "" || (latest > 0 && h.entries[latest-1] == h.entries[latest]) {
		h.entries = h.entries[:latest]
	}
	if len(h.entries) > maxCommandHistory {
		h.entries = append([]string(nil), h.entries[len(h.entries)-maxCommandHistory:]...)
	}
}

type interactiveLineReader struct {
	scanner    *bufio.Scanner
	terminal   *term.Terminal
	history    *sessionHistory
	restore    func() error
	output     io.Writer
	terminalFD int
}

func newInteractiveLineReader(input io.Reader, output io.Writer) *interactiveLineReader {
	reader := &interactiveLineReader{output: output, terminalFD: -1}
	inputFile, inputIsFile := input.(*os.File)
	outputFile, outputIsFile := output.(*os.File)
	if inputIsFile && outputIsFile && term.IsTerminal(int(inputFile.Fd())) && term.IsTerminal(int(outputFile.Fd())) {
		state, err := term.MakeRaw(int(inputFile.Fd()))
		if err == nil {
			reader.configureTerminal(input, output)
			reader.terminalFD = int(outputFile.Fd())
			reader.refreshTerminalSize()
			reader.restore = func() error {
				return term.Restore(int(inputFile.Fd()), state)
			}
			return reader
		}
	}
	reader.scanner = bufio.NewScanner(input)
	return reader
}

func newTerminalLineReader(input io.Reader, output io.Writer, width, height int) *interactiveLineReader {
	reader := &interactiveLineReader{output: output, terminalFD: -1}
	reader.configureTerminal(input, output)
	if width > 0 && height > 0 {
		_ = reader.terminal.SetSize(width, height)
	}
	return reader
}

func (r *interactiveLineReader) configureTerminal(input io.Reader, output io.Writer) {
	r.history = &sessionHistory{}
	r.terminal = term.NewTerminal(splitReadWriter{Reader: input, Writer: output}, "")
	r.terminal.History = r.history
	r.terminal.SetBracketedPasteMode(true)
}

func (r *interactiveLineReader) ReadLine(prompt string) (string, bool, error) {
	if r.terminal == nil {
		_, _ = fmt.Fprint(r.output, prompt)
		if !r.scanner.Scan() {
			return "", false, r.scanner.Err()
		}
		return r.scanner.Text(), true, nil
	}

	r.refreshTerminalSize()
	r.terminal.SetPrompt(prompt)
	defer r.terminal.SetPrompt(prompt)
	var pastedLines []string
	for {
		line, err := r.terminal.ReadLine()
		if err == term.ErrPasteIndicator {
			r.history.dropLatest()
			pastedLines = append(pastedLines, line)
			// x/term returns every newline inside a bracketed paste as a
			// partial line. Suppress the primary prompt while collecting the
			// remaining lines so one paste still looks like one command.
			r.terminal.SetPrompt("")
			continue
		}
		if errors.Is(err, io.EOF) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		if len(pastedLines) > 0 {
			r.history.dropLatest()
			pastedLines = append(pastedLines, line)
			line = strings.TrimSpace(strings.Join(pastedLines, " "))
			r.history.Add(line)
			r.history.finalizeLatest()
			return line, true, nil
		}
		r.history.replaceLatest(strings.TrimSpace(line))
		return line, true, nil
	}
}

func (r *interactiveLineReader) AddHistory(line string) {
	if r.history == nil {
		return
	}
	r.history.Add(line)
	r.history.finalizeLatest()
}

func (r *interactiveLineReader) ReadValue(prompt string) (string, error) {
	line, ok, err := r.ReadLine(prompt)
	if r.history != nil {
		// Prompted values are inputs to the current command, not standalone
		// commands that should replace it in session history.
		r.history.dropLatest()
	}
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("input ended before a value was read")
	}
	return line, nil
}

func (r *interactiveLineReader) ReadSecret(prompt, confirmationPrompt string, fromStdin bool) (string, error) {
	if r.terminal != nil {
		value, err := r.terminal.ReadPassword(prompt)
		if err != nil {
			return "", err
		}
		if fromStdin {
			return value, nil
		}
		confirmation, err := r.terminal.ReadPassword(confirmationPrompt)
		if err != nil {
			return "", err
		}
		if value != confirmation {
			return "", errors.New("secret values do not match")
		}
		return value, nil
	}
	if !fromStdin {
		return "", errors.New("secure secret prompting requires a terminal; use the command's standard-input option for automation")
	}
	if !r.scanner.Scan() {
		if err := r.scanner.Err(); err != nil {
			return "", err
		}
		return "", errors.New("standard input ended before a secret was read")
	}
	return r.scanner.Text(), nil
}

// Writer returns an output writer that preserves terminal cursor state while
// raw mode is active. In particular, term.Terminal translates line feeds to
// carriage-return/line-feed pairs; writing directly to the raw TTY would make
// each rendered line begin at the previous line's cursor column.
func (r *interactiveLineReader) Writer(fallback io.Writer) io.Writer {
	if r.terminal != nil {
		return r.terminal
	}
	return fallback
}

func (r *interactiveLineReader) Close() error {
	if r.terminal != nil {
		r.terminal.SetBracketedPasteMode(false)
	}
	if r.restore == nil {
		return nil
	}
	return r.restore()
}

func (r *interactiveLineReader) refreshTerminalSize() {
	if r.terminal == nil || r.terminalFD < 0 {
		return
	}
	width, height, err := term.GetSize(r.terminalFD)
	if err == nil && width > 0 && height > 0 {
		_ = r.terminal.SetSize(width, height)
	}
}
