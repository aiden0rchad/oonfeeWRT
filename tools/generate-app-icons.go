//go:build ignore

// Regenerate the original, code-drawn application icons from the repository root:
// go run tools/generate-app-icons.go
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
)

func main() {
	for _, size := range []int{192, 512} {
		img := image.NewRGBA(image.Rect(0, 0, size, size))
		draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{0x18, 0x56, 0x9f, 255}), image.Point{}, draw.Src)
		// Keep the mark inside the maskable icon safe zone. Geometry matches the
		// project's existing bar-based favicon; no external artwork is included.
		for i, top := range []int{38, 29, 21} {
			r := image.Rect((19+i*10)*size/64, top*size/64, (25+i*10)*size/64, 46*size/64)
			draw.Draw(img, r, image.NewUniform(color.White), image.Point{}, draw.Src)
		}
		draw.Draw(img, image.Rect(19*size/64, 16*size/64, 45*size/64, 19*size/64),
			image.NewUniform(color.RGBA{0x63, 0xd0, 0xc7, 255}), image.Point{}, draw.Src)
		path := fmt.Sprintf("ui/public/app-icon-%d.png", size)
		f, err := os.Create(path)
		if err != nil { panic(err) }
		if err := png.Encode(f, img); err != nil { f.Close(); panic(err) }
		if err := f.Close(); err != nil { panic(err) }
	}
}
