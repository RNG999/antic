package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/RNG999/antic/internal/host/guest"
)

// GuestStats holds resource metrics for a guest.
type GuestStats struct {
	Name        string  `json:"name"`
	State       string  `json:"state"`
	Uptime      string  `json:"uptime"`
	CPUs        int     `json:"cpus"`
	CPUPercent  float64 `json:"cpu_percent"`
	MemoryUsed  int64   `json:"memory_used"`
	MemoryLimit string  `json:"memory_limit"`
	PIDs        int     `json:"pids"`
	Sessions    int     `json:"sessions"`
}

type sample struct {
	time     time.Time
	cpuUsec  int64
	memBytes int64
	pids     int
	sessions int
	state    string
	uptime   string
	cpus     int
	memLimit string
}

func readGuestSample(ctx context.Context, g *guest.Guest) sample {
	now := time.Now()
	info, err := g.Inspect(ctx)
	if err != nil {
		return sample{time: now, state: "error"}
	}
	s := sample{
		time:     now,
		state:    info.State,
		uptime:   uptime(info),
		cpus:     g.Cfg.CPUs,
		memLimit: g.Cfg.Memory,
	}
	if info.State != "running" {
		return s
	}

	cg := g.CgroupDir()
	// Read CPU usage in microseconds from cgroup v2
	if b, err := os.ReadFile(filepath.Join(cg, "cpu.stat")); err == nil {
		sc := bufio.NewScanner(strings.NewReader(string(b)))
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) == 2 && fields[0] == "usage_usec" {
				s.cpuUsec, _ = strconv.ParseInt(fields[1], 10, 64)
				break
			}
		}
	}
	// Read current memory usage in bytes from cgroup v2
	if b, err := os.ReadFile(filepath.Join(cg, "memory.current")); err == nil {
		s.memBytes, _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	}
	// Read tasks / pids count
	if b, err := os.ReadFile(filepath.Join(cg, "pids.current")); err == nil {
		s.pids, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	// Read running command sessions count
	if ss, err := g.Sessions(ctx); err == nil {
		s.sessions = len(ss)
	}
	return s
}

func computeStats(name string, prev, curr sample) GuestStats {
	st := GuestStats{
		Name:        name,
		State:       curr.state,
		Uptime:      curr.uptime,
		CPUs:        curr.cpus,
		MemoryUsed:  curr.memBytes,
		MemoryLimit: curr.memLimit,
		PIDs:        curr.pids,
		Sessions:    curr.sessions,
	}
	if curr.state == "running" && prev.state == "running" {
		deltaT := curr.time.Sub(prev.time).Microseconds()
		deltaCPU := curr.cpuUsec - prev.cpuUsec
		if deltaT > 0 && deltaCPU >= 0 {
			st.CPUPercent = (float64(deltaCPU) / float64(deltaT)) * 100.0
		}
	}
	return st
}

func formatMem(used int64, limit string) string {
	if used <= 0 {
		return "- / " + limit
	}
	return formatBytes(used) + " / " + limit
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func printStatsTable(w io.Writer, stats []GuestStats) {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tCPUS\tCPU%\tMEM USAGE / LIMIT\tPIDS\tSESSIONS")
	for _, s := range stats {
		cpuStr := "-"
		pidsStr := "-"
		sessStr := "-"
		if s.State == "running" {
			cpuStr = fmt.Sprintf("%.1f%%", s.CPUPercent)
			pidsStr = strconv.Itoa(s.PIDs)
			sessStr = strconv.Itoa(s.Sessions)
		}
		memStr := formatMem(s.MemoryUsed, s.MemoryLimit)
		if s.State != "running" {
			memStr = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			s.Name, s.State, s.CPUs, cpuStr, memStr, pidsStr, sessStr)
	}
	tw.Flush()
}

func (a *app) guestStats() *cobra.Command {
	var watch bool
	cmd := &cobra.Command{
		Use:               "stats [NAME]",
		Short:             "Display guest resource usage statistics",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.guestArgs,
		RunE: func(c *cobra.Command, args []string) error {
			if err := a.load(c.Context()); err != nil {
				return err
			}
			var targets []*guest.Guest
			if len(args) > 0 {
				g, err := a.get(c.Context(), args[0])
				if err != nil {
					return err
				}
				targets = []*guest.Guest{g}
			} else {
				var err error
				targets, err = a.mgr.All()
				if err != nil {
					return err
				}
			}
			if len(targets) == 0 {
				return fmt.Errorf("no guests defined (run \"antic guest create <name>\")")
			}

			// If not watch mode, sample over a short window and output single snapshot
			if !watch {
				prev := make(map[string]sample)
				for _, g := range targets {
					prev[g.Name()] = readGuestSample(c.Context(), g)
				}
				time.Sleep(150 * time.Millisecond)
				var stats []GuestStats
				for _, g := range targets {
					curr := readGuestSample(c.Context(), g)
					stats = append(stats, computeStats(g.Name(), prev[g.Name()], curr))
				}
				if a.json {
					enc := json.NewEncoder(os.Stdout)
					enc.SetIndent("", "  ")
					return enc.Encode(stats)
				}
				printStatsTable(os.Stdout, stats)
				return nil
			}

			// Watch mode: continuously stream updates
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(sigCh)

			prev := make(map[string]sample)
			for _, g := range targets {
				prev[g.Name()] = readGuestSample(c.Context(), g)
			}

			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()

			// Print initial snapshot immediately
			time.Sleep(100 * time.Millisecond)
			var initialStats []GuestStats
			for _, g := range targets {
				curr := readGuestSample(c.Context(), g)
				initialStats = append(initialStats, computeStats(g.Name(), prev[g.Name()], curr))
				prev[g.Name()] = curr
			}
			if a.json {
				enc := json.NewEncoder(os.Stdout)
				_ = enc.Encode(initialStats)
			} else {
				fmt.Print("\033[H\033[2J")
				printStatsTable(os.Stdout, initialStats)
			}

			for {
				select {
				case <-c.Context().Done():
					return nil
				case <-sigCh:
					return nil
				case <-ticker.C:
					var stats []GuestStats
					for _, g := range targets {
						curr := readGuestSample(c.Context(), g)
						stats = append(stats, computeStats(g.Name(), prev[g.Name()], curr))
						prev[g.Name()] = curr
					}
					if a.json {
						enc := json.NewEncoder(os.Stdout)
						_ = enc.Encode(stats)
					} else {
						fmt.Print("\033[H\033[2J")
						printStatsTable(os.Stdout, stats)
					}
				}
			}
		},
	}
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "continuously stream resource statistics")
	a.jsonFlag(cmd)
	return cmd
}
