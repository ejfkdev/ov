package main

// -x 代理支持(参考 curl):
//  - http/https 代理: 自实现 HTTP CONNECT 隧道(uTLS 路径不走标准库 Transport, 需手动拨号);
//  - socks5/socks5h: 复用 golang.org/x/net/proxy。
// 标准库 Transport 路径则在 main 中直接配置 http.ProxyURL, 不经此处。

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// tunnelConn 在底层连接之上包一层 bufio.Reader, 把 ReadResponse
// 预读进缓冲的隧道起始字节一并还给上层。
type tunnelConn struct {
	net.Conn
	br *bufio.Reader
}

func (c *tunnelConn) Read(p []byte) (int, error) { return c.br.Read(p) }

// dialerFunc 将普通拨号函数包装成 proxy.Dialer。
type dialerFunc func(network, addr string) (net.Conn, error)

func (f dialerFunc) Dial(network, addr string) (net.Conn, error) { return f(network, addr) }

// newProxyDialer 根据 -x 的代理地址构建拨号器; 无代理时返回 nil。
func newProxyDialer(raw string) (proxy.Dialer, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("format")
	}
	switch u.Scheme {
	case "http", "https":
		return httpConnectDialer(u), nil
	case "socks5", "socks5h":
		return proxy.FromURL(u, proxy.Direct)
	default:
		return nil, fmt.Errorf("scheme: %s", u.Scheme)
	}
}

// httpConnectDialer 通过 HTTP CONNECT 代理建立到目标地址的 TCP 隧道。
// https 代理先对其自身做 TLS, socks 不在此处理。
func httpConnectDialer(pu *url.URL) proxy.Dialer {
	proxyAddr := pu.Host
	if !strings.Contains(proxyAddr, ":") {
		proxyAddr += ":80"
	}
	return dialerFunc(func(network, addr string) (net.Conn, error) {
		var conn net.Conn
		var err error
		d := &net.Dialer{Timeout: 10 * time.Second}
		if pu.Scheme == "https" {
			conn, err = tls.DialWithDialer(d, "tcp", proxyAddr,
				&tls.Config{ServerName: pu.Hostname()})
		} else {
			conn, err = d.Dial("tcp", proxyAddr)
		}
		if err != nil {
			return nil, err
		}
		if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
			conn.Close()
			return nil, err
		}
		if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Connection: Keep-Alive\r\n\r\n", addr, addr); err != nil {
			conn.Close()
			return nil, err
		}
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
		if err != nil {
			conn.Close()
			return nil, err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			conn.Close()
			return nil, fmt.Errorf(t("proxyConnectFailed"), resp.Status)
		}
		conn.SetDeadline(time.Time{})
		return &tunnelConn{Conn: conn, br: br}, nil
	})
}
