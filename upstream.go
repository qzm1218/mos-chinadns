//     Copyright (C) 2020, IrineSistiana
//
//     This file is part of mos-chinadns.
//
//     mos-chinadns is free software: you can redistribute it and/or modify
//     it under the terms of the GNU General Public License as published by
//     the Free Software Foundation, either version 3 of the License, or
//     (at your option) any later version.
//
//     mos-chinadns is distributed in the hope that it will be useful,
//     but WITHOUT ANY WARRANTY; without even the implied warranty of
//     MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
//     GNU General Public License for more details.
//
//     You should have received a copy of the GNU General Public License
//     along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/IrineSistiana/mos-chinadns/utils"
	"github.com/valyala/fasthttp"
	"golang.org/x/net/http2"

	"github.com/IrineSistiana/mos-chinadns/bufpool"
	"github.com/miekg/dns"
	"github.com/sirupsen/logrus"
)

const (
	tlsHandshakeTimeout = time.Second * 3
	dialTCPTimeout      = time.Second * 2
	dialUDPTimeout      = time.Second * 2
)

type upstream interface {
	Exchange(ctx context.Context, qRaw []byte, requestLogger *logrus.Entry) (rRaw *bufpool.MsgBuf, rtt time.Duration, err error)
}

// upstreamCommon represents a tcp/tls server
type upstreamCommon struct {
	addr        string
	dialNewConn func() (net.Conn, error)
	writeMsg    func(c io.Writer, msg []byte) (int, error)
	readMsg     func(c io.Reader) (msg *bufpool.MsgBuf, brokenDataLeft int, n int, err error)

	cp *connPool
}

func newUpstream(sc *BasicServerConfig, rootCAs *x509.CertPool) (upstream, error) {
	if sc == nil {
		panic("newUpstream: sc is nil")
	}

	var client upstream
	var err error
	switch sc.Protocol {
	case "udp", "":
		dialUDP := func() (net.Conn, error) {
			return net.DialTimeout("udp", sc.Addr, dialUDPTimeout)
		}
		client = &upstreamCommon{
			addr:        sc.Addr,
			dialNewConn: dialUDP,
			readMsg:     readMsgFromUDP,
			writeMsg:    writeMsgToUDP,
			cp:          newConnPool(0xffff, time.Second*10, time.Second*5),
		}
	case "tcp":
		idleTimeout := time.Duration(sc.TCP.IdleTimeout) * time.Second
		dialTCP := func() (net.Conn, error) {
			return net.DialTimeout("tcp", sc.Addr, dialTCPTimeout)
		}
		client = &upstreamCommon{
			addr:        sc.Addr,
			dialNewConn: dialTCP,
			readMsg:     readMsgFromTCP,
			writeMsg:    writeMsgToTCP,
			cp:          newConnPool(0xffff, idleTimeout, idleTimeout>>1),
		}
	case "dot":
		tlsConf := &tls.Config{
			ServerName:         sc.DoT.ServerName,
			RootCAs:            rootCAs,
			ClientSessionCache: tls.NewLRUClientSessionCache(64),

			// for test only
			InsecureSkipVerify: sc.insecureSkipVerify,
		}

		idleTimeout := time.Duration(sc.DoT.IdleTimeout) * time.Second
		dialTLS := func() (net.Conn, error) {
			c, err := net.DialTimeout("tcp", sc.Addr, dialTCPTimeout)
			if err != nil {
				return nil, err
			}
			tlsConn := tls.Client(c, tlsConf)
			tlsConn.SetDeadline(time.Now().Add(tlsHandshakeTimeout))
			// try handshake first
			if err := tlsConn.Handshake(); err != nil {
				c.Close()
				return nil, err
			}
			return tlsConn, nil
		}
		client = &upstreamCommon{
			addr:        sc.Addr,
			dialNewConn: dialTLS,
			readMsg:     readMsgFromTCP,
			writeMsg:    writeMsgToTCP,
			cp:          newConnPool(0xffff, idleTimeout, idleTimeout>>1),
		}
	case "doh":
		tlsConf := &tls.Config{
			// don't have to set servername here, fasthttp will do it itself.
			RootCAs:            rootCAs,
			ClientSessionCache: tls.NewLRUClientSessionCache(64),

			// for test only
			InsecureSkipVerify: sc.insecureSkipVerify,
		}

		if len(sc.DoH.URL) == 0 {
			return nil, fmt.Errorf("protocol [%s] needs URL", sc.Protocol)
		}
		client, err = newDoHUpstream(sc.DoH.URL, sc.Addr, tlsConf, sc.DoH.FastHTTP)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupport protocol: %s", sc.Protocol)
	}

	return client, nil
}

