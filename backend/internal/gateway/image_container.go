package gateway

import (
	"bytes"
	"encoding/binary"
	"strings"
)

// 参考图容器净化：把相机直出的「多图 JPEG」还原成单图 JPEG。
//
// ── 为什么需要 ──────────────────────────────────────────────────────
// 2026-08-24 客户 oumomo 的生图任务连续失败,上游只回一句
// `Invalid image file or mode for image`,并且因为我们对这类错误会重试,
// 最终以 `context deadline exceeded`(300s 生图转发硬超时)的形式暴露,
// 表象与根因完全对不上。
//
// 逐张二分后定位到：失败的是索尼 ZV-E1 直出的 JPEG,文件里带 APP2[MPF]
// （Multi-Picture Format）段并在第一个 EOI 之后拼接了多张子图(整份文件里
// 出现 20+ 个 SOI)。上游解码器吃不下这种容器。排查中依次证伪过
// 「像素太大」「文件太大」「EXIF」「alpha/CMYK」——判别实验是:
// **同样的像素、同样的 EXIF,只要重新编码一次就成功**,因为重编码把 MPF
// 和多余子图洗掉了。
//
// ── 为什么是无损重组而不是解码重编码 ──────────────────────────────
// 解码再编码能修好,但会二次压缩损画质、吃 CPU,而且会丢掉 EXIF 里的
// Orientation(那张图是 Orientation=6,丢了方向就变了)。这里改为只重写
// **容器**：丢掉 APP2[MPF] 段、截断到第一个 EOI,原始熵编码扫描数据与
// EXIF 原样保留——零画质损失,方向不变。
//
// 2026-08-24 用真实失败样本对生产上游验证：净化前 400,净化后正常出图。
const (
	jpegMarkerPrefix = 0xFF
	jpegSOI          = 0xD8
	jpegEOI          = 0xD9
	jpegSOS          = 0xDA
	jpegAPP2         = 0xE2
	jpegTEM          = 0x01
	jpegRST0         = 0xD0
	jpegRST7         = 0xD7
)

// mpfIdentifier APP2 段里标识 Multi-Picture Format 的固定前缀。
var mpfIdentifier = []byte("MPF\x00")

// isJPEGBytes 只认真正的 JPEG 起始标记,不依赖 Content-Type
// （相机原图经 CDN 转发时 Content-Type 常常不可靠）。
func isJPEGBytes(data []byte) bool {
	return len(data) >= 2 && data[0] == jpegMarkerPrefix && data[1] == jpegSOI
}

// sanitizeReferenceImage 对参考图做容器净化。
// 返回净化后的字节与是否发生了改动；无法解析或无需处理时原样返回。
//
// 只处理 JPEG：PNG/WEBP 没有这种多图容器形态。
func sanitizeReferenceImage(data []byte, mimeType string) ([]byte, bool) {
	if !isJPEGBytes(data) {
		return data, false
	}
	// mimeType 仅作旁证：真正的判断依据是字节头，因为 CDN 常把相机原图
	// 标成 application/octet-stream 之类。
	_ = strings.ToLower(mimeType)
	return stripJPEGMultiPicture(data)
}

// stripJPEGMultiPicture 重写 JPEG 容器：丢弃 APP2[MPF]，并在第一个 EOI 处截断。
//
// 安全性：熵编码数据里的 0xFF 一律被转义成 0xFF00，重启标记只用 0xFFD0~0xFFD7，
// 因此扫描段内出现的 0xFFD9 只可能是真正的 EOI，不会误截。
func stripJPEGMultiPicture(data []byte) ([]byte, bool) {
	out := make([]byte, 0, len(data))
	out = append(out, data[0], data[1]) // SOI
	i := 2
	changed := false

	for i < len(data)-1 {
		if data[i] != jpegMarkerPrefix {
			// 标记之间不应出现裸字节；结构异常时放弃净化，原样透传，
			// 宁可让上游去判，也不要交出一个被我们改坏的文件。
			return data, false
		}
		marker := data[i+1]

		switch {
		case marker == jpegEOI:
			out = append(out, jpegMarkerPrefix, jpegEOI)
			// 第一个 EOI 之后若还有内容，说明是多图容器，截断即为净化。
			if i+2 < len(data) {
				changed = true
			}
			return out, changed

		case marker == jpegSOS:
			// 扫描数据一直到第一个 EOI。
			end := indexJPEGEOI(data, i+2)
			if end < 0 {
				return data, false // 没有 EOI，文件本身就是坏的，别动
			}
			out = append(out, data[i:end+2]...)
			if end+2 < len(data) {
				changed = true
			}
			return out, changed

		case marker == jpegTEM || (marker >= jpegRST0 && marker <= jpegRST7):
			out = append(out, data[i], data[i+1])
			i += 2

		default:
			if i+4 > len(data) {
				return data, false
			}
			length := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
			if length < 2 || i+2+length > len(data) {
				return data, false
			}
			segment := data[i : i+2+length]
			if marker == jpegAPP2 && len(segment) >= 8 && bytes.Equal(segment[4:8], mpfIdentifier) {
				// APP2[MPF]：多图索引表，正是上游拒收的元凶，丢掉。
				changed = true
			} else {
				out = append(out, segment...)
			}
			i += 2 + length
		}
	}
	return data, false
}

// indexJPEGEOI 从 from 开始找第一个 0xFFD9 的位置；找不到返回 -1。
func indexJPEGEOI(data []byte, from int) int {
	for j := from; j < len(data)-1; j++ {
		if data[j] == jpegMarkerPrefix && data[j+1] == jpegEOI {
			return j
		}
	}
	return -1
}
