package cli

import (
	"fmt"

	"github.com/RNG999/antic/internal/proto"
	"github.com/spf13/cobra"
)

func (a *app) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use: "version", Short: "Print antic's version and guest protocol", Args: cobra.NoArgs,
		Run: func(*cobra.Command, []string) { fmt.Printf("antic %s (guest protocol %d)\n", version, proto.Version) },
	}
}
