package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	ss "github.com/shadowsocks/shadowsocks-go/shadowsocks"
)

type ChannelDefine struct {
	Name string `toml:"name"`
	Type string `toml:"type"`
	Addr string `toml:"addr"`
}

type EConn struct {
	net.Conn
}

func (c *EConn) Read(b []byte) (n int, err error) {
	n, err = c.Conn.Read(b)
	if err != nil {
		return
	}
	for i := 0; i < n; i++ {
		b[i] = ^b[i]
	}
	return
}

func (c *EConn) Write(b []byte) (n int, err error) {
	b1 := make([]byte, len(b))
	for i, c := range b {
		b1[i] = ^c
	}
	return c.Conn.Write(b1)
}

type EReader struct {
	r io.Reader
}

func (r *EReader) Read(p []byte) (n int, err error) {
	n, err = r.r.Read(p)
	if err != nil {
		return
	}
	for i := 0; i < n; i++ {
		p[i] = ^p[i]
	}
	return
}

var sspos = 0
var ssaddrs []string
var sslock sync.RWMutex
var Config struct {
	Listen  string `toml:"listen"`
	Channel []struct {
		Domains []string `toml:"domains"`
		ChannelDefine
	} `toml:"channel"`
	Socks5  ChannelDefine `toml:"socks5"`
	Default ChannelDefine `toml:"default"`
}
var channelCache = make(map[string]*ChannelDefine)
var channelLock sync.RWMutex

func main() {
	Config.Listen = "127.0.0.1:18080"
	_, err := toml.DecodeFile("goproxy.conf", &Config)
	if err != nil {
		log.Fatalln("DecodeFile failed:", err)
	}
	for i, channel := range Config.Channel {
		for j, d := range channel.Domains {
			if !strings.HasPrefix(d, ".") {
				Config.Channel[i].Domains[j] = "." + d
			}
		}
	}

	l, err := net.Listen("tcp", Config.Listen)
	if err != nil {
		log.Fatalln("Listen failed:", err)
	}
	for {
		c, err := l.Accept()
		if err != nil {
			log.Println("Accept failed:", err)
			continue
		}
		go doProxy(c)
	}
}

func getConnectByChannel(channel ChannelDefine, domain string, port uint16) (net.Conn, bool, error) {
	log.Println(channel.Name, ":", domain)
	if strings.HasPrefix(channel.Type, "ss,") {
		parts := strings.SplitN(channel.Type, ",", 3)
		if len(parts) != 3 {
			log.Println("Config shadowsocks failed:", channel.Type)
			return nil, false, errors.New("config_error")
		}
		c, err := connectShadowSocks(parts[1], parts[2], channel.Addr, domain, port)
		return c, false, err
	} else if channel.Type == "socks5" {
		c, err := connectSocks5(channel.Addr, domain, port)
		return c, false, err
	} else if channel.Type == "http" {
		c, err := connectHttpProxy(channel.Addr, domain, port)
		return c, false, err
	} else {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%v", domain, port), time.Second*5)
		return c, true, err
	}
}

func connectShadowSocks(method, password, ssaddr string, domain string, port uint16) (net.Conn, error) {
	cipher, err := ss.NewCipher(method, password)
	if err != nil {
		log.Println("ss.NewCipher failed:", err)
		return nil, err
	}
	var once sync.Once
	once.Do(func() {
		ssaddrs = strings.Split(ssaddr, ",")
	})

	sslock.RLock()
	addr := ssaddrs[sspos]
	sslock.RUnlock()

	rawaddr := make([]byte, 0, 512)
	rawaddr = append(rawaddr, []byte{3, byte(len(domain))}...)
	rawaddr = append(rawaddr, []byte(domain)...)
	rawaddr = append(rawaddr, byte(port>>8))
	rawaddr = append(rawaddr, byte(port&0xff))
	c, err := ss.DialWithRawAddr(rawaddr, addr, cipher.Copy())
	if err != nil {
		sslock.Lock()
		sspos++
		sspos = sspos % len(ssaddrs)
		sslock.Unlock()
	}
	return c, err
}

func getProxyConnect(domain string, port uint16) (net.Conn, bool, error) {
	channelLock.RLock()
	channel, ok := channelCache[domain]
	channelLock.RUnlock()
	if ok {
		return getConnectByChannel(*channel, domain, port)
	}
	for _, channel := range Config.Channel {
		for _, d := range channel.Domains {
			if d[1:] == domain || strings.HasSuffix(domain, d) {
				channelLock.Lock()
				channelCache[domain] = &channel.ChannelDefine
				channelLock.Unlock()

				return getConnectByChannel(channel.ChannelDefine, domain, port)
			}
		}
	}
	channelLock.Lock()
	channelCache[domain] = &Config.Default
	channelLock.Unlock()
	return getConnectByChannel(Config.Default, domain, port)
}