func (u *upstreamCommon) Exchange(ctx context.Context, qRaw []byte, entry *logrus.Entry) (rRaw *bufpool.MsgBuf, rtt time.Duration, err error) {
	t := time.Now()
	rRaw, err = u.exchange(ctx, qRaw, entry, false)
	return rRaw, time.Since(t), err
}

func (u *upstreamCommon) exchange(ctx context.Context, qRaw []byte, entry *logrus.Entry, forceNewConn bool) (rRaw *bufpool.MsgBuf, err error) {
	if err = ctx.Err(); err != nil {
		return nil, err
	}

	if len(qRaw) < 12 {
		return nil, dns.ErrShortRead
	}

	var isNewConn bool
	var dc *dnsConn
	if !forceNewConn { // we want a new connection
		dc = u.cp.get()
	}

	// if we need a new conn
	if dc == nil {
		c, err := u.dialNewConn()
		if err != nil {
			return nil, err
		}

		dc = newDNSConn(c)

		isNewConn = true
		// dialNewConn might take some time, check if ctx is done
		if err = ctx.Err(); err != nil {
			u.cp.put(dc)
			return nil, err
		}
	} else {
		dc.msgID++
		dc.SetDeadline(time.Time{}) // overwrite ddl
	}

	queryCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// this once is to make sure that the following
	// dc.Conn.SetDeadline wouldn't be called after dc is put into connPool
	once := sync.Once{}
	go func() {
		select {
		case <-queryCtx.Done():
			once.Do(func() { dc.SetDeadline(time.Now()) })
		}
	}()

	// write first
	qRawCopy := bufpool.AcquireMsgBufAndCopy(qRaw)
	defer bufpool.ReleaseMsgBuf(qRawCopy)
	originalID := utils.ExchangeMsgID(dc.msgID, qRawCopy.B)
	n, err := u.writeMsg(dc.Conn, qRawCopy.B)
	if n > 0 {
		dc.lastIO = time.Now()
	}
	if err != nil {
		goto ioErr
	}

	// if we need to empty the conn (some data of previous reply)
	if dc.frameleft > 0 {
		buf := bufpool.AcquireMsgBuf(dc.frameleft)
		n, err := io.ReadFull(dc, buf.B)
		bufpool.ReleaseMsgBuf(buf)
		if n > 0 {
			dc.lastIO = time.Now()
			dc.frameleft = dc.frameleft - n
		}
		if err != nil {
			goto ioErr
		}
	}

read:
	rRaw, dc.frameleft, n, err = u.readMsg(dc.Conn)
	if n > 0 {
		dc.lastIO = time.Now()
	}
	if err != nil {
		goto ioErr
	}

	if utils.GetMsgID(rRaw.B) != dc.msgID {
		bufpool.ReleaseMsgBuf(rRaw)
		if !isNewConn {
			// this connection is reused, data might be the reply
			// of a previous qRaw, not this qRaw.
			// try to read again
			goto read
		} else {
			// new connection should not receive a mismatched id, this is an error
			dc.Close()
			return nil, dns.ErrId
		}
	}

	once.Do(func() {}) // do nothing, just fire the once
	u.cp.put(dc)

	utils.SetMsgID(originalID, rRaw.B)
	return rRaw, nil

ioErr:
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() && queryCtx.Err() != nil && dc.frameleft != unknownBrokenDataSize {
		entry.Debugf("exchange: io timeout Err after ctx was done: %v", err)
		// err caused by cancelled ctx, it's ok to reuse the connection
		once.Do(func() {}) // do nothing, just fire the once
		u.cp.put(dc)
		return nil, err
	}
	dc.Close()
	if isNewConn { // new connection shouldn't have any err
		entry.Warnf("exchange: new conn fatal ioErr: %v", err)
		return nil, err
	}

	// reused connection got an unexpected err, open a new conn and try again
	entry.Warnf("exchange: reused conn ioErr: %v", err)
	return u.exchange(ctx, qRaw, entry, true)
}

