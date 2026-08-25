package gateway

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"strings"
)

// 参考图归一化：相机直出 JPEG 携带的 EXIF 色彩元数据（如 bt470bg 传输/原色字段
// 不完整）会让部分上游解码器概率性失败（快速 400 / 长时间挂起，2026-08-25 与
// MiniMax canvas-20 实测确认）。容器级净化（image_container.go）只能去掉 MPF
// 多图拼接，治不了色彩元数据兼容问题；上游建议的修法是解码后转 RGB 重新编码。
//
// 这里对命中特征的 JPEG 做完整归一化：解码 → 按 EXIF Orientation 旋转像素 →
// 超过 normalizeMaxLongEdge 时降采样 → 重编码为不带任何元数据段的标准 JFIF
// sRGB JPEG。副作用是同内容体积从 3-4MB 降到几百 KB，实测把上游响应从
// 100s+/挂死拉回 24-41s 稳定返回。
//
// 与既有管线的关系：normalizeReferenceImage 在 readImageRefBytes（JSON 引用图）
// 与 normalizeImagesEditMultipartBody（multipart 直传）两条路共用；解码失败时
// 降级为容器净化后原样透传（宁可让上游判，不交出改坏的文件）。

const (
	// normalizeMaxLongEdge 归一化后的长边像素上限。gpt-image-2 参考图在上游会被
	// 压成 latent，2048 足够；同时把上传体积压一个数量级。
	normalizeMaxLongEdge = 2048
	// normalizeJPEGQuality 重编码质量。参考图场景 90 与原图肉眼不可分。
	normalizeJPEGQuality = 90
)

// needsImageNormalization 判断 JPEG 是否需要归一化：携带 EXIF/ICC/MPF 等
// APP1/APP2 元数据段（相机/手机直出图必带），或像素长边超过
// normalizeMaxLongEdge。非 JPEG 一律不处理。
// 注意不能把「缺 JFIF APP0」当触发条件：Go 自己的 jpeg.Encode 输出就不带
// APP0（上游实测接受），那样会把归一化输出再次判为待处理。
func needsImageNormalization(data []byte) bool {
	if !isJPEGBytes(data) {
		return false
	}
	hasMetadata := false
	i := 2
	for i+3 < len(data) {
		if data[i] != jpegMarkerPrefix {
			break
		}
		marker := data[i+1]
		if marker == jpegSOI || marker == jpegEOI || (marker >= 0xD0 && marker <= 0xD7) || marker == 0x01 {
			i += 2
			continue
		}
		if marker == jpegSOS {
			break
		}
		segLen := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if segLen < 2 || i+2+segLen > len(data) {
			break
		}
		if marker == 0xE1 || marker == 0xE2 { // APP1(EXIF/XMP) / APP2(ICC/MPF)
			hasMetadata = true
		}
		i += 2 + segLen
	}
	if hasMetadata {
		return true
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return true
	}
	return cfg.Width > normalizeMaxLongEdge || cfg.Height > normalizeMaxLongEdge
}

// jpegExifOrientation 从 APP1 Exif 段解析 Orientation（1-8）；解析不到返回 0。
func jpegExifOrientation(data []byte) int {
	i := 2
	for i+3 < len(data) {
		if data[i] != jpegMarkerPrefix {
			return 0
		}
		marker := data[i+1]
		if marker == jpegSOS || marker == jpegEOI {
			return 0
		}
		if marker == jpegSOI || (marker >= 0xD0 && marker <= 0xD7) || marker == 0x01 {
			i += 2
			continue
		}
		segLen := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if segLen < 2 || i+2+segLen > len(data) {
			return 0
		}
		if marker == 0xE1 {
			seg := data[i+4 : i+2+segLen]
			if o := parseExifOrientation(seg); o != 0 {
				return o
			}
		}
		i += 2 + segLen
	}
	return 0
}

