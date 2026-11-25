package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

func newRawCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "raw <endpoint> [json-body|-]",
		Short: "Call any /api/v3 endpoint directly (escape hatch for unsupported operations)",
		Example: `  notion raw getSpaces '{}'
  echo '{"query":"roadmap","limit":5}' | notion raw search -`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			if err := e.applySpaceFlag(); err != nil {
				return err
			}
			body := "{}"
			if len(args) == 2 {
				body = args[1]
				if body == "-" {
					b, err := io.ReadAll(os.Stdin)
					if err != nil {
						return err
					}
					body = string(b)
				}
			}
			var v any
			if err := json.Unmarshal([]byte(body), &v); err != nil {
				return fmt.Errorf("body is not valid JSON: %w", err)
			}
			data, err := e.client.Raw(e.ctx, args[0], v)
			if err != nil {
				return err
			}
			var out any
			if json.Unmarshal(data, &out) == nil {
				return printJSON(out)
			}
			_, err = os.Stdout.Write(data)
			return err
		},
	}
}