type connPool struct {
	sync.Mutex
	maxSize          int
	ttl              time.Duration
	cleannerInterval time.Duration

	pool      []*dnsConn
	lastClean time.Time
}

type dnsConn struct {
	net.Conn
	frameleft int
	msgID     uint16
	lastIO    time.Time
}

func newDNSConn(c net.Conn) *dnsConn {
	return &dnsConn{
		Conn:      c,
		frameleft: 0,
		msgID:     dns.Id(),
		lastIO:    time.Now(),
	}
}

func newConnPool(size int, ttl, gcInterval time.Duration) *connPool {
	return &connPool{
		maxSize:          size,
		ttl:              ttl,
		cleannerInterval: gcInterval,
		pool:             make([]*dnsConn, 0),
	}

}

// runCleanner must run under lock
func (p *connPool) runCleanner(force bool) {
	if p == nil && len(p.pool) == 0 {
		return
	}

	//scheduled for forced
	if force || time.Since(p.lastClean) > p.cleannerInterval {
		p.lastClean = time.Now()
		res := p.pool[:0]
		for i := range p.pool {
			// remove expired conns
			if time.Since(p.pool[i].lastIO) < p.ttl {
				res = append(res, p.pool[i])
			} else { // expired, release the resources
				p.pool[i].Conn.Close()
				p.pool[i] = nil
			}
		}
		p.pool = res
	}

	//when the pool is full
	if len(p.pool) >= p.maxSize {
		res := p.pool[:0]
		mid := len(p.pool) >> 1
		for i := range p.pool {
			// forcely remove half conns first
			if i < mid {
				p.pool[i].Conn.Close()
				p.pool[i] = nil
			}

			//then remove expired conns
			if time.Since(p.pool[i].lastIO) < p.ttl {
				res = append(res, p.pool[i])
			} else {
				p.pool[i].Conn.Close()
				p.pool[i] = nil
			}
		}
		p.pool = res
	}
}

func (p *connPool) put(dc *dnsConn) {
	if dc == nil || dc.Conn == nil {
		return
	}

	if p == nil || p.maxSize <= 0 || p.ttl <= 0 || dc.frameleft == unknownBrokenDataSize {
		dc.Conn.Close()
		return
	}

	p.Lock()
	defer p.Unlock()

	p.runCleanner(false)

	if len(p.pool) >= p.maxSize {
		dc.Conn.Close() // pool is full, drop it
	} else {
		p.pool = append(p.pool, dc)
	}
}

func (p *connPool) get() (dc *dnsConn) {
	if p == nil {
		return nil
	}
	if p.maxSize <= 0 || p.ttl <= 0 {
		return nil
	}

	p.Lock()
	defer p.Unlock()

	p.runCleanner(false)

	if len(p.pool) > 0 {
		dc := p.pool[len(p.pool)-1]
		p.pool[len(p.pool)-1] = nil
		p.pool = p.pool[:len(p.pool)-1]

		if time.Since(dc.lastIO) > p.ttl {
			dc.Conn.Close() // expired
			// the last elem is expired, means all elems are expired
			// remove them asap
			p.runCleanner(true)
			return nil
		}
		return dc
	}
	return nil
}