// parseExifOrientation 在 APP1 段体（含 "Exif\0\0" 前缀）里找 IFD0 的
// Orientation(0x0112) 标签。
func parseExifOrientation(seg []byte) int {
	const exifHeader = "Exif\x00\x00"
	if len(seg) < len(exifHeader)+8 || string(seg[:len(exifHeader)]) != exifHeader {
		return 0
	}
	tiff := seg[len(exifHeader):]
	var order binary.ByteOrder
	switch {
	case bytes.HasPrefix(tiff, []byte("II")):
		order = binary.LittleEndian
	case bytes.HasPrefix(tiff, []byte("MM")):
		order = binary.BigEndian
	default:
		return 0
	}
	if len(tiff) < 8 || order.Uint16(tiff[2:4]) != 42 {
		return 0
	}
	ifdOffset := int(order.Uint32(tiff[4:8]))
	if ifdOffset < 0 || ifdOffset+2 > len(tiff) {
		return 0
	}
	count := int(order.Uint16(tiff[ifdOffset : ifdOffset+2]))
	entryBase := ifdOffset + 2
	for e := range count {
		off := entryBase + e*12
		if off+12 > len(tiff) {
			return 0
		}
		tag := order.Uint16(tiff[off : off+2])
		if tag != 0x0112 {
			continue
		}
		typ := order.Uint16(tiff[off+2 : off+4])
		if typ != 3 { // SHORT
			return 0
		}
		val := int(order.Uint16(tiff[off+8 : off+10]))
		if val >= 1 && val <= 8 {
			return val
		}
		return 0
	}
	return 0
}

// applyExifOrientation 把 EXIF Orientation 语义真实作用到像素上，返回视觉正立的图。
func applyExifOrientation(img image.Image, orientation int) image.Image {
	switch orientation {
	case 2:
		return transformImage(img, func(w, _ int, x, y int) (int, int) { return w - 1 - x, y })
	case 3:
		return transformImage(img, func(w, h int, x, y int) (int, int) { return w - 1 - x, h - 1 - y })
	case 4:
		return transformImage(img, func(_, h int, x, y int) (int, int) { return x, h - 1 - y })
	case 5:
		return transformImageSwap(img, func(_, _ int, x, y int) (int, int) { return y, x })
	case 6:
		// 旋转 90° CW 显示：dst(x,y) = src(y, srcH-1-x)
		return transformImageSwap(img, func(_, h int, x, y int) (int, int) { return y, h - 1 - x })
	case 7:
		return transformImageSwap(img, func(w, h int, x, y int) (int, int) { return w - 1 - y, h - 1 - x })
	case 8:
		// 旋转 270° CW 显示：dst(x,y) = src(srcW-1-y, x)
		return transformImageSwap(img, func(w, _ int, x, y int) (int, int) { return w - 1 - y, x })
	default:
		return img
	}
}

// transformImage 同尺寸像素重排；mapSrc 输入输出图坐标 (x,y)，返回源图坐标。
func transformImage(img image.Image, mapSrc func(w, h, x, y int) (int, int)) image.Image {
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			sx, sy := mapSrc(w, h, x, y)
			out.Set(x, y, img.At(bounds.Min.X+sx, bounds.Min.Y+sy))
		}
	}
	return out
}

// transformImageSwap 宽高互换的像素重排（旋转 90/270 及其翻转变体）。
// mapSrc 输入输出图坐标 (x,y)（输出图 w=源 h、h=源 w），返回源图坐标 (sx,sy)。
// 回调收到的 (w,h) 是源图宽高。
func transformImageSwap(img image.Image, mapSrc func(w, h, x, y int) (int, int)) image.Image {
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	out := image.NewRGBA(image.Rect(0, 0, h, w))
	for y := range w {
		for x := range h {
			sx, sy := mapSrc(w, h, x, y)
			out.Set(x, y, img.At(bounds.Min.X+sx, bounds.Min.Y+sy))
		}
	}
	return out
}

