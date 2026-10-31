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
	"testing"
	"time"

	"github.com/antchfx/xmlquery"
	"github.com/sibprogrammer/xq/internal/utils"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
)

func execute(cmd *cobra.Command, args ...string) (string, error) {
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	if len(args) > 0 {
		cmd.SetArgs(args)
	} else {
		cmd.SetArgs([]string{})
	}

	err := cmd.Execute()

	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		_ = f.Value.Set(f.DefValue)
	})

	return strings.TrimSpace(buf.String()), err
}

func TestRootCmd(t *testing.T) {
	command := NewRootCmd()
	InitFlags(command)

	var output string
	var err error
	xmlFilePath := filepath.Join("..", "test", "data", "xml", "unformatted.xml")
	formattedXmlFilePath := filepath.Join("..", "test", "data", "xml", "formatted.xml")
	htmlFilePath := filepath.Join("..", "test", "data", "html", "unformatted.html")
	jsonFilePath := filepath.Join("..", "test", "data", "json", "unformatted.json")

	output, err = execute(command)
	assert.Nil(t, err)
	assert.Contains(t, output, "Usage:")

	output, err = execute(command, "--in-place", formattedXmlFilePath)
	assert.Nil(t, err)
	assert.Equal(t, output, "")

	output, err = execute(command, xmlFilePath)
	assert.Nil(t, err)
	assert.Contains(t, output, "This is not a real user")

	output, err = execute(command, "--no-color", xmlFilePath)
	assert.Nil(t, err)
	assert.Contains(t, output, "first_name")

	output, err = execute(command, "--indent", "0", xmlFilePath)
	assert.Nil(t, err)
	assert.NotContains(t, output, "\n")

	output, err = execute(command, jsonFilePath)
	assert.Nil(t, err)
	assert.Contains(t, output, "{")

	output, err = execute(command, "--no-pager", xmlFilePath)
	assert.Nil(t, err)
	assert.Contains(t, output, "first_name")

	output, err = execute(command, "--tab", xmlFilePath)
	assert.Nil(t, err)
	assert.Contains(t, output, "\t")

	output, err = execute(command, "-m", htmlFilePath)
	assert.Nil(t, err)
	assert.Contains(t, output, "<html>")

	output, err = execute(command, "-q", "body > p", htmlFilePath)
	assert.Nil(t, err)
	assert.Contains(t, output, "text")

	output, err = execute(command, "-x", "/user/@status", xmlFilePath)
	assert.Nil(t, err)
	assert.Contains(t, output, "active")

	output, err = execute(command, "--no-color", "-x", "/user/@status", xmlFilePath)
	assert.Nil(t, err)
	assert.Contains(t, output, "active")

	output, err = execute(command, "--color", "-x", "/user/@status", xmlFilePath)
	assert.Nil(t, err)
	assert.Contains(t, output, "active")

	output, err = execute(command, "-x", "//first_name", xmlFilePath)
	assert.Nil(t, err)
	assert.Contains(t, output, "John")

	_, err = execute(command, "-x", "//missing", xmlFilePath)
	assert.Nil(t, err)

	_, err = execute(command, "-e", "//missing", xmlFilePath)
	assert.Nil(t, err)

	_, err = execute(command, "-q", "body > missing", htmlFilePath)
	assert.Nil(t, err)

	_, err = execute(command, "-s", "-x", "//missing", xmlFilePath)
	assert.ErrorIs(t, err, utils.ErrNoMatch)

	_, err = execute(command, "-s", "-e", "//missing", xmlFilePath)
	assert.ErrorIs(t, err, utils.ErrNoMatch)

	_, err = execute(command, "-s", "-q", "body > missing", htmlFilePath)
	assert.ErrorIs(t, err, utils.ErrNoMatch)

	output, err = execute(command, "-s", "-x", "//first_name", xmlFilePath)
	assert.Nil(t, err)
	assert.Contains(t, output, "John")

	_, err = execute(command, "nonexistent.xml")
	assert.ErrorContains(t, err, "no such file or directory")

	_, err = execute(command, "--indent", "-1", xmlFilePath)
	assert.ErrorContains(t, err, "indent should be")

	_, err = execute(command, "--indent", "incorrect", xmlFilePath)
	assert.ErrorContains(t, err, "invalid argument")
}

func TestCDATASupport(t *testing.T) {
	input := "<root><![CDATA[1 & 2]]></root>"
	doc, err := xmlquery.Parse(strings.NewReader(input))
	assert.Nil(t, err)

	result := utils.NodeToJSON(doc, 10)
	expected := map[string]interface{}{"root": "1 & 2"}

	assert.Equal(t, expected, result)
}

