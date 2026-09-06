package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"the8020/kernel/logging/daemon"
	"the8020/kernel/logging/records"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "logd:", records.Text(err.Error(), 1024))
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 1 {
		return fmt.Errorf("logd accepts configuration only through the kernel control descriptor")
	}
	// Restore nonblocking mode after inheritance so Go polling can cancel reads.
	for _, fd := range []int{3, 4, 5} {
		if err := syscall.SetNonblock(fd, true); err != nil {
			return fmt.Errorf("required logging descriptor %d is unavailable", fd)
		}
	}
	controlFile := os.NewFile(3, "logd-control")
	raw := [2]*os.File{os.NewFile(4, "kernel-stdout"), os.NewFile(5, "kernel-stderr")}
	defer raw[0].Close()
	defer raw[1].Close()
	control, err := net.FileConn(controlFile)
	_ = controlFile.Close()
	if err != nil {
		return fmt.Errorf("invalid logging control descriptor")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return daemon.Run(ctx, control, raw)
}
