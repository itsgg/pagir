// Package qr draws a QR code in a terminal with half blocks, two modules per
// character row. Light modules are drawn, dark ones left as background, so on
// a dark terminal it reads as dark-on-light, the way scanners expect.
package qr

import (
	"strings"

	"rsc.io/qr"
)

const quiet = 2 // modules of light border; scanners want some, terminals are small

// Render returns text's QR code as lines of half blocks.
func Render(text string) (string, error) {
	code, err := qr.Encode(text, qr.L)
	if err != nil {
		return "", err
	}
	light := func(x, y int) bool { return !code.Black(x, y) }
	var b strings.Builder
	for y := -quiet; y < code.Size+quiet; y += 2 {
		for x := -quiet; x < code.Size+quiet; x++ {
			top, bottom := light(x, y), light(x, y+1)
			if y+1 >= code.Size+quiet {
				bottom = false
			}
			switch {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}
