package cmd

import (
	"os"

	"github.com/roffe/gocan/pkg/t5"
	"github.com/spf13/cobra"
)

var t5flashCmd = &cobra.Command{
	Use:   "flash <filename>",
	Short: "flash ECU",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		c, err := initCAN(ctx, 0xC)
		if err != nil {
			return err
		}
		defer c.Close()

		tr := t5.New(c)

		bin, err := os.ReadFile(args[0])
		if err != nil {
			return err
		}

		ecutype, err := tr.DetermineECU(ctx)
		if err != nil {
			return err
		}

		if err := tr.EraseECU(ctx); err != nil {
			return err
		}

		if err := tr.FlashECU(ctx, ecutype, bin); err != nil {
			return err
		}

		if err := tr.ResetECU(ctx); err != nil {
			return err
		}

		return nil
	},
}

func init() {
	t5Cmd.AddCommand(t5flashCmd)
}