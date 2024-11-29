// The following code is based on code from GolangCI.
// Source: https://github.com/golangci-lint/pkg/golinters/lll/lll.go
// License: GNU

package linters

import (
	"bufio"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/golangci/plugin-module-register/register"
	"golang.org/x/tools/go/analysis"
)

const (
	linterName               = "ll"
	goCommentDirectivePrefix = "//go:"

	defaultMaxLineLen       = 80
	defaultTabWidthInSpaces = 8
	defaultLogRegex         = `^\s*.*(L|l)og\.`
)

// LLConfig is the configuration for the ll linter.
type LLConfig struct {
	LineLength int    `json:"line-length"`
	TabWidth   int    `json:"tab-width"`
	LogRegex   string `json:"log-regex"`
}

// New creates a new LLPlugin from the given settings. It satisfies the
// signature required by the golangci-lint linter for plugins.
func New(settings any) (register.LinterPlugin, error) {
	cfg, err := register.DecodeSettings[LLConfig](settings)
	if err != nil {
		return nil, err
	}

	// Fill in default config values if they are not set.
	if cfg.LineLength == 0 {
		cfg.LineLength = defaultMaxLineLen
	}
	if cfg.TabWidth == 0 {
		cfg.TabWidth = defaultTabWidthInSpaces
	}
	if cfg.LogRegex == "" {
		cfg.LogRegex = defaultLogRegex
	}

	return &LLPlugin{cfg: cfg}, nil
}

// LLPlugin is a golangci-linter plugin that can be used to check that code line
// lengths do not exceed a certain limit.
type LLPlugin struct {
	cfg LLConfig
}

// BuildAnalyzers creates the analyzers for the ll linter.
//
// NOTE: This is part of the register.LinterPlugin interface.
func (l *LLPlugin) BuildAnalyzers() ([]*analysis.Analyzer, error) {
	return []*analysis.Analyzer{
		{
			Name: linterName,
			Doc:  "Reports long lines",
			Run:  l.run,
		},
	}, nil
}

// GetLoadMode returns the load mode for the ll linter.
//
// NOTE: This is part of the register.LinterPlugin interface.
func (l *LLPlugin) GetLoadMode() string {
	return register.LoadModeSyntax
}

func (l *LLPlugin) run(pass *analysis.Pass) (any, error) {
	var (
		spaces   = strings.Repeat(" ", l.cfg.TabWidth)
		logRegex = regexp.MustCompile(l.cfg.LogRegex)
	)

	for _, f := range pass.Files {
		fileName := getFileName(pass, f)

		issues, err := getLLLIssuesForFile(
			fileName, l.cfg.LineLength, spaces, logRegex,
		)
		if err != nil {
			return nil, err
		}

		file := pass.Fset.File(f.Pos())
		for _, issue := range issues {
			pos := file.LineStart(issue.pos.Line)

			pass.Report(analysis.Diagnostic{
				Pos:      pos,
				End:      0,
				Category: linterName,
				Message:  issue.text,
			})
		}

	}

	return nil, nil
}

type issue struct {
	pos  token.Position
	text string
}

func getLLLIssuesForFile(filename string, maxLineLen int,
	tabSpaces string, logRegex *regexp.Regexp) ([]*issue, error) {

	f, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("can't open file %s: %w", filename, err)
	}
	defer f.Close()

	var (
		res                []*issue
		lineNumber         int
		multiImportEnabled bool
		multiLinedLog      bool
	)

	// Scan over each line.
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lineNumber++

		// Replace all tabs with spaces.
		line := scanner.Text()
		line = strings.ReplaceAll(line, "\t", tabSpaces)

		// Ignore any //go: directives since these cant be wrapped onto
		// a new line.
		if strings.HasPrefix(line, goCommentDirectivePrefix) {
			continue
		}

		// We never want the linter to run on imports since these cannot
		// be wrapped onto a new line. If this is a single line import
		// we can skip the line entirely. If this is a multi-line import
		// skip until the closing bracket.
		//
		// NOTE: We trim the line space around the line here purely for
		// the purpose of being able to test this part of the linter
		// without the risk of the `gosimports` tool reformatting the
		// test case and removing the import.
		if strings.HasPrefix(strings.TrimSpace(line), "import") {
			multiImportEnabled = strings.HasSuffix(line, "(")
			continue
		}

		// If we have marked the start of a multi-line import, we should
		// skip until the closing bracket of the import block.
		if multiImportEnabled {
			if line == ")" {
				multiImportEnabled = false
			}

			continue
		}

		// Check if the line matches the log pattern.
		if logRegex.MatchString(line) {
			multiLinedLog = !strings.HasSuffix(line, ")")
			continue
		}

		if multiLinedLog {
			// Check for the end of a multiline log call.
			if strings.HasSuffix(line, ")") {
				multiLinedLog = false
			}

			continue
		}

		// Otherwise, we can check the length of the line and report if
		// it exceeds the maximum line length.
		lineLen := utf8.RuneCountInString(line)
		if lineLen > maxLineLen {
			res = append(res, &issue{
				pos: token.Position{
					Filename: filename,
					Line:     lineNumber,
				},
				text: fmt.Sprintf("the line is %d "+
					"characters long, which exceeds the "+
					"maximum of %d characters.", lineLen,
					maxLineLen),
			})
		}
	}

	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) &&
			maxLineLen < bufio.MaxScanTokenSize {

			// scanner.Scan() might fail if the line is longer than
			// bufio.MaxScanTokenSize. In the case where the
			// specified maxLineLen is smaller than
			// bufio.MaxScanTokenSize we can return this line as a
			// long line instead of returning an error. The reason
			// for this change is that this case might happen with
			// autogenerated files. The go-bindata tool for instance
			// might generate a file with a very long line. In this
			// case, as it's an auto generated file, the warning
			// returned by lll will be ignored.
			// But if we return a linter error here, and this error
			// happens for an autogenerated file the error will be
			// discarded (fine), but all the subsequent errors for
			// lll will be discarded for other files, and we'll miss
			// legit error.
			res = append(res, &issue{
				pos: token.Position{
					Filename: filename,
					Line:     lineNumber,
					Column:   1,
				},
				text: fmt.Sprintf("line is more than "+
					"%d characters",
					bufio.MaxScanTokenSize),
			})
		} else {
			return nil, fmt.Errorf("can't scan file %s: %w",
				filename, err)
		}
	}

	return res, nil
}

func getFileName(pass *analysis.Pass, file *ast.File) string {
	fileName := pass.Fset.PositionFor(file.Pos(), true).Filename
	ext := filepath.Ext(fileName)
	if ext != "" && ext != ".go" {
		// The position has been adjusted to a non-go file,
		// revert to original file.
		position := pass.Fset.PositionFor(file.Pos(), false)
		fileName = position.Filename
	}

	return fileName
}

func init() {
	// Register the linter with the plugin module register.
	register.Plugin(linterName, New)
}
