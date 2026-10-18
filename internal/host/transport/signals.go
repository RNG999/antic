package transport

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/crypto/ssh"
)

func notifyWinch(c chan os.Signal) { signal.Notify(c, syscall.SIGWINCH) }
func stopNotify(c chan os.Signal)  { signal.Stop(c) }

func sshSignal(s os.Signal) ssh.Signal {
	switch s {
	case syscall.SIGINT:
		return ssh.SIGINT
	case syscall.SIGTERM:
		return ssh.SIGTERM
	case syscall.SIGHUP:
		return ssh.SIGHUP
	case syscall.SIGQUIT:
		return ssh.SIGQUIT
	}
	return ssh.SIGTERM
}

func signalNumber(s os.Signal) int {
	if n, ok := s.(syscall.Signal); ok {
		return int(n)
	}
	return int(syscall.SIGTERM)
}
