package e2e

import (
	"fmt"
	"image"
	"image/color"
	"testing"
)

type imageSemanticStats struct {
	pixels          int
	red             int
	white           int
	blueBottomRight int
	redBounds       image.Rectangle
	blueBRBounds    image.Rectangle
	hasRedBounds    bool
	hasBlueBRBounds bool
}

func measureImageSemantics(img image.Image) imageSemanticStats {
	bounds := img.Bounds()
	stats := imageSemanticStats{pixels: bounds.Dx() * bounds.Dy()}
	bottomRightX := bounds.Min.X + bounds.Dx()/2
	bottomRightY := bounds.Min.Y + bounds.Dy()/2
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r16, g16, b16, _ := img.At(x, y).RGBA()
			r, g, b := int(r16>>8), int(g16>>8), int(b16>>8)
			if r >= 160 && r >= g*3/2 && r >= b*3/2 {
				stats.red++
				stats.redBounds, stats.hasRedBounds = extendPixelBounds(stats.redBounds, stats.hasRedBounds, x, y)
			}
			if r >= 220 && g >= 220 && b >= 220 {
				stats.white++
			}
			if b >= 140 && b >= r*3/2 && b >= g*3/2 {
				if x >= bottomRightX && y >= bottomRightY {
					stats.blueBottomRight++
					stats.blueBRBounds, stats.hasBlueBRBounds = extendPixelBounds(stats.blueBRBounds, stats.hasBlueBRBounds, x, y)
				}
			}
		}
	}
	return stats
}

func validateGeneratedImageSemantics(img image.Image) error {
	stats := measureImageSemantics(img)
	if stats.pixels == 0 {
		return fmt.Errorf("generated image has no pixels")
	}
	if stats.red*100 < stats.pixels*2 {
		return fmt.Errorf("generated image red pixels = %.2f%%, want at least 2%%", percentage(stats.red, stats.pixels))
	}
	if stats.white*100 < stats.pixels*30 {
		return fmt.Errorf("generated image white pixels = %.2f%%, want at least 30%%", percentage(stats.white, stats.pixels))
	}
	redWidth, redHeight := stats.redBounds.Dx(), stats.redBounds.Dy()
	if !stats.hasRedBounds || redWidth*4 < redHeight*3 || redHeight*4 < redWidth*3 {
		return fmt.Errorf("generated red shape bounds = %v, want roughly equal width and height", stats.redBounds)
	}
	redBoundsPixels := redWidth * redHeight
	if stats.red*100 < redBoundsPixels*45 || stats.red*100 > redBoundsPixels*90 {
		return fmt.Errorf("generated red-shape fill = %.2f%%, want a compact circular silhouette", percentage(stats.red, redBoundsPixels))
	}
	imageBounds := img.Bounds()
	if absInt(stats.redBounds.Min.X+stats.redBounds.Max.X-imageBounds.Min.X-imageBounds.Max.X) > imageBounds.Dx()/5 ||
		absInt(stats.redBounds.Min.Y+stats.redBounds.Max.Y-imageBounds.Min.Y-imageBounds.Max.Y) > imageBounds.Dy()/5 {
		return fmt.Errorf("generated red shape bounds = %v, want a centered shape", stats.redBounds)
	}
	return nil
}

func validateEditedImageSemantics(source, edited image.Image) error {
	sourceStats := measureImageSemantics(source)
	stats := measureImageSemantics(edited)
	if stats.pixels == 0 || stats.pixels != sourceStats.pixels {
		return fmt.Errorf("edited image pixels = %d, want source size %d", stats.pixels, sourceStats.pixels)
	}
	if stats.red*200 < stats.pixels {
		return fmt.Errorf("edited image red pixels = %.2f%%, want at least 0.5%%", percentage(stats.red, stats.pixels))
	}
	minimumBlueIncrease := stats.pixels / 2000
	if stats.blueBottomRight < sourceStats.blueBottomRight+minimumBlueIncrease {
		return fmt.Errorf(
			"edited image bottom-right blue pixels = %d, source=%d, want increase of at least %d",
			stats.blueBottomRight,
			sourceStats.blueBottomRight,
			minimumBlueIncrease,
		)
	}
	blueWidth, blueHeight := stats.blueBRBounds.Dx(), stats.blueBRBounds.Dy()
	if !stats.hasBlueBRBounds || blueWidth*2 < blueHeight || blueHeight*2 < blueWidth {
		return fmt.Errorf("edited bottom-right blue bounds = %v, want a compact square-like shape", stats.blueBRBounds)
	}
	if stats.blueBottomRight*100 < blueWidth*blueHeight*25 {
		return fmt.Errorf("edited bottom-right blue fill = %.2f%%, want a solid square-like shape", percentage(stats.blueBottomRight, blueWidth*blueHeight))
	}
	return nil
}

func extendPixelBounds(bounds image.Rectangle, initialized bool, x, y int) (image.Rectangle, bool) {
	pixel := image.Rect(x, y, x+1, y+1)
	if !initialized {
		return pixel, true
	}
	return bounds.Union(pixel), true
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func percentage(value, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(value) * 100 / float64(total)
}

func TestImageSemanticOracle(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 100, 100))
	fillImage(source, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
	fillCircle(source, 50, 50, 30, color.NRGBA{R: 230, A: 255})
	if err := validateGeneratedImageSemantics(source); err != nil {
		t.Fatalf("valid generated fixture: %v", err)
	}

	edited := cloneNRGBA(source)
	fillRect(edited, image.Rect(70, 70, 90, 90), color.NRGBA{B: 230, A: 255})
	if err := validateEditedImageSemantics(source, edited); err != nil {
		t.Fatalf("valid edited fixture: %v", err)
	}

	plain := image.NewNRGBA(image.Rect(0, 0, 100, 100))
	fillImage(plain, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
	if err := validateGeneratedImageSemantics(plain); err == nil {
		t.Fatal("plain generated fixture unexpectedly passed")
	}
	redSquare := cloneNRGBA(plain)
	fillRect(redSquare, image.Rect(20, 20, 80, 80), color.NRGBA{R: 230, A: 255})
	if err := validateGeneratedImageSemantics(redSquare); err == nil {
		t.Fatal("red-square generated fixture unexpectedly passed the circle oracle")
	}
	if err := validateEditedImageSemantics(source, cloneNRGBA(source)); err == nil {
		t.Fatal("unchanged edit fixture unexpectedly passed")
	}
}

func fillImage(img *image.NRGBA, value color.NRGBA) {
	fillRect(img, img.Bounds(), value)
}

func fillRect(img *image.NRGBA, bounds image.Rectangle, value color.NRGBA) {
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			img.SetNRGBA(x, y, value)
		}
	}
}

func fillCircle(img *image.NRGBA, centerX, centerY, radius int, value color.NRGBA) {
	for y := centerY - radius; y <= centerY+radius; y++ {
		for x := centerX - radius; x <= centerX+radius; x++ {
			if deltaX, deltaY := x-centerX, y-centerY; deltaX*deltaX+deltaY*deltaY <= radius*radius {
				img.SetNRGBA(x, y, value)
			}
		}
	}
}

func cloneNRGBA(source *image.NRGBA) *image.NRGBA {
	clone := image.NewNRGBA(source.Bounds())
	copy(clone.Pix, source.Pix)
	return clone
}
