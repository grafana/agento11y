package clihelp

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"golang.org/x/term"
)

type Row struct{ Name, Description string }
type Section struct {
	Title string
	Rows  []Row
}
type Page struct {
	Command  string
	Summary  string
	Usage    []string
	Sections []Section
	Examples []Row
}

type Renderer struct {
	writer io.Writer
	styles *lipgloss.Renderer
	width  int
}

func New(w io.Writer) *Renderer {
	width := 80
	styled := false
	if f, ok := w.(interface{ Fd() uintptr }); ok {
		fd := int(f.Fd())
		styled = term.IsTerminal(fd)
		if columns, _, err := term.GetSize(fd); err == nil && columns > 0 {
			width = columns
		}
	}
	r := lipgloss.NewRenderer(w)
	if !styled || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		r.SetColorProfile(termenv.Ascii)
	}
	return &Renderer{writer: w, styles: r, width: width}
}

func (r *Renderer) Heading(text string) string {
	return r.styles.NewStyle().Bold(true).Foreground(lipgloss.Color("#FF671D")).Render(text)
}
func (r *Renderer) Name(text string) string { return r.styles.NewStyle().Bold(true).Render(text) }
func (r *Renderer) Success(text string) string {
	return r.styles.NewStyle().Foreground(lipgloss.Color("#73BF69")).Render(text)
}
func (r *Renderer) Plain() { r.styles.SetColorProfile(termenv.Ascii) }

func (r *Renderer) Warning(text string) string {
	return r.styles.NewStyle().Foreground(lipgloss.Color("#FF9830")).Render(text)
}

func (r *Renderer) Error(text string) string {
	return r.styles.NewStyle().Bold(true).Foreground(lipgloss.Color("#F2495C")).Render(text)
}

func (r *Renderer) Detail(text string) string { return r.styles.NewStyle().Faint(true).Render(text) }

func (r *Renderer) Render(page Page) {
	_, _ = fmt.Fprintf(r.writer, "%s\n\n%s\n\n%s\n", r.Heading(page.Command), page.Summary, r.Heading("Usage:"))
	for _, usage := range page.Usage {
		_, _ = fmt.Fprintf(r.writer, "  %s\n", usage)
	}
	for _, section := range page.Sections {
		if len(section.Rows) == 0 {
			continue
		}
		_, _ = fmt.Fprintf(r.writer, "\n%s\n", r.Heading(section.Title+":"))
		r.Rows(section.Rows)
	}
	if len(page.Examples) > 0 {
		_, _ = fmt.Fprintf(r.writer, "\n%s\n", r.Heading("Examples:"))
		r.Rows(page.Examples)
	}
}

func (r *Renderer) Rows(rows []Row) {
	width := 0
	for _, row := range rows {
		width = max(width, lipgloss.Width(row.Name))
	}
	for _, row := range rows {
		if r.width < width+24 || strings.Contains(row.Description, "\n") {
			_, _ = fmt.Fprintf(r.writer, "  %s\n", r.Name(row.Name))
			if row.Description != "" {
				_, _ = fmt.Fprintf(r.writer, "    %s\n", strings.ReplaceAll(row.Description, "\n", "\n    "))
			}
		} else {
			_, _ = fmt.Fprintf(r.writer, "  %s%s  %s\n", r.Name(row.Name), strings.Repeat(" ", width-lipgloss.Width(row.Name)), row.Description)
		}
	}
}

func UsageError(w io.Writer, page Page, message string) {
	_, _ = fmt.Fprintf(w, "%s: %s\n", page.Command, message)
	for _, usage := range page.Usage {
		_, _ = fmt.Fprintf(w, "usage: %s\n", usage)
	}
	_, _ = fmt.Fprintf(w, "Run `%s --help` for help.\n", page.Command)
}

func Flags(fs *flag.FlagSet, excluded map[string]bool, names map[string]string) []Row {
	var rows []Row
	fs.VisitAll(func(f *flag.Flag) {
		if excluded[f.Name] {
			return
		}
		valueName, description := flag.UnquoteUsage(f)
		if name, ok := names[f.Name]; ok {
			valueName = name
		}
		name := "--" + f.Name
		if valueName != "" {
			name += " " + valueName
		}
		if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" {
			description += " (default: " + strconv.Quote(f.DefValue) + ")"
		}
		rows = append(rows, Row{Name: name, Description: description})
	})
	return append(rows, Row{Name: "--help, -h", Description: "Show help."})
}
