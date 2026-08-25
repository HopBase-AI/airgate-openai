package gateway

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"mime/multipart"
	"net/textproto"
	"testing"
)

// exifSegmentWithOrientation 构造带 Orientation 标签的合法 APP1[Exif] 段。
func exifSegmentWithOrientation(orientation uint16) []byte {
	tiff := make([]byte, 0, 26)
	tiff = append(tiff, 'I', 'I') // little-endian
	tiff = binary.LittleEndian.AppendUint16(tiff, 42)
	tiff = binary.LittleEndian.AppendUint32(tiff, 8) // IFD0 offset
	tiff = binary.LittleEndian.AppendUint16(tiff, 1) // entry count
	tiff = binary.LittleEndian.AppendUint16(tiff, 0x0112)
	tiff = binary.LittleEndian.AppendUint16(tiff, 3) // SHORT
	tiff = binary.LittleEndian.AppendUint32(tiff, 1) // count
	tiff = binary.LittleEndian.AppendUint16(tiff, orientation)
	tiff = binary.LittleEndian.AppendUint16(tiff, 0) // value padding
	tiff = binary.LittleEndian.AppendUint32(tiff, 0) // next IFD
	return appSegment(0xE1, "Exif\x00\x00", tiff)
}

// buildCameraJPEG 构造相机直出同构样本：EXIF(Orientation) + MPF + 尾部子图。
func buildCameraJPEG(t *testing.T, w, h int, orientation uint16, fill func(x, y int) color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if fill != nil {
				img.Set(x, y, fill(x, y))
			} else {
				img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("编码测试 JPEG 失败: %v", err)
	}
	primary := buf.Bytes()
	second := buildTestJPEG(t, 16, 16)

	var out bytes.Buffer
	out.Write(primary[:2])
	out.Write(exifSegmentWithOrientation(orientation))
	out.Write(appSegment(0xE2, "MPF\x00", bytes.Repeat([]byte{0x22}, 48)))
	out.Write(primary[2:])
	out.Write(second)
	return out.Bytes()
}

func TestJPEGExifOrientationParse(t *testing.T) {
	data := buildCameraJPEG(t, 32, 24, 6, nil)
	if got := jpegExifOrientation(data); got != 6 {
		t.Fatalf("orientation = %d, want 6", got)
	}
	clean := buildTestJPEG(t, 32, 24)
	if got := jpegExifOrientation(clean); got != 0 {
		t.Fatalf("clean orientation = %d, want 0", got)
	}
}

func TestNormalizeReferenceImageCleansCameraJPEG(t *testing.T) {
	data := buildCameraJPEG(t, 32, 24, 6, nil)
	out, mimeOut, changed := normalizeReferenceImage(data, "image/jpeg")
	if !changed {
		t.Fatal("相机样本应被归一化")
	}
	if mimeOut != "image/jpeg" {
		t.Fatalf("mime = %s", mimeOut)
	}
	if needsImageNormalization(out) {
		t.Fatal("归一化输出仍命中归一化条件（应为无元数据的标准 JFIF）")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("输出不可解码: %v", err)
	}
	// Orientation=6 → 像素真旋转，宽高互换。
	if cfg.Width != 24 || cfg.Height != 32 {
		t.Fatalf("输出尺寸 %dx%d, want 24x32", cfg.Width, cfg.Height)
	}
	if n := bytes.Count(out, []byte{0xFF, 0xD8, 0xFF}); n != 1 {
		t.Fatalf("输出含 %d 个 SOI, want 1", n)
	}
}

func TestNormalizeRotationOrientation6Pixels(t *testing.T) {
	// 源图 32x16：左半红、右半蓝。旋转 90° CW 后 16x32：上半红、下半蓝。
	data := buildCameraJPEG(t, 32, 16, 6, func(x, _ int) color.RGBA {
		if x < 16 {
			return color.RGBA{R: 220, G: 20, B: 20, A: 255}
		}
		return color.RGBA{R: 20, G: 20, B: 220, A: 255}
	})
	out, _, changed := normalizeReferenceImage(data, "image/jpeg")
	if !changed {
		t.Fatal("应被归一化")
	}
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if img.Bounds().Dx() != 16 || img.Bounds().Dy() != 32 {
		t.Fatalf("尺寸 %v, want 16x32", img.Bounds())
	}
	r, _, b, _ := img.At(8, 4).RGBA() // 上半
	if r <= b {
		t.Fatalf("上半应为红色, got r=%d b=%d", r>>8, b>>8)
	}
	r, _, b, _ = img.At(8, 28).RGBA() // 下半
	if b <= r {
		t.Fatalf("下半应为蓝色, got r=%d b=%d", r>>8, b>>8)
	}
}

