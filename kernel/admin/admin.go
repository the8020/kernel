// Package admin owns one-shot and interactive administrative invocation.
package admin

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"
	"the8020/kernel/cbus/cli"
	"the8020/kernel/cbus/client"
	"the8020/kernel/cbus/core"
	"the8020/kernel/instance"
)

type options struct {
	root    string
	json    bool
	command []string
}

// Main runs the administrative executable and returns a process exit code.
func Main(catalog []core.Command, args []string, input io.Reader, output, errorOutput io.Writer) int {
	options, err := parseOptions(args)
	if err != nil {
		_, _ = fmt.Fprintf(errorOutput, "error: %s\n", err)
		return 2
	}
	root, err := instance.ResolveRoot(options.root)
	if err != nil {
		_, _ = fmt.Fprintf(errorOutput, "error: %s\n", err)
		return 2
	}
	paths := instance.NewPaths(root)
	commandClient := client.New(paths.Socket)
	defer commandClient.Close()
	runner := cli.New(catalog, commandClient)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(options.command) > 0 {
		runner.SetValueResolver(oneShotValueResolver(input, errorOutput))
		runner.SetSecretResolver(oneShotSecretResolver(input, errorOutput))
		return runner.Run(ctx, options.command, options.json, output)
	}
	return interactive(ctx, runner, input, output, errorOutput, options.json)
}

func oneShotValueResolver(input io.Reader, promptOutput io.Writer) cli.ValueResolver {
	scanner := bufio.NewScanner(input)
	return func(prompt string) (string, error) {
		inputFile, isFile := input.(*os.File)
		if !isFile || !term.IsTerminal(int(inputFile.Fd())) {
			return "", errors.New("ordinary argument prompting requires a terminal; provide the missing argument in the command")
		}
		_, _ = fmt.Fprint(promptOutput, prompt)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return "", err
			}
			return "", errors.New("terminal input ended before a value was read")
		}
		return scanner.Text(), nil
	}
}

func oneShotSecretResolver(input io.Reader, promptOutput io.Writer) cli.SecretResolver {
	scanner := bufio.NewScanner(input)
	return func(prompt, confirmationPrompt string, fromStdin bool) (string, error) {
		inputFile, isFile := input.(*os.File)
		isTerminal := isFile && term.IsTerminal(int(inputFile.Fd()))
		if fromStdin && !isTerminal {
			if !scanner.Scan() {
				if err := scanner.Err(); err != nil {
					return "", err
				}
				return "", errors.New("standard input ended before a password was read")
			}
			return scanner.Text(), nil
		}
		if !isTerminal {
			return "", errors.New("secure password prompting requires a terminal; use --password-stdin for automation")
		}
		read := func(label string) (string, error) {
			if label != "" {
				_, _ = fmt.Fprint(promptOutput, label)
			}
			value, err := term.ReadPassword(int(inputFile.Fd()))
			if label != "" {
				_, _ = fmt.Fprintln(promptOutput)
			}
			return string(value), err
		}
		value, err := read(func() string {
			if fromStdin {
				return ""
			}
			return prompt
		}())
		if err != nil || fromStdin {
			return value, err
		}
		confirmation, err := read(confirmationPrompt)
		if err != nil {
			return "", err
		}
		if value != confirmation {
			return "", errors.New("passwords do not match")
		}
		return value, nil
	}
}

func parseOptions(args []string) (options, error) {
	var result options
	for len(args) > 0 {
		switch args[0] {
		case "--root":
			if len(args) < 2 {
				return result, fmt.Errorf("--root requires a path")
			}
			result.root = args[1]
			args = args[2:]
		case "--json":
			result.json = true
			args = args[1:]
		case "--help", "-h":
			result.command = []string{"help"}
			args = args[1:]
			if len(args) > 0 {
				result.command = append(result.command, args...)
				args = nil
			}
		default:
			result.command = append([]string(nil), args...)
			args = nil
		}
	}
	return result, nil
}

func interactive(ctx context.Context, runner *cli.Runner, input io.Reader, output, errorOutput io.Writer, jsonOutput bool) int {
	lineReader := newInteractiveLineReader(input, output)
	runner.SetValueResolver(lineReader.ReadValue)
	runner.SetSecretResolver(lineReader.ReadSecret)
	commandOutput := lineReader.Writer(output)
	commandErrorOutput := lineReader.Writer(errorOutput)
	defer func() {
		if err := lineReader.Close(); err != nil {
			_, _ = fmt.Fprintf(errorOutput, "error: restore terminal: %s\n", err)
		}
	}()
	for {
		line, ok, err := lineReader.ReadLine("admin> ")
		if err != nil {
			_, _ = fmt.Fprintf(commandErrorOutput, "error: read input: %s\n", err)
			return 1
		}
		if !ok {
			return 0
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lineReader.AddHistory(line)
		args, err := cli.SplitLine(line)
		if err != nil {
			_, _ = fmt.Fprintf(commandErrorOutput, "error: %s\n", err)
			continue
		}
		if len(args) == 1 && args[0] == "exit" {
			return 0
		}
		select {
		case <-ctx.Done():
			return 130
		default:
		}
		runner.Run(ctx, args, jsonOutput, commandOutput)
	}
}
