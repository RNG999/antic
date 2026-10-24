package transport

import (
	"os"
	"os/signal"
	"syscall"
)

func notifyWinch(c chan os.Signal) { signal.Notify(c, syscall.SIGWINCH) }
func stopNotify(c chan os.Signal)  { signal.Stop(c) }

func signalNumber(s os.Signal) int {
	if n, ok := s.(syscall.Signal); ok {
		return int(n)
	}
	return int(syscall.SIGTERM)
}