func TestProcessAsJSON(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		contentType utils.ContentType
		flags       map[string]interface{}
		expected    map[string]interface{}
		wantErr     bool
	}{
		{
			name:        "Simple XML",
			input:       "<root><child>value</child></root>",
			contentType: utils.ContentXml,
			expected: map[string]interface{}{
				"root": map[string]interface{}{
					"child": "value",
				},
			},
		},
		{name: "Simple JSON",
			input:       `{"root": {"child": "value"}}`,
			contentType: utils.ContentJson,
			expected: map[string]interface{}{
				"root": map[string]interface{}{
					"child": "value",
				},
			},
		},
		{
			name:        "Simple HTML",
			input:       "<html><body><p>text</p></body></html>",
			contentType: utils.ContentHtml,
			expected: map[string]interface{}{
				"html": map[string]interface{}{
					"body": map[string]interface{}{
						"p": "text",
					},
				},
			},
		},
		{
			name:        "Plain text",
			input:       "text",
			contentType: utils.ContentText,
			expected: map[string]interface{}{
				"text": "text",
			},
		},
		{
			name:    "invalid input",
			input:   "thinking>\nI'll analyze each command and its output:\n</thinking>",
			wantErr: true,
		},
		{
			name: "combined",
			expected: map[string]interface{}{
				"#text":    "Thank you\nBye.",
				"thinking": "1. woop",
			},
			input: `Thank you
<thinking>
1. woop
</thinking>

Bye.`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set up flags
			flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
			flags.Bool("compact", false, "")
			flags.Int("depth", -1, "")
			for name, v := range tt.flags {
				_ = flags.Set(name, fmt.Sprint(v))
			}

			reader := strings.NewReader(tt.input)
			var output bytes.Buffer

			err := processAsJSON(flags, reader, &output, tt.contentType)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)

				var resultMap map[string]interface{}
				err = json.Unmarshal(output.Bytes(), &resultMap)
				assert.NoError(t, err)

				assert.Equal(t, tt.expected, resultMap)
			}
		})
	}
}

func TestProcessingErrorIsNotMaskedByLaterSuccess(t *testing.T) {
	badFile := filepath.Join(t.TempDir(), "bad.xml")
	assert.NoError(t, os.WriteFile(badFile, []byte("<root><unclosed>"), 0600))

	goodFile := filepath.Join("..", "test", "data", "xml", "formatted.xml")

	command := NewRootCmd()
	InitFlags(command)

	output, err := execute(command, "--no-pager", badFile, goodFile)

	assert.Error(t, err)
	assert.Contains(t, output, "Error:")
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("simulated downstream write failure")
}

func TestDownstreamWriteFailureStopsProducer(t *testing.T) {
	command := NewRootCmd()
	InitFlags(command)

	errBuf := new(bytes.Buffer)
	command.SetOut(failingWriter{})
	command.SetErr(errBuf)
	command.SetArgs([]string{"--no-pager", filepath.Join("..", "test", "data", "xml", "formatted.xml")})

	done := make(chan error, 1)
	go func() {
		done <- command.Execute()
	}()

	select {
	case err := <-done:
		assert.ErrorContains(t, err, "simulated downstream write failure")
	case <-time.After(5 * time.Second):
		t.Fatal("command hung after the downstream writer failed")
	}
}

func TestPagerEarlyExitWithUnfinishedInput(t *testing.T) {
	t.Setenv("XQ_PAGER", "true")

	command := NewRootCmd()
	InitFlags(command)

	// The input pipe is never closed and only carries a partial document:
	// the input stream has not ended when the pager exits.
	inReader, inWriter := io.Pipe()
	defer func() { _ = inWriter.Close() }()
	go func() {
		_, _ = inWriter.Write([]byte("<abc>"))
	}()

	outBuf := new(bytes.Buffer)
	errBuf := new(bytes.Buffer)
	command.SetIn(inReader)
	command.SetOut(outBuf)
	command.SetErr(errBuf)
	command.SetArgs([]string{"--no-pager=false"})

	done := make(chan error, 1)
	go func() {
		done <- command.Execute()
	}()

	select {
	case err := <-done:
		assert.NoError(t, err)
		assert.Empty(t, errBuf.String())
	case <-time.After(5 * time.Second):
		t.Fatal("command did not exit after the pager closed while input was still open")
	}
}
