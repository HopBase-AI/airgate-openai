package gateway

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// buildTestJPEG 生成一张真实可解码的 JPEG。
func buildTestJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 8), G: uint8(y * 8), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("生成测试 JPEG 失败: %v", err)
	}
	return buf.Bytes()
}

// appSegment 拼一个 APPn 段。
func appSegment(marker byte, identifier string, payload []byte) []byte {
	body := append([]byte(identifier), payload...)
	seg := []byte{0xFF, marker, 0, 0}
	binary.BigEndian.PutUint16(seg[2:4], uint16(len(body)+2))
	return append(seg, body...)
}

// buildMultiPictureJPEG 构造与客户失败样本同构的文件：
// SOI + APP1[Exif] + APP2[MPF] + 主图 ... EOI + 尾部拼接的第二张图。
func buildMultiPictureJPEG(t *testing.T) (multi []byte, primary []byte) {
	t.Helper()
	primary = buildTestJPEG(t, 32, 24)
	second := buildTestJPEG(t, 16, 16)

	exif := appSegment(0xE1, "Exif\x00\x00", bytes.Repeat([]byte{0x11}, 64))
	mpf := appSegment(0xE2, "MPF\x00", bytes.Repeat([]byte{0x22}, 48))

	var out bytes.Buffer
	out.Write(primary[:2]) // SOI
	out.Write(exif)
	out.Write(mpf)
	out.Write(primary[2:]) // 主图其余部分（含 EOI）
	out.Write(second)      // 尾部多余子图
	return out.Bytes(), primary
}

// TestSanitizeReferenceImageStripsMultiPicture 相机直出的多图 JPEG 必须被还原成单图。
//
// 回归背景（2026-08-24）：客户 oumomo 的参考图是索尼 ZV-E1 直出 JPEG，带
// APP2[MPF] 且在第一个 EOI 后拼了多张子图，上游解码器直接拒收
// （`Invalid image file or mode`），又因为我们对该错误会重试，最终以
// `context deadline exceeded` 的形式暴露，表象与根因完全对不上。
func TestSanitizeReferenceImageStripsMultiPicture(t *testing.T) {
	multi, _ := buildMultiPictureJPEG(t)

	if !bytes.Contains(multi, []byte("MPF\x00")) {
		t.Fatal("测试样本构造失败：应含 APP2[MPF]")
	}

	clean, changed := sanitizeReferenceImage(multi, "image/jpeg")
	if !changed {
		t.Fatal("多图 JPEG 应被判定为需要净化")
	}
	if bytes.Contains(clean, []byte("MPF\x00")) {
		t.Error("APP2[MPF] 未被去除——这正是上游拒收的元凶")
	}
	if len(clean) >= len(multi) {
		t.Errorf("净化后应更小：%d → %d", len(multi), len(clean))
	}
	// EXIF 必须保留：那张失败样本是 Orientation=6，丢了方向就变了。
	if !bytes.Contains(clean, []byte("Exif\x00\x00")) {
		t.Error("EXIF 被误删——会丢失 Orientation")
	}
	// 结构完整：以 SOI 开头、EOI 结尾，且尾部子图已截断。
	if !bytes.HasPrefix(clean, []byte{0xFF, 0xD8}) {
		t.Error("净化结果不是以 SOI 开头")
	}
	if !bytes.HasSuffix(clean, []byte{0xFF, 0xD9}) {
		t.Error("净化结果不是以 EOI 结尾")
	}
	// 仍然可解码，且尺寸与主图一致（无损重组，不是重新编码）。
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(clean))
	if err != nil {
		t.Fatalf("净化后无法解码: %v", err)
	}
	if cfg.Width != 32 || cfg.Height != 24 {
		t.Errorf("净化后尺寸 %dx%d，应与主图一致 32x24", cfg.Width, cfg.Height)
	}
}

// TestSanitizeReferenceImageIsIdempotent 已经干净的图不应被改动。
func TestSanitizeReferenceImageIsIdempotent(t *testing.T) {
	plain := buildTestJPEG(t, 32, 24)
	out, changed := sanitizeReferenceImage(plain, "image/jpeg")
	if changed {
		t.Error("普通单图 JPEG 不该被判定为需要净化")
	}
	if !bytes.Equal(out, plain) {
		t.Error("普通 JPEG 必须原字节返回——不做二次压缩")
	}

	multi, _ := buildMultiPictureJPEG(t)
	once, _ := sanitizeReferenceImage(multi, "image/jpeg")
	twice, changedAgain := sanitizeReferenceImage(once, "image/jpeg")
	if changedAgain {
		t.Error("净化应幂等：第二次不该再改动")
	}
	if !bytes.Equal(once, twice) {
		t.Error("净化不幂等")
	}
}

// TestSanitizeReferenceImageLeavesNonJPEGAlone PNG/WEBP 没有这种多图容器，
// 结构异常的文件也一律原样透传——宁可让上游去判，也不要交出一个被我们改坏的文件。
func TestSanitizeReferenceImageLeavesNonJPEGAlone(t *testing.T) {
	cases := map[string][]byte{
		"PNG":       {0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 1, 2, 3},
		"空":         {},
		"太短":        {0xFF},
		"仅SOI":      {0xFF, 0xD8},
		"SOI后是裸字节":  {0xFF, 0xD8, 0x00, 0x01, 0x02},
		"段长非法":      {0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x01},
		"有SOS但无EOI": {0xFF, 0xD8, 0xFF, 0xDA, 0x00, 0x02, 0x11, 0x22},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			out, changed := sanitizeReferenceImage(in, "image/png")
			if changed {
				t.Errorf("%s 不该被判定为需要净化", name)
			}
			if !bytes.Equal(out, in) {
				t.Errorf("%s 必须原样返回", name)
			}
		})
	}
}
