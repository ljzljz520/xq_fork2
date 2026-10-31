package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/antchfx/xmlquery"
	"github.com/sibprogrammer/xq/internal/utils"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Version information
var Version string

var rootCmd = NewRootCmd()

func NewRootCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "xq",
		Short:        "Command-line XML and HTML beautifier and content extractor",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var err error
			var readers []io.Reader
			var fileNames []string
			var indent string

			inPlace, _ := cmd.Flags().GetBool("in-place")

			if indent, err = getIndent(cmd.Flags()); err != nil {
				return err
			}
			if len(args) == 0 {
				stdin := cmd.InOrStdin()

				if stat, ok := stdin.(interface{ Stat() (os.FileInfo, error) }); ok {
					fileInfo, _ := stat.Stat()
					if (fileInfo.Mode() & os.ModeCharDevice) != 0 {
						_ = cmd.Help()
						return nil
					}
				}

				readers = append(readers, stdin)
			} else {
				for _, fileName := range args {
					fileNames = append(fileNames, fileName)
					f, err := os.Open(fileName)
					if err != nil {
						return err
					}
					defer func() {
						_ = f.Close()
					}()

					readers = append(readers, f)
				}
			}

			xPathQuery, singleNode := getXpathQuery(cmd.Flags())
			withTags, _ := cmd.Flags().GetBool("node")
			colors := getColorMode(cmd.Flags())

			options := utils.QueryOptions{
				WithTags: withTags,
				Indent:   indent,
				Colors:   colors,
			}

			cssQuery, _ := cmd.Flags().GetString("query")
			cssAttr, _ := cmd.Flags().GetString("attr")
			if cssAttr != "" && cssQuery == "" {
				return errors.New("query option (-q) is missed for attribute selection")
			}
			jsonOutputMode, _ := cmd.Flags().GetBool("json")

			if (xPathQuery != "" || cssQuery != "") && inPlace {
				return errors.New("in-place formatting is incompatible with nodes selection")
			}

			if inPlace {
				for i := range len(fileNames) {
					var content []byte
					fileName := fileNames[i]
					reader := readers[i]
					pr, pw := io.Pipe()

					go func() {
						err := processContent(reader, pw, cmd.Flags(), jsonOutputMode, indent, colors)
						_ = pw.CloseWithError(err)
					}()

					if content, err = io.ReadAll(pr); err != nil {
						return err
					}

					if err = os.WriteFile(fileName, content, 0600); err != nil {
						return err
					}
				}

				return nil
			}

			// Unified lifecycle for the normal output path. A producer
			// goroutine processes the inputs into the pipe, while the pager
			// drains it on the main goroutine. When the consumer finishes
			// (pager quit early, a downstream write failure, or all the input
			// was printed), the cancel channel is closed and the read end of
			// the pipe is aborted, so input reading stops and the producer
			// goroutine is always reclaimed before the command returns.
			pr, pw := io.Pipe()
			cancel := make(chan struct{})
			done := make(chan producerResult, 1)

			go func() {
				result := producerResult{}

				defer func() {
					_ = pw.Close()
					done <- result
				}()

				for _, reader := range readers {
					reader := newCancelableReader(reader, cancel, errConsumerClosed)

					var processErr error
					if xPathQuery != "" {
						processErr = utils.XPathQuery(reader, pw, xPathQuery, singleNode, options)
					} else if cssQuery != "" {
						processErr = utils.CSSQuery(reader, pw, cssQuery, cssAttr, options)
					} else {
						processErr = processContent(reader, pw, cmd.Flags(),
							jsonOutputMode, indent, colors)
					}

					switch {
					case processErr == nil:
						result.matched = true
					case errors.Is(processErr, utils.ErrNoMatch):
						// A file without matches is not a failure: keep
						// processing the remaining inputs.
					case errors.Is(processErr, errConsumerClosed):
						// The consumer went away: stop reading immediately.
						return
					default:
						select {
						case <-cancel:
							// The error was induced by the consumer going
							// away (it may be wrapped by a parser), ignore it.
							return
						default:
						}
						// The first real error sticks and must not be
						// overwritten by any later successful file.
						if result.err == nil {
							result.err = processErr
						}
					}
				}
			}()

			pagerErr := utils.PagerPrint(pr, cmd.OutOrStdout(), getPager(cmd.Flags()))

			// The consumer is done: unblock pending reads and writes, stop
			// the producer and wait until it is reclaimed.
			close(cancel)
			_ = pr.CloseWithError(errConsumerClosed)
			result := <-done

			if result.err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Error: %v\n", result.err)
				cmd.SilenceErrors = true
				return result.err
			}

			if pagerErr != nil {
				return pagerErr
			}

			strict, _ := cmd.Flags().GetBool("strict")
			if strict && (xPathQuery != "" || cssQuery != "") && !result.matched {
				cmd.SilenceErrors = true
				return utils.ErrNoMatch
			}

			return nil
		},
	}
}

