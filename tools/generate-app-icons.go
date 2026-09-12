//go:build ignore

// Render the checked-in SVG master to maskable application icons:
// go run tools/generate-app-icons.go
// Requires ImageMagick 7 (magick); not needed for ordinary application builds.
package main

import (
	"fmt"
	"os"
	"os/exec"
)

func main() {
	if _, err := exec.LookPath("magick"); err != nil {
		panic("install ImageMagick 7 to regenerate the application icons")
	}
	license, err := os.ReadFile("third_party/lucide-orbit/LICENSE")
	if err != nil {
		panic(err)
	}
	for _, size := range []int{192, 512} {
		// Supersample the same vector master, then reduce for smooth thin strokes.
		// Explicit MSVG selects the native renderer, not a platform SVG delegate.
		cmd := exec.Command("magick",
			"-background", "none", "-density", "2304", "MSVG:ui/public/app-icon.svg",
			"-resize", fmt.Sprintf("%dx%d", size, size),
			"-set", "comment", string(license), "-define", "png:exclude-chunk=date,time",
			"PNG24:"+fmt.Sprintf("ui/public/app-icon-%d.png", size))
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			panic(err)
		}
	}
}
