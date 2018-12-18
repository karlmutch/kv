// This needs significant optimization, mainly to reduce memory
// allocation.

// Package kvlog provides a writer intended for use with the
// Go standard library log package.
package kvlog

import (
	"bytes"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/jjeffery/kv"
	"github.com/mattn/go-colorable"
	"golang.org/x/crypto/ssh/terminal"
)

var (
	// headerRE matches the possible combinations of date and/or time that the
	// stdlib log package will produce. No attempt is made to match the file/line
	// but it could be added.
	headerRE = regexp.MustCompile(`^(\d{4}/\d\d/\d\d )?(\d\d:\d\d:\d\d(\.\d{0,6})? )?`)

	// newline is the newline sequence. Originally changed if windows, but seems
	// to work fine as the same value for all operating systems.
	// TODO: change to a const.
	newline = "\n"

	// verbosePrefixes is a list of prefixes that indicate the message should only
	// be displayed in verbose mode
	verbosePrefixes = []string{
		"debug:",
		"trace:",
	}

	// error prefixes get color treatment
	errorPrefixes = [][]byte{
		[]byte("error:"),
	}

	whiteSpaceRE = regexp.MustCompile(`^\s+`)
	blackSpaceRE = regexp.MustCompile(`^[^\s,]+`)
)

// IsTerminal returns true if the writer is a terminal.
func IsTerminal(writer io.Writer) bool {
	if fder, ok := writer.(interface{ Fd() uintptr }); ok {
		fd := int(fder.Fd())
		return terminal.IsTerminal(fd)
	}
	return false
}

// Writer implements io.Writer and can be used as the writer for
// log.SetOutput.
type Writer struct {
	Out         io.Writer
	Width       func() int
	Verbose     bool
	colorOutput bool
	mutex       sync.Mutex
	origOut     io.Writer
}

// NewWriter returns a writer that can be used as a writer for the log.
func NewWriter(writer io.Writer) *Writer {
	w := &Writer{
		Out:     writer,
		origOut: writer,
	}
	const defaultWidth = 170
	if fder, ok := writer.(interface{ Fd() uintptr }); ok {
		fd := int(fder.Fd())

		if terminal.IsTerminal(fd) {
			w.Width = func() int {
				width, _, err := terminal.GetSize(fd)
				if err != nil {
					return defaultWidth
				}
				// return one less than the full width: on bit bash windows
				// if you print the full number of characters you get an ugly
				// line feed.
				return width - 1
			}
			if file, ok := writer.(*os.File); ok {
				w.Out = colorable.NewColorable(file)
				w.colorOutput = true
			}
		}
	}
	if w.Width == nil {
		w.Width = func() int {
			return defaultWidth
		}
	}
	return w
}

// NoColor suppresses any color output.
func (w *Writer) NoColor() *Writer {
	w.Out = w.origOut
	w.colorOutput = false
	return w
}

// messageText contains the message parts and rune count required to
// display each part.
type messageText struct {
	text           string
	textRuneCount  int
	totalRuneCount int
	keyvals        []keyvalText
}

// keyvalText contains the key/value text and the rune count required
// to display.
type keyvalText struct {
	text          string
	textRuneCount int
}

func (w *Writer) Write(p []byte) (int, error) {
	msg := kv.Parse(p)

	var prefix string
	{
		match := headerRE.FindStringSubmatch(msg.Text)
		if len(match) > 0 {
			prefix = match[0]
			msg.Text = msg.Text[len(prefix):]
		}
	}
	if !w.Verbose {
		for _, vp := range verbosePrefixes {
			if strings.HasPrefix(msg.Text, vp) {
				// suppress verbose messages
				return 0, nil
			}
		}
	}
	width := w.Width()
	var indent string
	if len(prefix) > 0 {
		indent = strings.Repeat(" ", len(prefix))
	} else {
		indent = "    "
	}

	msgText := messageText{
		text:          msg.Text,
		textRuneCount: utf8.RuneCountInString(msg.Text),
	}
	// totalRuneCount is the number of columns required to display the entire message on one line
	msgText.totalRuneCount = msgText.textRuneCount

	for i := 0; i < len(msg.List); i += 2 {
		keyval := kv.P(msg.List[i].(string), msg.List[i+1]).String()
		runeCount := utf8.RuneCountInString(keyval)
		msgText.keyvals = append(msgText.keyvals, keyvalText{
			text:          keyval,
			textRuneCount: runeCount,
		})
		// the totalRuneCount includes a "+1" for a space because
		// it is used to determine if the message will fit all by
		// itself on one line.
		msgText.totalRuneCount += runeCount + 1
	}

	var sb bytes.Buffer
	sb.WriteString(prefix)
	col := len(prefix)
	var needSpace int

	// TODO: check if the message text will not fit on one line, and if that is the case display the
	// text with line-wrapping (which will be slower).
	if col+msgText.textRuneCount > width || w.colorOutput {
		// this is where the message itself is too long to fit on one line, so we need to
		// line wrap
		in := []byte(msgText.text)
		for len(in) > 0 {
			ws := whiteSpaceRE.Find(in)
			if len(ws) > 0 {
				in = in[len(ws):]
			}
			var wsLen int
			if len(ws) > 0 {
				wsLen = 1
			}
			bs := blackSpaceRE.Find(in)
			if len(bs) > 0 {
				in = in[len(bs):]
			}
			bsLen := utf8.RuneCount(bs)

			// The black space RE will terminate before punctuation to handle very long
			// strings with no spaces but possibly punctuation. Detect if it has terminated
			// before punctuation, and if so include the punctuation char on the same line.
			var (
				punct    rune
				punctLen int
			)
			if len(in) > 0 {
				var size int
				punct, size = utf8.DecodeRune(in)
				if !unicode.IsSpace(punct) {
					punctLen = size
					in = in[size:]
				}
			}

			if bsLen+wsLen+punctLen+col > width {
				sb.WriteString(newline)
				sb.WriteString(indent)
				sb.Write(bs)
				col = len(indent) + bsLen
			} else {
				if len(ws) > 0 {
					sb.WriteRune(' ')
					col++
				}
				if w.colorOutput {
					var isError bool
					for _, p := range errorPrefixes {
						if len(bs) >= len(p) {
							if bytes.EqualFold(bs[:len(p)], p) {
								isError = true
								break
							}
						}
					}
					if isError {
						sb.WriteString("\x1b[0;31m")
						sb.Write(bs)
						sb.WriteString("\x1b[0m")
					} else {
						sb.Write(bs)
					}
				} else {
					sb.Write(bs)
				}

				col += bsLen
			}
			if punctLen > 0 {
				sb.WriteRune(punct)
				col += punctLen
			}
		}
		needSpace = 1
	} else if msgText.textRuneCount > 0 {
		if needSpace > 0 {
			sb.WriteRune(' ')
			col++
			needSpace = 0
		}
		sb.WriteString(msgText.text)
		col += msgText.textRuneCount
		needSpace = 1
	}

	for _, keyvalText := range msgText.keyvals {
		if keyvalText.textRuneCount+needSpace+col > width {
			sb.WriteString(newline)
			sb.WriteString(indent)
			col = len(indent)
			needSpace = 0
		}
		if needSpace > 0 {
			sb.WriteRune(' ')
			needSpace = 0
			col++
		}
		sb.WriteString(keyvalText.text)
		col += keyvalText.textRuneCount
		needSpace = 1
	}

	sb.WriteString(newline)

	w.mutex.Lock()
	n, err := w.Out.Write(sb.Bytes())
	w.mutex.Unlock()
	return n, err
}