// errConsumerClosed breaks the producer out of blocking reads and writes once
// the output consumer (pager or plain downstream writer) has finished.
var errConsumerClosed = errors.New("output consumer closed")

// producerResult is the producer goroutine's terminal state.
type producerResult struct {
	err     error
	matched bool
}

// cancelableReader wraps a reader so blocked and future Read calls return the
// given cause as soon as the cancel channel is closed. This stops input
// reading promptly when the consumer goes away, even if the input never
// reaches EOF. The goroutine waiting for an in-flight blocking read is
// released once data arrives or the underlying source is closed.
type cancelableReader struct {
	reader io.Reader
	cancel <-chan struct{}
	cause  error
}

func newCancelableReader(reader io.Reader, cancel <-chan struct{}, cause error) *cancelableReader {
	return &cancelableReader{reader: reader, cancel: cancel, cause: cause}
}

func (r *cancelableReader) Read(p []byte) (int, error) {
	select {
	case <-r.cancel:
		return 0, r.cause
	default:
	}

	type readResult struct {
		n   int
		err error
	}
	readDone := make(chan readResult, 1)

	go func() {
		n, err := r.reader.Read(p)
		readDone <- readResult{n: n, err: err}
	}()

	select {
	case <-r.cancel:
		return 0, r.cause
	case result := <-readDone:
		return result.n, result.err
	}
}

func InitFlags(cmd *cobra.Command) {
	homeDir, _ := os.UserHomeDir()
	configFile := filepath.Join(homeDir, ".xq")
	if err := utils.LoadConfig(configFile); err != nil {
		fmt.Printf("Error while reading the config file: %v\n", err)
		os.Exit(1)
	}

	cmd.Version = Version

	cmd.Flags().BoolP("help", "h", false, "Print this help message")
	cmd.Flags().BoolP("version", "v", false, "Print version information")
	cmd.PersistentFlags().StringP("xpath", "x", "", "Extract the node(s) from XML")
	cmd.PersistentFlags().StringP("extract", "e", "", "Extract a single node from XML")
	cmd.PersistentFlags().Bool("tab", utils.GetConfig().Tab, "Use tabs for indentation")
	cmd.PersistentFlags().Int("indent", utils.GetConfig().Indent,
		"Use the given number of spaces for indentation")
	cmd.PersistentFlags().Bool("no-color", utils.GetConfig().NoColor, "Disable colorful output")
	cmd.PersistentFlags().BoolP("color", "c", utils.GetConfig().Color,
		"Force colorful output")
	cmd.PersistentFlags().BoolP("html", "m", utils.GetConfig().Html, "Use HTML formatter")
	cmd.PersistentFlags().StringP("query", "q", "",
		"Extract the node(s) using CSS selector")
	cmd.PersistentFlags().StringP("attr", "a", "",
		"Extract an attribute value instead of node content for provided CSS query")
	cmd.PersistentFlags().BoolP("node", "n", utils.GetConfig().Node,
		"Return the node content instead of text")
	cmd.PersistentFlags().BoolP("json", "j", false, "Output the result as JSON")
	cmd.PersistentFlags().Bool("compact", false, "Compact JSON output (no indentation)")
	cmd.PersistentFlags().IntP("depth", "d", -1, "Maximum nesting depth for JSON output (-1 for unlimited)")
	cmd.PersistentFlags().BoolP("in-place", "i", false, "Format file in place")
	cmd.PersistentFlags().Bool("no-pager", utils.GetConfig().NoPager, "Disable pager for the output")
	cmd.PersistentFlags().BoolP("strict", "s", utils.GetConfig().Strict,
		"Exit with a non-zero code if the query yields no matches")
}

