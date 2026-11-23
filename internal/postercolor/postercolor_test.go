package postercolor

import (
	"image"
	"image/color"
	"net/url"
	"testing"
)

func fill(w, h int, at func(x, y int) color.Color) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, at(x, y))
		}
	}
	return img
}

func TestPickFindsTheMainColour(t *testing.T) {
	// Mostly black with a large navy block and a small red one: navy wins.
	img := fill(200, 300, func(x, y int) color.Color {
		switch {
		case y > 100 && y < 260:
			return color.RGBA{0x1f, 0x3a, 0x88, 0xff}
		case y < 20:
			return color.RGBA{0xd0, 0x20, 0x20, 0xff}
		}
		return color.Black
	})
	if got := Pick(img); got != "#1f3a88" {
		t.Fatalf("Pick = %s, want #1f3a88", got)
	}
}

func TestPickGreyPoster(t *testing.T) {
	img := fill(100, 150, func(x, y int) color.Color { return color.Gray{0x80} })
	if got := Pick(img); got != "#808080" {
		t.Fatalf("Pick = %s, want #808080", got)
	}
}

func TestOnlyCatalogueHosts(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://image.tmdb.org/t/p/w300/x.jpg":         true,
		"https://covers.openlibrary.org/b/id/1-M.jpg":   true,
		"https://ia800100.us.archive.org/view/x.jpg":    true,
		"http://image.tmdb.org/t/p/w300/x.jpg":          false,
		"https://169.254.169.254/latest/meta-data":      false,
		"https://evil.example/image.tmdb.org.jpg":       false,
		"https://image.tmdb.org.evil.example/t/p/x.jpg": false,
	} {
		u, _ := url.Parse(raw)
		if got := allowed(u); got != want {
			t.Errorf("allowed(%s) = %v, want %v", raw, got, want)
		}
	}
}