func connectHttpProxy(http, domain string, port uint16) (net.Conn, error) {
	c2, err := net.Dial("tcp", http)
	if err != nil {
		log.Println("Conn.Dial failed:", err, http)
		return nil, err
	}
	c2.SetDeadline(time.Now().Add(10 * time.Second))
	c2.Write([]byte(fmt.Sprintf("CONNECT %v:%v HTTP/1.1\r\nHost: %v:%v\r\n\r\n", domain, port)))
	buff := make([]byte, 17, 256)
	c2.Read(buff)
	for !bytes.HasSuffix(buff, []byte("\r\n\r\n")) {
		ch := make([]byte, 1)
		_, err = c2.Read(ch)
		if err != nil {
			log.Println("Conn.Read failed:", err, http)
			return nil, err
		}
		buff = append(buff, ch[0])
		if len(buff) > 255 {
			log.Println("HTTP Proxy Connect failed: return too long")
			return nil, errors.New("http_proxy_failed")
		}
	}
	if buff[9] != '2' {
		log.Println("HTTP Proxy Connect failed:", string(buff))
		return nil, errors.New("http_proxy_failed")
	}
	c2.SetDeadline(time.Time{})
	return c2, nil
}

func connectSocks5(socks5, domain string, port uint16) (net.Conn, error) {
	c2, err := net.Dial("tcp", socks5)
	if err != nil {
		log.Println("Conn.Dial failed:", err, socks5)
		return nil, err
	}
	c2.SetDeadline(time.Now().Add(10 * time.Second))
	c2.Write([]byte{5, 2, 0, 0x81})
	resp := make([]byte, 2)
	n, err := c2.Read(resp)
	if err != nil {
		log.Println("Conn.Read failed:", err)
		return nil, err
	}
	if n != 2 {
		log.Println("socks5 response error:", resp)
		return nil, errors.New("socks5_error")
	}
	method := resp[1]
	if method != 0 && method != 0x81 {
		log.Println("socks5 not support 'NO AUTHENTICATION REQUIRED'")
		return nil, errors.New("socks5_error")
	}
	send := make([]byte, 0, 512)
	send = append(send, []byte{5, 1, 0, 3, byte(len(domain))}...)
	if method == 0 {
		send = append(send, []byte(domain)...)
	} else {
		edomain := []byte(domain)
		for i, c := range edomain {
			edomain[i] = ^c
		}
		send = append(send, edomain...)
	}
	send = append(send, byte(port>>8))
	send = append(send, byte(port&0xff))
	_, err = c2.Write(send)
	if err != nil {
		log.Println("Conn.Write failed:", err)
		return nil, err
	}
	n, err = c2.Read(send[0:10])
	if err != nil {
		log.Println("Conn.Read failed:", err)
		return nil, err
	}
	if send[1] != 0 {
		switch send[1] {
		case 1:
			log.Println("socks5 general SOCKS server failure")
		case 2:
			log.Println("socks5 connection not allowed by ruleset")
		case 3:
			log.Println("socks5 Network unreachable")
		case 4:
			log.Println("socks5 Host unreachable")
		case 5:
			log.Println("socks5 Connection refused")
		case 6:
			log.Println("socks5 TTL expired")
		case 7:
			log.Println("socks5 Command not supported")
		case 8:
			log.Println("socks5 Address type not supported")
		default:
			log.Println("socks5 Unknown eerror:", send[1])
		}
		return nil, errors.New("socks5_error")
	}
	c2.SetDeadline(time.Time{})
	if method == 0 {
		return c2, nil
	} else {
		return &EConn{c2}, nil
	}
}