func Execute() {
	InitFlags(rootCmd)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func getIndent(flags *pflag.FlagSet) (string, error) {
	var indentWidth int
	var tabIndent bool
	var err error

	if indentWidth, err = flags.GetInt("indent"); err != nil {
		return "", err
	}
	if indentWidth < 0 || indentWidth > 8 {
		return "", errors.New("indent should be between 0-8 spaces")
	}

	indent := strings.Repeat(" ", indentWidth)

	if tabIndent, err = flags.GetBool("tab"); err != nil {
		return "", err
	}

	if tabIndent {
		indent = "\t"
	}

	return indent, nil
}

func getXpathQuery(flags *pflag.FlagSet) (query string, single bool) {
	if query, _ = flags.GetString("xpath"); query != "" {
		return query, false
	}

	query, _ = flags.GetString("extract")
	return query, true
}

func getPager(flags *pflag.FlagSet) string {
	noPager, _ := flags.GetBool("no-pager")
	if noPager {
		return ""
	}

	if pager, ok := os.LookupEnv("XQ_PAGER"); ok {
		return pager
	}

	return os.Getenv("PAGER")
}

func getColorMode(flags *pflag.FlagSet) int {
	inPlace, _ := flags.GetBool("in-place")
	if inPlace {
		return utils.ColorsDisabled
	}

	colors := utils.ColorsDefault

	disableColors, _ := flags.GetBool("no-color")
	if disableColors {
		colors = utils.ColorsDisabled
	}

	forcedColors, _ := flags.GetBool("color")
	if forcedColors {
		colors = utils.ColorsForced
	}

	return colors
}

func detectFormat(flags *pflag.FlagSet, origReader io.Reader) (utils.ContentType, io.Reader) {
	isHtmlFormatter, _ := flags.GetBool("html")
	if isHtmlFormatter {
		return utils.ContentHtml, origReader
	}

	buf := make([]byte, 10)
	length, err := origReader.Read(buf)
	if err != nil {
		return utils.ContentText, origReader
	}

	reader := io.MultiReader(bytes.NewReader(buf[:length]), origReader)

	if utils.IsJSON(string(buf)) {
		return utils.ContentJson, reader
	}

	if utils.IsHTML(string(buf)) {
		return utils.ContentHtml, reader
	}

	return utils.ContentXml, reader
}

func processContent(reader io.Reader, pw io.Writer, flags *pflag.FlagSet, jsonOutputMode bool, indent string, colors int) error {
	var err error
	var contentType utils.ContentType

	contentType, reader = detectFormat(flags, reader)

	if jsonOutputMode {
		err = processAsJSON(flags, reader, pw, contentType)
	} else {
		switch contentType {
		case utils.ContentHtml:
			err = utils.FormatHtml(reader, pw, indent, colors)
		case utils.ContentXml:
			err = utils.FormatXml(reader, pw, indent, colors)
		case utils.ContentJson:
			err = utils.FormatJson(reader, pw, indent, colors)
		default:
			err = fmt.Errorf("unknown content type: %v", contentType)
		}
	}

	return err
}

func processAsJSON(flags *pflag.FlagSet, reader io.Reader, w io.Writer, contentType utils.ContentType) error {
	var (
		jsonCompact bool
		jsonDepth   int
		result      interface{}
	)
	jsonCompact, _ = flags.GetBool("compact")
	if flags.Changed("depth") {
		jsonDepth, _ = flags.GetInt("depth")
	} else {
		jsonDepth = -1
	}

	switch contentType {
	case utils.ContentXml, utils.ContentHtml:
		sanitizedReader, _ := utils.SanitizeXmlVersion(reader)
		doc, err := xmlquery.Parse(sanitizedReader)
		if err != nil {
			return fmt.Errorf("error while parsing XML: %w", err)
		}
		result = utils.NodeToJSON(doc, jsonDepth)
	case utils.ContentJson:
		decoder := json.NewDecoder(reader)
		if err := decoder.Decode(&result); err != nil {
			return fmt.Errorf("error while parsing JSON: %w", err)
		}
	default:
		// Treat as plain text
		content, err := io.ReadAll(reader)
		if err != nil {
			return fmt.Errorf("error while reading content: %w", err)
		}
		result = map[string]interface{}{
			"text": string(content),
		}
	}
	jsonData, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("error while marshaling JSON: %w", err)
	}
	indent := ""
	if !jsonCompact {
		indent = "  "
	}
	colors := getColorMode(flags)
	return utils.FormatJson(bytes.NewReader(jsonData), w, indent, colors)
}
