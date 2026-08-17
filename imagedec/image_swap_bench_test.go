package imagedec

import (
	"testing"

	"github.com/unxed/vtui"
)

// Benchmarks for the WIC channel pass: the combined swap+straighten sweep
// a 32bppPBGRA copy runs, against swapRB then unpremultiplyRGBA separately.

func BenchmarkSwapUnpremultiply(b *testing.B) {
	surf := vtui.NewImageSurface(1920, 1080)
	for i := range surf.Pix {
		surf.Pix[i] = byte(i)
	}
	b.SetBytes(int64(len(surf.Pix)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		swapUnpremultiplyRGBA(surf.Pix)
	}
}

func BenchmarkSwapThenUnpremultiply(b *testing.B) {
	surf := vtui.NewImageSurface(1920, 1080)
	for i := range surf.Pix {
		surf.Pix[i] = byte(i)
	}
	b.SetBytes(int64(len(surf.Pix)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		swapRB(surf.Pix)
		unpremultiplyRGBA(surf.Pix)
	}
}
