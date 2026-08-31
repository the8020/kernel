package sshserver

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	gossh "golang.org/x/crypto/ssh"
)

const maximumHostKeyBytes = 64 * 1024

func loadOrCreateHostKey(path string) (gossh.Signer, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("SSH host key path must be absolute")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create SSH host key directory: %w", err)
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect SSH host key directory: %w", err)
	}
	if !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("SSH host key directory must be a real directory")
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, fmt.Errorf("restrict SSH host key directory: %w", err)
	}
	if signer, err := readHostKey(path); err == nil {
		return signer, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate SSH host key: %w", err)
	}
	defer clear(privateKey)
	encoded, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("encode SSH host key: %w", err)
	}
	defer clear(encoded)
	data := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})
	defer clear(data)
	temporary, err := os.CreateTemp(filepath.Dir(path), ".host-ed25519-*")
	if err != nil {
		return nil, fmt.Errorf("stage SSH host key: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(data)
	}
	if err == nil {
		err = temporary.Sync()
	}
	closeErr := temporary.Close()
	if err != nil {
		return nil, fmt.Errorf("write SSH host key: %w", err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close SSH host key: %w", closeErr)
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("publish SSH host key: %w", err)
		}
	}
	return readHostKey(path)
}

func readHostKey(path string) (gossh.Signer, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("SSH host key must be a regular file")
	}
	if info.Size() <= 0 || info.Size() > maximumHostKeyBytes {
		return nil, errors.New("SSH host key size is invalid")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("restrict SSH host key: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read SSH host key: %w", err)
	}
	signer, err := gossh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("parse SSH host key: %w", err)
	}
	if signer.PublicKey().Type() != gossh.KeyAlgoED25519 {
		return nil, errors.New("SSH host key must be Ed25519")
	}
	return signer, nil
}