const (
	fastHTTPIOTimeout = time.Second * 3
)

type upstreamDoH struct {
	useFastHTTP bool
	preparedURL []byte

	fasthttpClient *fasthttp.HostClient
	netHTTPClient  *http.Client
}

func newDoHUpstream(urlStr, addr string, tlsConfig *tls.Config, fastHTTP bool) (*upstreamDoH, error) {
	// check urlStr
	u, err := url.ParseRequestURI(urlStr)
	if err != nil {
		return nil, fmt.Errorf("url.ParseRequestURI: %w", err)
	}

	if u.Scheme != "https" {
		return nil, fmt.Errorf("invalid url scheme [%s]", u.Scheme)
	}

	u.ForceQuery = true // make sure we have a '?' at somewhere
	urlStr = u.String()
	if strings.HasSuffix(urlStr, "?") {
		urlStr = urlStr + "dns=" // the only one and the first arg
	} else {
		urlStr = urlStr + "&dns=" // the last arg
	}

	c := new(upstreamDoH)
	c.preparedURL = []byte(urlStr)
	c.useFastHTTP = fastHTTP

	if fastHTTP {
		c.fasthttpClient = &fasthttp.HostClient{
			Addr: u.Hostname(),
			Dial: func(_ string) (net.Conn, error) {
				d := net.Dialer{Timeout: fastHTTPIOTimeout}
				return d.Dial("tcp", addr)
			},
			IsTLS:                         true,
			TLSConfig:                     tlsConfig,
			ReadTimeout:                   fastHTTPIOTimeout,
			WriteTimeout:                  fastHTTPIOTimeout,
			MaxResponseBodySize:           dns.MaxMsgSize,
			DisableHeaderNamesNormalizing: true,
			DisablePathNormalizing:        true,
			NoDefaultUserAgentHeader:      true,
		}
	} else {
		tc := new(tls.Config)
		if tlsConfig != nil {
			tc = tlsConfig.Clone()
		}
		transport := &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				d := net.Dialer{}
				return d.DialContext(ctx, network, addr)
			},
			TLSClientConfig: tc,

			IdleConnTimeout:       time.Minute,
			ResponseHeaderTimeout: time.Second * 5,
			ForceAttemptHTTP2:     true,
		}

		err := http2.ConfigureTransport(transport) // enable http2
		if err != nil {
			return nil, err
		}
		c.netHTTPClient = &http.Client{
			Transport: transport,
		}
	}

	return c, nil
}

// some consistent string vars for DoH client
var (
	headerCanonicalKeyAccept = []byte("Accept")
	headerValueMediaType     = []byte("application/dns-message")
	dohCommomHeader          = http.Header{"Accept": []string{"application/dns-message"}}
)

func (u *upstreamDoH) Exchange(ctx context.Context, qRaw []byte, requestLogger *logrus.Entry) (rRaw *bufpool.MsgBuf, rtt time.Duration, err error) {
	t := time.Now()
	r, err := u.exchange(ctx, qRaw, requestLogger)
	return r, time.Since(t), err
}