func TestNormalizeDownscalesOversized(t *testing.T) {
	data := buildTestJPEG(t, 2600, 1000)
	out, _, changed := normalizeReferenceImage(data, "image/jpeg")
	if !changed {
		t.Fatal("超长边应被降采样")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if cfg.Width != normalizeMaxLongEdge {
		t.Fatalf("长边 = %d, want %d", cfg.Width, normalizeMaxLongEdge)
	}
	wantH := 1000 * normalizeMaxLongEdge / 2600
	if cfg.Height != wantH {
		t.Fatalf("短边 = %d, want %d", cfg.Height, wantH)
	}
}

func TestNormalizeLeavesCleanSmallJPEGAlone(t *testing.T) {
	data := buildTestJPEG(t, 100, 80)
	out, mimeOut, changed := normalizeReferenceImage(data, "image/jpeg")
	if changed {
		t.Fatal("干净小图不应被改写")
	}
	if !bytes.Equal(out, data) || mimeOut != "image/jpeg" {
		t.Fatal("干净小图应原样返回")
	}
}

func TestNormalizeLeavesPNGAlone(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 3000, 100))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	out, _, changed := normalizeReferenceImage(buf.Bytes(), "image/png")
	if changed || !bytes.Equal(out, buf.Bytes()) {
		t.Fatal("PNG 不应被归一化")
	}
}

func TestNormalizeFallsBackOnUndecodableJPEG(t *testing.T) {
	garbage := append([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x04, 0xAA, 0xBB}, bytes.Repeat([]byte{0xCC}, 64)...)
	out, _, _ := normalizeReferenceImage(garbage, "image/jpeg")
	if out == nil {
		t.Fatal("不可解码文件应透传而非报错")
	}
}

func TestNormalizeImagesEditMultipartBody(t *testing.T) {
	camera := buildCameraJPEG(t, 32, 24, 6, nil)
	var maskBuf bytes.Buffer
	if err := png.Encode(&maskBuf, image.NewNRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "gpt-image-2")
	_ = mw.WriteField("prompt", "combine")
	imgHeader := make(textproto.MIMEHeader)
	imgHeader.Set("Content-Disposition", `form-data; name="image[]"; filename="ref.jpg"`)
	imgHeader.Set("Content-Type", "image/jpeg")
	part, _ := mw.CreatePart(imgHeader)
	_, _ = part.Write(camera)
	maskHeader := make(textproto.MIMEHeader)
	maskHeader.Set("Content-Disposition", `form-data; name="mask"; filename="mask.png"`)
	maskHeader.Set("Content-Type", "image/png")
	part, _ = mw.CreatePart(maskHeader)
	_, _ = part.Write(maskBuf.Bytes())
	_ = mw.Close()
	contentType := mw.FormDataContentType()

	out, changed, err := normalizeImagesEditMultipartBody(buf.Bytes(), contentType)
	if err != nil {
		t.Fatalf("normalize multipart: %v", err)
	}
	if !changed {
		t.Fatal("含相机图的 multipart 应被改写")
	}

	reader := multipart.NewReader(bytes.NewReader(out), mw.Boundary())
	form, err := reader.ReadForm(32 << 20)
	if err != nil {
		t.Fatalf("重读 multipart: %v", err)
	}
	defer func() { _ = form.RemoveAll() }()
	if got := form.Value["model"]; len(got) != 1 || got[0] != "gpt-image-2" {
		t.Fatalf("model 字段丢失: %v", got)
	}
	imgFile, err := form.File["image[]"][0].Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = imgFile.Close() }()
	imgBytes := make([]byte, form.File["image[]"][0].Size)
	if _, err := imgFile.Read(imgBytes); err != nil && err.Error() != "EOF" {
		t.Fatal(err)
	}
	if bytes.Equal(imgBytes, camera) {
		t.Fatal("参考图应被归一化改写")
	}
	if needsImageNormalization(imgBytes) {
		t.Fatal("改写后的参考图不应再命中归一化条件")
	}
	maskFile, err := form.File["mask"][0].Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = maskFile.Close() }()
	maskBytes := make([]byte, form.File["mask"][0].Size)
	if _, err := maskFile.Read(maskBytes); err != nil && err.Error() != "EOF" {
		t.Fatal(err)
	}
	if !bytes.Equal(maskBytes, maskBuf.Bytes()) {
		t.Fatal("mask 必须原样保留")
	}
}

func TestNormalizeMultipartNoChangePassthrough(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "gpt-image-2")
	imgHeader := make(textproto.MIMEHeader)
	imgHeader.Set("Content-Disposition", `form-data; name="image"; filename="ref.jpg"`)
	imgHeader.Set("Content-Type", "image/jpeg")
	part, _ := mw.CreatePart(imgHeader)
	_, _ = part.Write(buildTestJPEG(t, 64, 48))
	_ = mw.Close()

	out, changed, err := normalizeImagesEditMultipartBody(buf.Bytes(), mw.FormDataContentType())
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("干净图不应触发改写")
	}
	if !bytes.Equal(out, buf.Bytes()) {
		t.Fatal("未改写时应返回原始 body")
	}
}
