package cli

import (
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/subdiox/notion-cli/internal/notion"
)

func newFileCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "file", Aliases: []string{"files"}, Short: "Download and upload files/images"}

	var out string
	var urlOnly bool
	download := &cobra.Command{
		Use:     "download <block-url|id>",
		Aliases: []string{"get"},
		Short:   "Download the file of an image/file/pdf/video/audio block (or print a signed URL)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			_, _, anchor, _ := notion.ParseRef(args[0])
			ref := args[0]
			if anchor != "" {
				ref = anchor // page URL with #block anchor
			}
			b, err := e.target(ref, "block")
			if err != nil {
				return err
			}
			src := notion.BlockFileSource(b)
			if src == "" {
				return fmt.Errorf("block %s (%s) has no file", b.ID, b.Type())
			}
			u, err := e.store().FileURL(e.ctx, b, src)
			if err != nil {
				return err
			}
			if urlOnly {
				if gf.json {
					return printJSON(map[string]any{"url": u, "source": src})
				}
				fmt.Println(u)
				return nil
			}
			name := notion.FileName(src, b.ID)
			dst := out
			if dst == "" {
				dst = name
			} else if st, err := os.Stat(dst); err == nil && st.IsDir() {
				dst = filepath.Join(dst, name)
			}
			f, err := os.Create(dst)
			if err != nil {
				return err
			}
			defer f.Close()
			ctype, err := e.client.Download(e.ctx, u, f)
			if err != nil {
				os.Remove(dst)
				return err
			}
			if gf.json {
				return printJSON(map[string]any{"path": dst, "content_type": ctype})
			}
			fmt.Println(dst)
			return nil
		},
	}
	download.Flags().StringVarP(&out, "output", "o", "", "output file or directory")
	download.Flags().BoolVar(&urlOnly, "url", false, "only print a temporary signed URL")

	var to, after, caption string
	var dry bool
	upload := &cobra.Command{
		Use:   "upload <path>... --to <page>",
		Short: "Upload files and append them to a page as image/video/audio/pdf/file blocks",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			if to == "" {
				return fmt.Errorf("--to is required")
			}
			parent, err := e.target(to, "block")
			if err != nil {
				return err
			}
			var specs []*notion.BlockSpec
			for _, p := range args {
				abs, err := filepath.Abs(p)
				if err != nil {
					return err
				}
				specs = append(specs, &notion.BlockSpec{Type: mediaTypeFor(abs), LocalFile: abs, Props: map[string]any{}})
				if caption != "" {
					specs[len(specs)-1].Props["caption"] = notion.ParseInline(caption)
				}
			}
			w := e.newWriter(parent.SpaceID, "uploadFiles")
			if !dry {
				if err := w.PrepareUploads(specs, notion.Pointer{Table: "block", ID: parent.ID, SpaceID: parent.SpaceID}); err != nil {
					return err
				}
			}
			afterID := ""
			if after != "" {
				if afterID, _, _, err = notion.ParseRef(after); err != nil {
					return err
				}
			}
			ids := w.Create(specs, parent.ID, "block", afterID)
			if err := e.commit(w, dry); err != nil {
				return err
			}
			if dry {
				return nil
			}
			return printWriteResult("uploaded to", parent.ID, map[string]any{"block_ids": ids})
		},
	}
	upload.Flags().StringVar(&to, "to", "", "page or block to append to")
	upload.Flags().StringVar(&after, "after", "", "insert after this child block")
	upload.Flags().StringVar(&caption, "caption", "", "caption (Markdown)")
	upload.Flags().BoolVar(&dry, "dry-run", false, "print the transaction instead of sending it")

	cmd.AddCommand(download, upload)
	return cmd
}

func mediaTypeFor(path string) string {
	ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	switch {
	case strings.HasPrefix(ct, "image/"):
		return "image"
	case strings.HasPrefix(ct, "video/"):
		return "video"
	case strings.HasPrefix(ct, "audio/"):
		return "audio"
	case ct == "application/pdf":
		return "pdf"
	}
	return "file"
}