func doProxy(c net.Conn) {
	defer c.Close()

	var cr io.Reader = bufio.NewReader(c)
	buff, err := cr.(*bufio.Reader).Peek(3)
	if err != nil {
		log.Println("Conn.Read failed:", err)
		return
	}
	var peer net.Conn
	var direct bool
	if bytes.Equal(buff, []byte("CON")) {
		buff, err := cr.(*bufio.Reader).ReadSlice(' ')
		if err != nil {
			log.Println("Conn.Read failed:", err)
			return
		}
		if !bytes.Equal(buff, []byte("CONNECT ")) {
			log.Println("Protocol error:", string(buff))
			return
		}
		buff, err = cr.(*bufio.Reader).ReadSlice(':')
		if err != nil {
			log.Println("Conn.Read failed:", err)
			return
		}
		if len(buff) <= 1 {
			log.Println("CONNECT protocol error: host not found")
			return
		}
		domain := string(buff[:len(buff)-1])
		buff, err = cr.(*bufio.Reader).ReadSlice(' ')
		if err != nil {
			log.Println("Conn.Read failed:", err)
			return
		}
		if len(buff) <= 1 {
			log.Println("CONNECT protocol error: port not found")
			return
		}
		_port := string(buff[:len(buff)-1])
		port, err := strconv.Atoi(_port)
		if err != nil {
			log.Println("CONNECT protocol error: port format error:", err, _port)
			return
		}
		for {
			if buff, _, err = cr.(*bufio.Reader).ReadLine(); err != nil {
				log.Println("Conn.Read failed:", err)
				return
			} else if len(buff) == 0 {
				break
			}
		}
		peer, direct, err = getProxyConnect(domain, uint16(port))
		if err != nil {
			log.Println("connect failed:", err)
			return
		}
		defer peer.Close()
		c.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	} else if buff[0] >= 'A' && buff[0] <= 'Z' {
		buff, err := cr.(*bufio.Reader).ReadBytes('\n')
		n := len(buff)
		p1 := bytes.Index(buff[:n], []byte("http://"))
		if p1 == -1 {
			log.Println("http proxy format error, host not found")
			return
		}
		p2 := bytes.Index(buff[p1+7:n], []byte("/"))
		if p2 == -1 {
			log.Println("http proxy format error, host not finish")
			return
		}
		url := string(buff[p1+7 : p1+7+p2])
		buff = append(buff[:p1], buff[p1+7+p2:]...)
		n -= (7 + p2)
		p3 := strings.IndexByte(url, ':')
		port := 80
		_port := "80"
		domain := url
		if p3 == -1 {
			url += ":80"
		} else {
			domain = url[:p3]
			_port = string(url[p3+1:])
			port, err = strconv.Atoi(_port)
			if err != nil {
				log.Println("http port format error:", _port)
				return
			}
		}
		peer, direct, err = getProxyConnect(domain, uint16(port))
		if err != nil {
			log.Println("connect socks5 failed:", err)
			return
		}
		defer peer.Close()
		_, err = peer.Write(buff[:n])
		if err != nil {
			log.Println("Conn.Write failed:", err)
			return
		}
	} else if buff[0] == 5 {
		temp := make([]byte, 355)
		_, err := io.ReadAtLeast(cr, temp, 2+int(buff[2]))
		if err != nil {
			log.Println("Conn.Read failed:", err)
			return
		}
		var method byte = 0xff
		for _, ch := range temp[2 : buff[1]+2] {
			if ch == 0 {
				if method == 0xff {
					method = 0
				}
			} else if ch == 0x81 {
				method = 0x81
				break
			}
		}
		if method == 0xff {
			log.Println("Socks5 NO ACCEPTABLE METHODS:", temp[:buff[1]+2])
			return
		}
		_, err = c.Write([]byte{5, method})
		if err != nil {
			log.Println("Conn.Write failed:", err)
			return
		}
		buff, err = cr.(*bufio.Reader).Peek(5)
		if err != nil {
			log.Println("Conn.Read failed:", err)
			return
		}
		if buff[1] != 1 {
			log.Println("Socks5 not support cmd:", buff[1])
			return
		}
		var domain string
		var port uint16
		var n int
		switch buff[3] {
		case 1, 4:
			iplen := net.IPv4len
			if buff[3] == 4 {
				iplen = net.IPv6len
			}
			n, err = io.ReadAtLeast(cr, temp, 6+iplen)
			if err != nil {
				log.Println("Conn.Read failed:", err)
				return
			}
			end := 4 + iplen
			domain = net.IP(temp[4:end]).String()
			port = uint16(temp[end])<<8 + uint16(temp[end+1])
		case 3:
			n, err = io.ReadAtLeast(cr, temp, 6+int(temp[4])+1)
			if err != nil {
				log.Println("Conn.Read failed:", err)
				return
			}
			end := temp[4] + 5
			_domain := temp[5:end]
			if method == 0x81 {
				for i, c := range _domain {
					_domain[i] = ^c
				}
			}
			domain = string(_domain)
			port = uint16(temp[end])<<8 + uint16(temp[end+1])
		}
		if method == 0 {
			peer, direct, err = getConnectByChannel(Config.Socks5, domain, uint16(port))
		} else {
			peer, direct, err = getProxyConnect(domain, uint16(port))
		}
		if err != nil {
			log.Println("connect socks5 failed:", err)
			temp[1] = 1
			_, err = c.Write(temp[:n])
			if err != nil {
				log.Println("Conn.Write failed:", err)
			}
			return
		}
		_, err = c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
		if err != nil {
			log.Println("Conn.Write failed:", err)
			return
		}
		if method == 0x81 {
			c = &EConn{c}
			cr = &EReader{cr}
		}
		defer peer.Close()

	} else {
		log.Println("unknown protocol:", string(buff))
		return
	}
	_ = direct

	go func() {
		defer peer.Close()
		defer c.Close()
		io.Copy(c, peer)
	}()
	io.Copy(peer, cr)
}