// resizeImageArea 面积平均降采样（仅用于缩小）。参考图是照片，
// 邻近采样会引入锯齿，这里按源区域取均值。
func resizeImageArea(img image.Image, width, height int) *image.RGBA {
	bounds := img.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	out := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		sy0 := y * srcH / height
		sy1 := (y + 1) * srcH / height
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for x := range width {
			sx0 := x * srcW / width
			sx1 := (x + 1) * srcW / width
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var r, g, b, a, n uint64
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					pr, pg, pb, pa := img.At(bounds.Min.X+sx, bounds.Min.Y+sy).RGBA()
					r += uint64(pr)
					g += uint64(pg)
					b += uint64(pb)
					a += uint64(pa)
					n++
				}
			}
			out.SetRGBA(x, y, color.RGBA{
				R: uint8((r / n) >> 8),
				G: uint8((g / n) >> 8),
				B: uint8((b / n) >> 8),
				A: uint8((a / n) >> 8),
			})
		}
	}
	return out
}

// normalizeReferenceImage 对参考图做完整归一化（见文件头注释）。
// 返回处理后的字节、MIME 与是否发生改写；无需处理或无法处理时原样返回
// （无法解码时退回容器净化）。
func normalizeReferenceImage(data []byte, mimeType string) ([]byte, string, bool) {
	if !needsImageNormalization(data) {
		return data, mimeType, false
	}
	orientation := jpegExifOrientation(data)
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		// 解码不了（伪装扩展名/损坏文件）：退回容器净化，剩下交上游判。
		sanitized, changed := sanitizeReferenceImage(data, mimeType)
		return sanitized, mimeType, changed
	}
	img = applyExifOrientation(img, orientation)

	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if long := max(w, h); long > normalizeMaxLongEdge {
		scaledW := w * normalizeMaxLongEdge / long
		scaledH := h * normalizeMaxLongEdge / long
		img = resizeImageArea(img, max(scaledW, 1), max(scaledH, 1))
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, flattenForJPEG(img), &jpeg.Options{Quality: normalizeJPEGQuality}); err != nil {
		sanitized, changed := sanitizeReferenceImage(data, mimeType)
		return sanitized, mimeType, changed
	}
	return buf.Bytes(), "image/jpeg", true
}

// normalizeImagesEditMultipartBody 对客户端直传的 multipart /images/edits 请求体
// 中的参考图字段（image / image[]）做归一化，mask 与其余字段原样保留。
// 未发生任何改写时返回原始 body。
func normalizeImagesEditMultipartBody(body []byte, contentType string) ([]byte, bool, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.EqualFold(mediaType, "multipart/form-data") {
		return body, false, nil
	}
	boundary := params["boundary"]
	if boundary == "" {
		return body, false, nil
	}

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	if err := writer.SetBoundary(boundary); err != nil {
		return body, false, nil
	}

	changedAny := false
	for {
		part, nextErr := reader.NextPart()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return nil, false, fmt.Errorf("multipart 读取失败: %w", nextErr)
		}
		header := make(textproto.MIMEHeader, len(part.Header))
		for key, values := range part.Header {
			header[key] = append([]string(nil), values...)
		}
		data, readErr := io.ReadAll(part)
		_ = part.Close()
		if readErr != nil {
			return nil, false, fmt.Errorf("multipart part %q 读取失败: %w", part.FormName(), readErr)
		}
		if name := part.FormName(); name == "image" || name == "image[]" {
			if normalized, mimeOut, changed := normalizeReferenceImage(data, header.Get("Content-Type")); changed {
				data = normalized
				changedAny = true
				if header.Get("Content-Type") != "" {
					header.Set("Content-Type", mimeOut)
				}
			}
		}
		dst, createErr := writer.CreatePart(header)
		if createErr != nil {
			return nil, false, fmt.Errorf("multipart part %q 重建失败: %w", part.FormName(), createErr)
		}
		if _, writeErr := dst.Write(data); writeErr != nil {
			return nil, false, fmt.Errorf("multipart part %q 写入失败: %w", part.FormName(), writeErr)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, false, fmt.Errorf("multipart 请求结束失败: %w", err)
	}
	if !changedAny {
		return body, false, nil
	}
	return buf.Bytes(), true, nil
}
