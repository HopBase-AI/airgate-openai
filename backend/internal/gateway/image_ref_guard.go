package gateway

// 参考图服务端代取的 SSRF 防护:客户端可传任意 http(s) URL 让网关下载,
// 不设防等于把内网探测开放给任意持 key 客户。守卫装在 dial 层(Control 钩子
// 在 DNS 解析后、连接前对实际地址校验),DNS rebinding 也拦得住。

import (
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

var errInternalImageRefAddr = fmt.Errorf("参考图 URL 不允许指向内部地址")

// allowInternalImageRefAddrs 仅测试用:httptest 上游全在环回地址上。
var allowInternalImageRefAddrs = false

func guardImageRefAddr(address string) error {
	if allowInternalImageRefAddrs {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errInternalImageRefAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return errInternalImageRefAddr
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return errInternalImageRefAddr
	}
	return nil
}

// newImageRefHTTPClient 构造参考图下载专用 client:dial 层拒内部地址。
func newImageRefHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			return guardImageRefAddr(address)
		},
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = dialer.DialContext
	return &http.Client{Timeout: 30 * time.Second, Transport: transport}
}