func (u *upstreamDoH) exchange(ctx context.Context, qRaw []byte, requestLogger *logrus.Entry) (rRaw *bufpool.MsgBuf, err error) {
	if len(qRaw) < 12 {
		return nil, dns.ErrShortRead // avoid panic when access msg id in m[0] and m[1]
	}

	qRawCopy := bufpool.AcquireMsgBuf(len(qRaw))
	defer bufpool.ReleaseMsgBuf(qRawCopy)
	copy(qRawCopy.B, qRaw)

	// In order to maximize HTTP cache friendliness, DoH clients using media
	// formats that include the ID field from the DNS message header, such
	// as "application/dns-message", SHOULD use a DNS ID of 0 in every DNS
	// request.
	// https://tools.ietf.org/html/rfc8484 4.1
	oldID := utils.ExchangeMsgID(0, qRawCopy.B)

	// Padding characters for base64url MUST NOT be included.
	// See: https://tools.ietf.org/html/rfc8484 6
	// That's why we use base64.RawURLEncoding
	urlLen := len(u.preparedURL) + base64.RawURLEncoding.EncodedLen(len(qRawCopy.B))
	urlBytes := bufpool.AcquireMsgBuf(urlLen)
	copy(urlBytes.B, u.preparedURL)
	base64MsgStart := len(u.preparedURL)
	base64.RawURLEncoding.Encode(urlBytes.B[base64MsgStart:], qRawCopy.B)

	if u.useFastHTTP {
		rRaw, err = u.doFasthttp(urlBytes.B, requestLogger)
		if err != nil {
			return nil, fmt.Errorf("doFasthttp: %w", err)
		}
	} else {
		rRaw, err = u.doHTTP(ctx, string(urlBytes.B), requestLogger)
		if err != nil {
			return nil, fmt.Errorf("doHTTP: %w", err)
		}
	}

	// change the id back
	if utils.GetMsgID(rRaw.B) != 0 { // check msg id
		bufpool.ReleaseMsgBuf(rRaw)
		return nil, dns.ErrId
	}
	utils.SetMsgID(oldID, rRaw.B)
	return rRaw, nil
}

func (u *upstreamDoH) doFasthttp(url []byte, requestLogger *logrus.Entry) (*bufpool.MsgBuf, error) {
	//Note: It is forbidden copying Request instances. Create new instances and use CopyTo instead.
	//Request instance MUST NOT be used from concurrently running goroutines.
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.SetRequestURIBytes(url)
	req.Header.SetMethod(fasthttp.MethodGet)
	req.Header.SetCanonical(headerCanonicalKeyAccept, headerValueMediaType)
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)

	if err := u.fasthttpClient.Do(req, resp); err != nil {
		return nil, fmt.Errorf("Do: %w", err)
	}

	// check Content_Length
	if resp.Header.ContentLength() > dns.MaxMsgSize {
		return nil, fmt.Errorf("ContentLength is too big [%d]", resp.Header.ContentLength())
	}

	// check statu code
	statusCode := resp.StatusCode()
	if statusCode != fasthttp.StatusOK {
		return nil, fmt.Errorf("HTTP status codes [%d]", statusCode)
	}

	body := resp.Body()
	if len(body) < 12 {
		return nil, dns.ErrShortRead
	}

	rRaw := bufpool.AcquireMsgBufAndCopy(body)
	return rRaw, nil
}

func (u *upstreamDoH) doHTTP(ctx context.Context, url string, requestLogger *logrus.Entry) (*bufpool.MsgBuf, error) {

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("NewRequestWithContext: %w", err)
	}
	req.Header = dohCommomHeader.Clone()

	resp, err := u.netHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Do: %w", err)
	}
	defer resp.Body.Close()

	// check Content-Length
	if resp.ContentLength > dns.MaxMsgSize {
		return nil, fmt.Errorf("ContentLength is too big [%d]", resp.ContentLength)
	}

	// check statu code
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP status codes [%d]", resp.StatusCode)
	}

	buf := bufpool.AcquireBytesBuf()
	defer bufpool.ReleaseBytesBuf(buf)
	_, err = buf.ReadFrom(io.LimitReader(resp.Body, dns.MaxMsgSize))

	if err != nil {
		return nil, fmt.Errorf("Response body is too large: %w", err)
	}
	body := buf.Bytes()

	if len(body) < 12 {
		return nil, dns.ErrShortRead
	}

	rRaw := bufpool.AcquireMsgBufAndCopy(body)
	return rRaw, nil
}
