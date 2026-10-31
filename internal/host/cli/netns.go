package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/RNG999/antic/internal/host/egress"
)

// netnsCmd is the hidden argv[1] that runs antic as the first process in a
// guest's network namespace (pasta runs it; see guest.startNet).
const netnsCmd = egress.HelperCmd

// netnsMain loads the egress filter, starts the DNS filter if the guest has
// an allow list, and runs passt. It exits when passt exits; if the DNS filter
// fails, passt is stopped so the guest loses its network rather than its
// filter.
func netnsMain(args []string) int {
	fs := flag.NewFlagSet(netnsCmd, flag.ContinueOnError)
	nft := fs.String("nft", "", "nft binary")
	rules := fs.String("rules", "", "nftables rules file")
	var domains string
	fs.StringVar(&domains, "allow-domains", "", "file with allowed domains")
	fs.StringVar(&domains, "dns-domains", "", "alias for --allow-domains")
	denyDomains := fs.String("deny-domains", "", "file with denied domains")
	defaultDeny := fs.Bool("default-deny", false, "refuse names the allow list does not match")
	upstream := fs.String("upstream", "", "upstream resolver address")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	argv := fs.Args()
	if *nft == "" || *rules == "" || len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "antic netns: usage: --nft PATH --rules FILE [--allow-domains FILE] [--deny-domains FILE] [--default-deny] [--upstream ADDR] -- PASST...")
		return exitError
	}
	if err := netnsRun(*nft, *rules, domains, *denyDomains, *defaultDeny, *upstream, argv); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "antic netns: %v\n", err)
		return exitError
	}
	return 0
}

func netnsRun(nft, rules, domainsFile, denyFile string, defaultDeny bool, upstream string, argv []string) error {
	if out, err := exec.Command(nft, "-f", rules).CombinedOutput(); err != nil {
		return fmt.Errorf("nft -f %s: %w: %s", rules, err, strings.TrimSpace(string(out)))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var proxy *egress.Proxy
	if domainsFile != "" || denyFile != "" || defaultDeny {
		var allowList, denyList []string
		if domainsFile != "" {
			b, err := os.ReadFile(domainsFile)
			if err != nil {
				return err
			}
			allowList = strings.Fields(string(b))
		}
		if denyFile != "" {
			b, err := os.ReadFile(denyFile)
			if err != nil {
				return err
			}
			denyList = strings.Fields(string(b))
		}
		up, err := netip.ParseAddr(upstream)
		if err != nil {
			return fmt.Errorf("--upstream: %w", err)
		}
		proxy, err = egress.New(egress.Config{
			Upstream:    netip.AddrPortFrom(up, 53),
			Allow:       allowList,
			Deny:        denyList,
			DefaultDeny: defaultDeny,
			NFT:         nft,
			Log:         os.Stderr,
		})
		if err != nil {
			return fmt.Errorf("dns filter: %w", err)
		}
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		for s := range sigs {
			cmd.Process.Signal(s)
		}
	}()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	proxyErr := make(chan error, 1)
	if proxy != nil {
		go func() { proxyErr <- proxy.Serve(ctx) }()
	}
	select {
	case err := <-done:
		return err
	case err := <-proxyErr:
		cmd.Process.Kill()
		<-done
		if err == nil {
			err = errors.New("stopped")
		}
		return fmt.Errorf("dns filter: %w", err)
	}
}
